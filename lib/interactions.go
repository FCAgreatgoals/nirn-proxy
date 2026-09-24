package lib

import "strings"

// interactionTokenPrefix is "interaction:" in base64. Discord interaction tokens
// are the base64 encoding of "interaction:<id>:<secret>".
const interactionTokenPrefix = "aW50ZXJhY3Rpb246"

// trimAPIPrefix strips the query string and the /api or /api/vN prefix, leaving
// the route itself.
func trimAPIPrefix(path string) string {
	path = strings.SplitN(path, "?", 2)[0]
	if !strings.HasPrefix(path, "/api/") {
		return path
	}
	path = strings.TrimPrefix(path, "/api")
	head, tail, found := strings.Cut(strings.TrimPrefix(path, "/"), "/")
	if found && len(head) > 1 && head[0] == 'v' && isDigits(head[1:]) {
		return "/" + tail
	}
	return path
}

func isDigits(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return s != ""
}

// IsInteractionEndpoint reports whether a request targets one of the
// interaction endpoints: the initial response, and the webhook routes an
// interaction token opens (original response and followups).
//
// Discord documents them as not bound to the global rate limit: "Interaction
// endpoints are not bound to the bot's Global Rate Limit"
// (/developers/topics/rate-limits#global-rate-limit). That matters most when the
// global limit is exhausted, which is precisely when a bot under load still has
// to answer the commands its users are sending.
func IsInteractionEndpoint(path string) bool {
	parts := strings.Split(strings.Trim(trimAPIPrefix(path), "/"), "/")
	switch {
	case len(parts) == 4 && parts[0] == MajorInteractions && parts[3] == "callback":
		return true
	case len(parts) >= 3 && parts[0] == MajorWebhooks && strings.HasPrefix(parts[2], interactionTokenPrefix):
		return true
	}
	return false
}
