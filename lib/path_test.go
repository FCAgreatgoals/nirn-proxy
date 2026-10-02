package lib

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The keycap emoji #️⃣, encoded as a client sends it. Decoded, its "#" starts
// a URL fragment.
const keycapReaction = "/api/v10/channels/203039963636301824/messages/203039963636301825/reactions/%23%EF%B8%8F%E2%83%A3/@me"

// pointAt sends Discord traffic to a test server for the length of a test.
func pointAt(t *testing.T, srv *httptest.Server) {
	t.Helper()
	previousURL, previousClient, previousTimeout := discordURL, client, contextTimeout
	if err := SetDiscordURL(srv.URL); err != nil {
		t.Fatal(err)
	}
	// Set by ConfigureDiscordHTTPClient in main; zero would expire every
	// request before it is sent.
	client, contextTimeout = srv.Client(), 5*time.Second
	t.Cleanup(func() { discordURL, client, contextTimeout = previousURL, previousClient, previousTimeout })
}

// A reaction with a keycap emoji reaches Discord whole. Upstream rebuilt the
// URL from the decoded path and sent ".../reactions/" instead.
func TestKeycapEmojiReachesDiscordIntact(t *testing.T) {
	var received string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received = r.URL.EscapedPath()
		w.WriteHeader(204)
	}))
	defer srv.Close()
	pointAt(t, srv)

	req := httptest.NewRequest(http.MethodPut, keycapReaction, nil)
	var res http.ResponseWriter = httptest.NewRecorder()
	if _, err := ProcessRequest(context.Background(), &QueueItem{Req: req, Res: &res}); err != nil {
		t.Fatal(err)
	}
	if received != keycapReaction {
		t.Errorf("Discord received %q\nwant %q", received, keycapReaction)
	}
}

// The hop between cluster nodes keeps the encoded path too: the node that
// owns the bucket must see what the client sent.
func TestClusterHopKeepsTheEncodedPath(t *testing.T) {
	var received string
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received = r.URL.EscapedPath()
		w.WriteHeader(204)
	}))
	defer node.Close()
	previous := client
	client = node.Client()
	defer func() { client = previous }()

	m := &QueueManager{}
	req := httptest.NewRequest(http.MethodPut, keycapReaction, nil)
	resp, err := m.routeRequest(strings.TrimPrefix(node.URL, "http://"), req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if received != keycapReaction {
		t.Errorf("node received %q\nwant %q", received, keycapReaction)
	}
}

func TestSetDiscordURL(t *testing.T) {
	previous := discordURL
	defer func() { discordURL = previous }()

	for _, ok := range []string{"https://discord.com", "http://localhost:7060", "http://localhost:7060/"} {
		if err := SetDiscordURL(ok); err != nil {
			t.Errorf("SetDiscordURL(%q) = %v", ok, err)
		}
	}
	if discordURL != "http://localhost:7060" {
		t.Errorf("trailing slash kept: %q", discordURL)
	}
	// A path would be prepended to every request path and silently break
	// them all, so it is refused rather than guessed at.
	for _, bad := range []string{"localhost:7060", "ftp://discord.com", "http://localhost:7060/api/v10", "http://"} {
		if err := SetDiscordURL(bad); err == nil {
			t.Errorf("SetDiscordURL(%q) accepted", bad)
		}
	}
}

// A long query value is not an interaction token. strings.SplitN(url, "?", 1)
// never splits, so upstream inspected the query too, and a request carrying a
// long enough query value was treated as an interaction: exempt from the 401
// lock and from the webhook 404 lock.
func TestQueryIsNotAnInteractionToken(t *testing.T) {
	url := "/api/v10/channels/203039963636301824/messages?around=" + strings.Repeat("9", 200)
	if isInteraction(url) {
		t.Error("a long query value was taken for an interaction token")
	}
	if path := GetOptimisticBucketPath(url, "GET"); strings.Contains(path, "?") {
		t.Errorf("query leaked into the bucket path: %q", path)
	}
}
