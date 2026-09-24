package lib

import (
	"net/http"
	"testing"
	"time"
)

func TestCountsAsInvalid(t *testing.T) {
	cases := []struct {
		status  int
		scope   string
		invalid bool
	}{
		{401, "user", true},
		{403, "user", true},
		{429, "user", true},
		{429, "global", true},
		// Discord: "429 errors returned with X-RateLimit-Scope: shared are not
		// counted against you."
		{429, "shared", false},
		{404, "user", false},
		{200, "user", false},
	}
	for _, c := range cases {
		if got := countsAsInvalid(c.status, c.scope); got != c.invalid {
			t.Errorf("countsAsInvalid(%d, %q) = %v, want %v", c.status, c.scope, got, c.invalid)
		}
	}
}

// The window slides by the minute: what happened ten minutes ago no longer
// counts, what happened nine minutes ago still does.
func TestInvalidRequestWindowSlides(t *testing.T) {
	var tr invalidRequestTracker
	start := time.Unix(1_800_000_000, 0).Truncate(time.Minute)

	for range 3 {
		tr.record(start)
	}
	if got := tr.record(start.Add(9 * time.Minute)); got != 4 {
		t.Errorf("nine minutes later = %d, want 4", got)
	}
	if got := tr.record(start.Add(10 * time.Minute)); got != 2 {
		t.Errorf("ten minutes later = %d, want 2: the first minute has left the window", got)
	}
}

// Each thousand past the warning threshold is reported once, and again only
// after the count has come back down.
func TestInvalidRequestWarningsDoNotRepeat(t *testing.T) {
	var tr invalidRequestTracker
	if tr.shouldWarn(invalidRequestWarnFrom - 1) {
		t.Error("warned below the threshold")
	}
	if !tr.shouldWarn(invalidRequestWarnFrom) {
		t.Error("did not warn on reaching the threshold")
	}
	if tr.shouldWarn(invalidRequestWarnFrom + 10) {
		t.Error("warned twice for the same thousand")
	}
	if !tr.shouldWarn(invalidRequestWarnFrom + 1000) {
		t.Error("did not warn on the next thousand")
	}
	tr.shouldWarn(100) // the window emptied
	if !tr.shouldWarn(invalidRequestWarnFrom) {
		t.Error("did not warn again after the count came back down")
	}
}

// Responses that reach the client through the queue are counted: a 403 goes
// into the window, a 200 does not.
func TestQueueCountsInvalidResponses(t *testing.T) {
	// The tracker is shared by every queue, including those earlier tests
	// left running, so it is read under its lock rather than reset.
	before := invalidRequests.count(time.Now())
	status := 403
	fake := &fakeDiscord{answer: func(*http.Request, []byte) (int, http.Header, string) {
		return status, http.Header{}, `{"message":"Missing Permissions","code":50013}`
	}}
	q := newTestQueue(fake)

	q.send(t, "GET", "/api/v10/guilds/203039963636301824/bans", "")
	status = 200
	q.send(t, "GET", "/api/v10/guilds/203039963636301824/bans", "")

	if got := invalidRequests.count(time.Now()) - before; got != 1 {
		t.Errorf("the queue counted %d invalid requests, want 1", got)
	}
}
