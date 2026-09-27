package lib

import (
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
// they share a queue, per channel; a rename keeps its own queue.
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
