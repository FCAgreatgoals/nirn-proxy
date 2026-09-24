package lib

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// One channel running out of budget does not hold back another: channel_id is
// a major parameter, so each channel has its own counter. Upstream queued every
// /channels/{id} request together, so locking channels during a raid waited on
// whichever one had just emptied its bucket.
func TestExhaustedChannelDoesNotHoldBackAnother(t *testing.T) {
	const resetAfter = time.Second
	fake := &fakeDiscord{answer: func(req *http.Request, _ []byte) (int, http.Header, string) {
		h := http.Header{}
		h.Set("X-RateLimit-Limit", "5")
		h.Set("X-RateLimit-Reset-After", "1.000")
		if strings.HasSuffix(req.URL.Path, "/203039963636301824") {
			h.Set("X-RateLimit-Remaining", "0") // this channel is out of budget
		} else {
			h.Set("X-RateLimit-Remaining", "4")
		}
		return 200, h, "{}"
	}}
	q := newTestQueue(fake)

	q.send(t, "PATCH", "/api/v10/channels/203039963636301824", `{"rate_limit_per_user":30}`)
	start := time.Now()
	q.send(t, "PATCH", "/api/v10/channels/203039963636301825", `{"rate_limit_per_user":30}`)
	if waited := time.Since(start); waited > resetAfter/2 {
		t.Errorf("second channel waited %v for the first channel's bucket", waited)
	}
}

// The same holds for guilds.
func TestExhaustedGuildDoesNotHoldBackAnother(t *testing.T) {
	const resetAfter = time.Second
	fake := &fakeDiscord{answer: func(req *http.Request, _ []byte) (int, http.Header, string) {
		h := http.Header{}
		h.Set("X-RateLimit-Limit", "10")
		h.Set("X-RateLimit-Reset-After", "1.000")
		if strings.Contains(req.URL.Path, "/203039963636301824/") {
			h.Set("X-RateLimit-Remaining", "0")
		} else {
			h.Set("X-RateLimit-Remaining", "9")
		}
		return 200, h, "[]"
	}}
	q := newTestQueue(fake)

	q.send(t, "GET", "/api/v10/guilds/203039963636301824/channels", "")
	start := time.Now()
	q.send(t, "GET", "/api/v10/guilds/203039963636301825/channels", "")
	if waited := time.Since(start); waited > resetAfter/2 {
		t.Errorf("second guild waited %v for the first guild's bucket", waited)
	}
}
