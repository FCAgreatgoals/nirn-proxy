package lib

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// By default the global limit is the documented 50 per second, and working it
// out costs no request: /gateway/bot, which the detection would call, is
// limited to 2 requests per 5 seconds and the bot's shards need them.
func TestGlobalLimitDefaultsToTheDocumentedValue(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("the global limit cost a request to %s", r.URL.Path)
		w.WriteHeader(500)
	}))
	defer srv.Close()
	pointAt(t, srv)

	user := &BotUserResponse{Id: "203039963636301824"}
	if limit, err := GetBotGlobalLimit("Bot x", user); err != nil || limit != 50 {
		t.Errorf("default limit = %d (%v), want 50", limit, err)
	}

	// A bot whose limit Discord raised says so explicitly.
	globalOverrideMap[user.Id] = 120
	defer delete(globalOverrideMap, user.Id)
	if limit, _ := GetBotGlobalLimit("Bot x", user); limit != 120 {
		t.Errorf("overridden limit = %d, want 120", limit)
	}
}
