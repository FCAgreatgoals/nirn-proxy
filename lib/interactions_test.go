package lib

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const testInteractionToken = "aW50ZXJhY3Rpb246ODg3NTU5MDA01AY4NTUxNDU0OnZwS3QycDhvREk2aVF3U1BqN2prcXBkRmNqNlp4VEhGRjZvSVlXSGh4WG4yb3l6Z3B6NTBPNVc3OHphV05OULLMOHBMa2RTZmVKd3lzVDA2b2h3OTUxaFJ4QlN0dkxXallPcmhnSHNJb0tSV0M5ZzY1NkN4VGRvemFOSHY4b05c"

const testCallback = "/api/v10/interactions/203039963636301824/" + testInteractionToken + "/callback"

func TestIsInteractionEndpoint(t *testing.T) {
	cases := []struct {
		path     string
		endpoint bool
	}{
		// The initial response, versioned or not, with or without a query.
		{testCallback, true},
		{"/api/interactions/203039963636301824/" + testInteractionToken + "/callback?with_response=true", true},
		// The routes an interaction token opens.
		{"/api/v10/webhooks/203039963636301824/" + testInteractionToken, true},
		{"/api/v10/webhooks/203039963636301824/" + testInteractionToken + "/messages/@original", true},
		{"/api/v10/webhooks/203039963636301824/" + testInteractionToken + "/messages/203039963636301825", true},
		// A real webhook is bound to the global limit like anything else.
		{testWebhook, false},
		{"/api/v10/webhooks/203039963636301824", false},
		{"/api/v10/channels/203039963636301824/messages", false},
		{"/api/v10/interactions/203039963636301824/get-author", false},
	}
	for _, c := range cases {
		if got := IsInteractionEndpoint(c.path); got != c.endpoint {
			t.Errorf("IsInteractionEndpoint(%.70s...) = %v, want %v", c.path, got, c.endpoint)
		}
	}
}

// A global lock holds ordinary requests, never interaction answers.
func TestGlobalLockSparesInteractions(t *testing.T) {
	fake := &fakeDiscord{answer: func(*http.Request, []byte) (int, http.Header, string) { return 204, http.Header{}, "" }}
	q := newTestQueue(fake)

	const lock = time.Second
	*q.globalLockedUntil = time.Now().Add(lock).UnixNano()

	start := time.Now()
	q.send(t, "POST", testCallback, `{"type":4,"data":{"content":"pong"}}`)
	if waited := time.Since(start); waited > lock/2 {
		t.Errorf("interaction answer waited %v on the global lock", waited)
	}

	q.send(t, "GET", "/api/v10/channels/203039963636301824", "")
	if waited := time.Since(start); waited < lock-100*time.Millisecond {
		t.Errorf("ordinary request went through after %v, during the global lock", waited)
	}
}

// Interaction answers neither wait on nor spend the global rate limit. The
// queue is injected so the manager never calls Discord to identify the bot.
func TestGlobalLimitSparesInteractions(t *testing.T) {
	fake := &fakeDiscord{answer: func(*http.Request, []byte) (int, http.Header, string) { return 204, http.Header{}, "" }}
	q := newTestQueue(fake)
	q.user = &BotUserResponse{Id: "203039963636301824"}
	q.botLimit = 1 // one request per second, so a second ordinary one must wait

	m := NewQueueManager(16, 16)
	m.queues["Bot x"] = q

	serve := func(method, path string) time.Duration {
		req := httptest.NewRequest(method, path, nil)
		req.Header.Set("Authorization", "Bot x")
		start := time.Now()
		m.DiscordRequestHandler(httptest.NewRecorder(), req)
		return time.Since(start)
	}

	serve("GET", "/api/v10/channels/203039963636301824") // spends the global budget
	for range 3 {
		if took := serve("POST", testCallback); took > 300*time.Millisecond {
			t.Fatalf("interaction answer took %v, it waited on the global limit", took)
		}
	}
	if took := serve("GET", "/api/v10/channels/203039963636301824"); took < 500*time.Millisecond {
		t.Errorf("ordinary request took %v: the interactions spent the global budget, or it was not enforced", took)
	}
}

// interactionToken builds a token the way Discord shapes them: the base64 of
// "interaction:<id>:<secret>", unpadded.
func interactionToken(id string) string {
	raw := "interaction:" + id + ":" + strings.Repeat("s", 80)
	return strings.TrimRight(base64.StdEncoding.EncodeToString([]byte(raw)), "=")
}

// The followup route and the original response of one interaction share a
// counter, so they share a queue; another interaction is not held back.
func TestInteractionWebhookRoutesShareOneQueue(t *testing.T) {
	const resetAfter = time.Second
	fake := &fakeDiscord{answer: func(req *http.Request, _ []byte) (int, http.Header, string) {
		h := http.Header{}
		h.Set("X-RateLimit-Limit", "5")
		h.Set("X-RateLimit-Reset-After", "1.000")
		if req.Method == "POST" {
			h.Set("X-RateLimit-Remaining", "0") // the followup spends the last request
		} else {
			h.Set("X-RateLimit-Remaining", "3")
		}
		return 200, h, "{}"
	}}
	q := newTestQueue(fake)

	first := "/api/v10/webhooks/203039963636301824/" + interactionToken("203039963636301830")
	other := "/api/v10/webhooks/203039963636301824/" + interactionToken("203039963636301831")

	q.send(t, "POST", first, `{"content":"followup"}`)
	start := time.Now()
	q.send(t, "PATCH", other+"/messages/@original", `{"content":"other"}`)
	if waited := time.Since(start); waited > resetAfter/2 {
		t.Errorf("another interaction waited %v", waited)
	}

	q.send(t, "PATCH", first+"/messages/@original", `{"content":"edit"}`)
	if gap := fake.call(2).at.Sub(fake.call(0).at); gap < resetAfter-100*time.Millisecond {
		t.Errorf("the original response was edited %v after the followup spent the shared counter", gap)
	}
}
