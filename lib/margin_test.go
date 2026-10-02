package lib

import (
	"net/http"
	"testing"
	"time"
)

// An emptied bucket is waited out past its announced reset, by the margin
// Discord's own reopening needs: sent exactly at the reset, a one request
// bucket was refused in a bucketmap run against Discord.
func TestEmptiedBucketWaitsPastItsReset(t *testing.T) {
	fake := &fakeDiscord{}
	fake.answer = func(*http.Request, []byte) (int, http.Header, string) {
		h := http.Header{}
		h.Set("X-RateLimit-Limit", "1")
		h.Set("X-RateLimit-Remaining", "0")
		h.Set("X-RateLimit-Reset-After", "0.250")
		return 204, h, ""
	}
	q := newTestQueue(fake)

	const route = "/api/v10/guilds/203039963636301824/scheduled-events/203039963636301825"
	for range 2 {
		q.send(t, "GET", route, "")
	}
	if gap := fake.call(1).at.Sub(fake.call(0).at); gap < 250*time.Millisecond+resetMargin-20*time.Millisecond {
		t.Errorf("second request sent %v after the bucket emptied, want its reset and the margin", gap)
	}
}
