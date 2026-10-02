package lib

import (
	"net/http"
	"strings"
	"testing"
)

const testWebhook = "/api/v10/webhooks/203039963636301824/some-ordinary-webhook-token-that-is-long-enough-to-look-like-one-000"

// A deleted webhook message is not a deleted webhook: the next request on the
// same route must still reach Discord.
func TestMissingWebhookMessageDoesNotLockTheWebhook(t *testing.T) {
	fake := &fakeDiscord{answer: func(req *http.Request, _ []byte) (int, http.Header, string) {
		if strings.HasSuffix(req.URL.Path, "/203039963636301830") {
			return 404, http.Header{}, `{"message":"Unknown Message","code":10008}`
		}
		return 200, http.Header{}, `{}`
	}}
	q := newTestQueue(fake)

	q.send(t, "GET", testWebhook+"/messages/203039963636301830", "")
	rec := q.send(t, "GET", testWebhook+"/messages/203039963636301831", "")

	if fake.count() != 2 {
		t.Fatalf("Discord saw %d requests, want 2: the route was locked after a missing message", fake.count())
	}
	if rec.Code != 200 {
		t.Errorf("second message = %d, want 200", rec.Code)
	}
}

// An unknown webhook is locked, as Discord asks: later requests are answered
// without being sent.
func TestUnknownWebhookLocksTheRoute(t *testing.T) {
	fake := &fakeDiscord{answer: func(*http.Request, []byte) (int, http.Header, string) {
		return 404, http.Header{}, `{"message":"Unknown Webhook","code":10015}`
	}}
	q := newTestQueue(fake)

	q.send(t, "GET", testWebhook+"/messages/203039963636301830", "")
	rec := q.send(t, "GET", testWebhook+"/messages/203039963636301831", "")

	if fake.count() != 1 {
		t.Errorf("Discord saw %d requests, want 1: an unknown webhook was used again", fake.count())
	}
	if rec.Code != 404 || !strings.Contains(rec.Body.String(), "10015") {
		t.Errorf("locked route answered %d %s", rec.Code, rec.Body.String())
	}
}
