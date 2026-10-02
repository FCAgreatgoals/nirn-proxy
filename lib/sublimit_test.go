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
	reset := 2400 * time.Millisecond
	cases := []struct {
		retryAfter time.Duration
		scope      string
		hold       time.Duration
	}{
		{3 * time.Second, "user", 0},                   // Retry-After rounds the reset up: an ordinary 429
		{540 * time.Second, "user", 540 * time.Second}, // far beyond the bucket's reset: a sublimit
		{899 * time.Second, "shared", 899 * time.Second},
		{time.Second, "shared", 0},       // an ordinary shared 429
		{540 * time.Second, "global", 0}, // the global lock has its own handling
		{0, "user", 0},
	}
	for _, c := range cases {
		if got := sublimitHold(c.retryAfter, c.scope, reset); got != c.hold {
			t.Errorf("wait %v scope %s: hold %v, want %v", c.retryAfter, c.scope, got, c.hold)
		}
	}
}

// A shared 429 says Retry-After: 1 whatever the wait, which only its body
// carries, as Discord wrote every one bucketmap captured.
func TestRefusalWaitReadsTheBody(t *testing.T) {
	resp := &http.Response{Header: http.Header{}, Body: io.NopCloser(strings.NewReader(
		`{"message": "Max number of prune requests has been reached. Try again later", "retry_after": 899.41, "global": false, "code": 30040}`))}
	resp.Header.Set("Retry-After", "1")
	if got := refusalWait(resp); got != 899410*time.Millisecond {
		t.Errorf("wait %v, want the body's 899.41s", got)
	}
	if rest, _ := io.ReadAll(resp.Body); !strings.Contains(string(rest), "30040") {
		t.Error("the body was not left readable")
	}

	plain := &http.Response{Header: http.Header{}, Body: io.NopCloser(strings.NewReader("not json"))}
	plain.Header.Set("Retry-After", "3")
	if got := refusalWait(plain); got != 3*time.Second {
		t.Errorf("wait %v, want the header's 3s when the body has none", got)
	}
}

// A route refused in the shared scope waits what the body asks for, though
// Retry-After says 1.
func TestSharedRefusalHoldsForTheBodysWait(t *testing.T) {
	const wait = 2500 * time.Millisecond
	first := true
	fake := &fakeDiscord{answer: func(_ *http.Request, _ []byte) (int, http.Header, string) {
		h := http.Header{}
		h.Set("X-RateLimit-Limit", "1000")
		h.Set("X-RateLimit-Remaining", "999")
		h.Set("X-RateLimit-Reset-After", "0.001")
		if first {
			first = false
			h.Set("Retry-After", "1")
			h.Set("X-RateLimit-Scope", "shared")
			return 429, h, `{"message": "The resource is being rate limited.", "retry_after": 2.5, "global": false}`
		}
		return 200, h, "{}"
	}}
	q := newTestQueue(fake)
	path := "/api/v10/guilds/111/prune"
	q.send(t, "POST", path, `{"days":30}`)
	q.send(t, "POST", path, `{"days":30}`)
	if gap := fake.call(1).at.Sub(fake.call(0).at); gap < wait-100*time.Millisecond {
		t.Errorf("second request reached Discord %v after the refusal, which asked for %v", gap, wait)
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
