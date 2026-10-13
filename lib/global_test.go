package lib

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// After a global 429, requests on every route wait out Retry-After instead of
// being sent into the limit. Upstream recorded the lock and never read it.
func TestGlobalLockHoldsEveryRoute(t *testing.T) {
	const retryAfter = time.Second
	fake := &fakeDiscord{answer: func(req *http.Request, _ []byte) (int, http.Header, string) {
		if strings.HasSuffix(req.URL.Path, "/first") {
			h := http.Header{}
			h.Set("X-RateLimit-Global", "true")
			h.Set("X-RateLimit-Scope", "global")
			h.Set("Retry-After", "1")
			return 429, h, `{"message":"You are being rate limited.","retry_after":1,"global":true}`
		}
		return 200, http.Header{}, "{}"
	}}
	q := newTestQueue(fake)

	q.send(t, "GET", "/api/v10/channels/203039963636301824/first", "")
	q.send(t, "GET", "/api/v10/guilds/203039963636301825", "")

	if gap := fake.call(1).at.Sub(fake.call(0).at); gap < retryAfter-100*time.Millisecond {
		t.Errorf("a request on another route reached Discord %v after a global 429 asking to wait %v", gap, retryAfter)
	}
}

// A lock only ever grows: two global 429s racing each other keep the later
// deadline, and a lock that has expired can be set again.
func TestGlobalLockOnlyExtends(t *testing.T) {
	var lock int64
	now := time.Now()

	if !lockGlobal(&lock, now.Add(2*time.Second)) {
		t.Fatal("first lock not taken")
	}
	if lockGlobal(&lock, now.Add(time.Second)) {
		t.Error("an earlier deadline shortened the lock")
	}
	if !lockGlobal(&lock, now.Add(3*time.Second)) {
		t.Error("a later deadline did not extend the lock")
	}

	// Upstream set the lock with a compare-and-swap from zero, which only
	// the first global 429 could ever win.
	expired := time.Now().Add(-time.Second).UnixNano()
	if !lockGlobal(&expired, now.Add(time.Second)) {
		t.Error("an expired lock could not be set again")
	}
}
