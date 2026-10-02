package lib

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

// Discord runs some buckets as token buckets and others as fixed windows, and
// X-RateLimit-Reset-After means the same thing in both: the time until the
// bucket is full again. On a token bucket it therefore grows with each request
// instead of shrinking. Five chained command edits answered, on real Discord:
//
//	remaining 4, 3, 2, 1, 0 with reset-after 4.000, 7.821, 11.624, 15.421, 19.246
//
// Sleeping reset-after once nothing remains, as the queue does, can only ever
// wait too long, never send into the limit. This replays that sequence at a
// tenth of its scale and checks the sixth request waits for the refill.
func TestTokenBucketIsNeverSentIntoTheLimit(t *testing.T) {
	resets := []string{"0.400", "0.782", "1.162", "1.542", "1.925"}
	fake := &fakeDiscord{}
	fake.answer = func(*http.Request, []byte) (int, http.Header, string) {
		n := fake.count() - 1 // this call is already recorded
		h := http.Header{}
		h.Set("X-RateLimit-Limit", "5")
		if n < len(resets) {
			h.Set("X-RateLimit-Remaining", strconv.Itoa(4-n))
			h.Set("X-RateLimit-Reset-After", resets[n])
		} else {
			h.Set("X-RateLimit-Remaining", "4")
			h.Set("X-RateLimit-Reset-After", "0.400")
		}
		return 200, h, "{}"
	}
	q := newTestQueue(fake)

	const route = "/api/v10/applications/203039963636301824/commands/203039963636301825"
	for range 6 {
		q.send(t, "PATCH", route, `{"description":"x"}`)
	}

	full, _ := strconv.ParseFloat(resets[4], 64)
	if gap := fake.call(5).at.Sub(fake.call(4).at); gap < time.Duration(full*float64(time.Second))-100*time.Millisecond {
		t.Errorf("sixth request sent %v after the bucket emptied, Discord announced %ss until it refills", gap, resets[4])
	}
}

// A 429 the proxy makes up itself looks like Discord's: the documented body,
// and the headers a client paces itself on.
func TestGenerated429LooksLikeDiscords(t *testing.T) {
	rec := httptest.NewRecorder()
	var w http.ResponseWriter = rec
	Generate429(&w)

	if rec.Code != 429 {
		t.Fatalf("status = %d", rec.Code)
	}
	for _, name := range []string{"Retry-After", "X-RateLimit-Reset-After", "X-RateLimit-Remaining"} {
		if rec.Header().Get(name) == "" {
			t.Errorf("%s missing", name)
		}
	}
	var body struct {
		Message    string   `json:"message"`
		RetryAfter *float64 `json:"retry_after"`
		Global     *bool    `json:"global"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.RetryAfter == nil || body.Global == nil || body.Message == "" {
		t.Errorf("body = %s", rec.Body.String())
	}
}
