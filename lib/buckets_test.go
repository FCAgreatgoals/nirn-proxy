package lib

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func routeOf(method, path, body string) string {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	_, bucket, _ := (&QueueManager{}).GetRequestRoutingInfo(req, "Bot test")
	return bucket
}

// A channel's edit and its permission overwrites share Discord's bucket, so
// they share a queue, per channel, from the first request; a rename keeps its
// own queue.
func TestRoutesOfOneFamilyShareAQueue(t *testing.T) {
	edit := routeOf("PATCH", "/api/v10/channels/111", `{"rate_limit_per_user":5}`)
	overwrite := routeOf("PUT", "/api/v10/channels/111/permissions/222", `{"type":0}`)
	other := routeOf("PATCH", "/api/v10/channels/333", `{"rate_limit_per_user":5}`)
	rename := routeOf("PATCH", "/api/v10/channels/111", `{"name":"x"}`)
	if edit != overwrite {
		t.Errorf("edit %q and overwrite %q of one channel are queued apart", edit, overwrite)
	}
	if edit == other {
		t.Errorf("two channels share queue %q", edit)
	}
	if rename == edit {
		t.Errorf("a rename lost its own queue: %q", rename)
	}
	if got := routeOf("PUT", "/api/v10/channels/111/messages/444/reactions/x/@me", ""); !strings.Contains(got, "reactions") {
		t.Errorf("reactions left their queue: %q", got)
	}
}

// Two routes the index knows no family for, answered with one bucket, share a
// queue from then on, per value of the major parameter: the proxy learns what
// Discord says rather than a list.
func TestRoutesAnsweredWithOneBucketShareAQueue(t *testing.T) {
	a := httptest.NewRequest("GET", "/api/v10/guilds/555/audit-logs", nil)
	b := httptest.NewRequest("GET", "/api/v10/guilds/555/integrations", nil)
	if routeOf(a.Method, a.URL.Path, "") == routeOf(b.Method, b.URL.Path, "") {
		t.Fatal("the two routes already share a queue before any answer")
	}
	header := http.Header{}
	header.Set("X-RateLimit-Bucket", "test-shared-bucket")
	learnBucket(a, header)
	learnBucket(b, header)
	if got, want := routeOf(b.Method, b.URL.Path, ""), routeOf(a.Method, a.URL.Path, ""); got != want {
		t.Errorf("routes answered with one bucket queue apart: %q and %q", got, want)
	}
	if routeOf("GET", "/api/v10/guilds/666/audit-logs", "") == routeOf(a.Method, a.URL.Path, "") {
		t.Error("two guilds share a queue")
	}
}
