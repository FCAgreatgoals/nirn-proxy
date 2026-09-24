package lib

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

const testChannel = "/api/v10/channels/203039963636301824"

func patchRequest(path, body string) *http.Request {
	req := httptest.NewRequest(http.MethodPatch, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return req
}

// A rename and a slowmode edit on the same channel land in different queues,
// and classifying a request leaves its body intact for Discord.
func TestRenamesGetTheirOwnQueue(t *testing.T) {
	m := &QueueManager{}
	cases := []struct {
		body   string
		rename bool
	}{
		{`{"name":"raid-lock"}`, true},
		{`{"topic":"under attack"}`, true},
		{`{"name":"x","rate_limit_per_user":30}`, true},
		{`{"rate_limit_per_user":30}`, false},
		{`{"permission_overwrites":[]}`, false},
		{`not json`, false},
	}
	for _, c := range cases {
		req := patchRequest(testChannel, c.body)
		_, path, _ := m.GetRequestRoutingInfo(req, "Bot x")
		if got := strings.HasSuffix(path, renameSuffix); got != c.rename {
			t.Errorf("%s: queue %q, rename = %v, want %v", c.body, path, got, c.rename)
		}
		if body, _ := io.ReadAll(req.Body); string(body) != c.body {
			t.Errorf("%s: body forwarded as %q", c.body, body)
		}
	}

	// Only PATCH on the channel itself is concerned.
	for _, req := range []*http.Request{
		httptest.NewRequest(http.MethodGet, testChannel, nil),
		patchRequest(testChannel+"/messages/203039963636301825", `{"name":"x"}`),
	} {
		if _, path, _ := m.GetRequestRoutingInfo(req, "Bot x"); strings.HasSuffix(path, renameSuffix) {
			t.Errorf("%s %s set apart as a rename", req.Method, req.URL.Path)
		}
	}
}

func TestSublimitHold(t *testing.T) {
	header := func(retryAfter string) http.Header {
		h := http.Header{}
		h.Set("Retry-After", retryAfter)
		return h
	}
	reset := 2400 * time.Millisecond
	cases := []struct {
		retryAfter string
		scope      string
		hold       time.Duration
	}{
		{"3", "user", 0},                   // Retry-After rounds the reset up: an ordinary 429
		{"540", "user", 540 * time.Second}, // far beyond the bucket's reset: a sublimit
		{"540", "shared", 0},               // other scopes have their own handling
		{"540", "global", 0},
		{"", "user", 0},
	}
	for _, c := range cases {
		if got := sublimitHold(header(c.retryAfter), c.scope, reset); got != c.hold {
			t.Errorf("Retry-After %q scope %s: hold %v, want %v", c.retryAfter, c.scope, got, c.hold)
		}
	}
}

// A rename that hits the sublimit holds the renames queued behind it until
// Retry-After, and nothing else: a slowmode edit on the same channel goes
// through at once.
func TestSublimitHoldsOnlyRenames(t *testing.T) {
	// Past the one second margin that tells a sublimit from a rounded
	// Retry-After, and short enough to keep the test quick.
	const retryAfter = 2 * time.Second
	fake := &fakeDiscord{answer: func(_ *http.Request, body []byte) (int, http.Header, string) {
		h := http.Header{}
		h.Set("X-RateLimit-Limit", "5")
		h.Set("X-RateLimit-Remaining", "4")
		h.Set("X-RateLimit-Reset-After", "0.050")
		if strings.Contains(string(body), `"first"`) {
			h.Set("Retry-After", strconv.Itoa(int(retryAfter.Seconds())))
			h.Set("X-RateLimit-Scope", "user")
			return 429, h, `{"message":"You are being rate limited.","retry_after":2,"global":false}`
		}
		return 200, h, "{}"
	}}
	q := newTestQueue(fake)

	q.send(t, "PATCH", testChannel, `{"name":"first"}`)
	start := time.Now()
	q.send(t, "PATCH", testChannel, `{"rate_limit_per_user":30}`)
	if waited := time.Since(start); waited > retryAfter/2 {
		t.Errorf("slowmode edit waited %v behind the rename sublimit", waited)
	}

	q.send(t, "PATCH", testChannel, `{"name":"second"}`)
	if gap := fake.call(2).at.Sub(fake.call(0).at); gap < retryAfter-100*time.Millisecond {
		t.Errorf("second rename reached Discord %v after the first, before the sublimit lifted", gap)
	}
}
