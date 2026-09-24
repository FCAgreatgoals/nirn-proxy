package lib

import (
	"net/http/httptest"
	"testing"
	"time"
)

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
