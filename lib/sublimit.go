package lib

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Channel renames sit behind a sublimit that Discord neither documents nor
// announces in the headers: a handful per ten minutes per channel, on top of the
// PATCH /channels/{id} bucket they share with every other channel edit. A 429
// on it carries a Retry-After far beyond the bucket's own reset.
//
// @discordjs/rest sets such requests apart (hasSublimit): a channel PATCH only
// waits on the sublimit when its body changes the name or the topic. Upstream
// kept every edit of a channel in one queue and slept only the bucket's short
// reset after a 429, so each following rename went straight back into the
// sublimit, another invalid request each time, while the slowmode edits queued
// behind it waited their turn. For an anti-raid bot that is the worst moment to
// stall: slowmode is one of the first things it reaches for.
//
// Renames therefore get their own queue, and only that queue waits out the
// sublimit.

// renameSuffix sets rename requests apart from other channel edits.
const renameSuffix = "/!rename"

// maxSublimitPeek bounds how much of a channel PATCH body is read to classify
// it. Channel edits are small JSON documents; anything larger is left alone.
const maxSublimitPeek = 1 << 20

// sublimitRetryMargin is how far Retry-After must exceed the bucket's reset
// for a 429 to read as a sublimit. Retry-After is rounded up to whole seconds
// while X-RateLimit-Reset-After keeps milliseconds, so an ordinary 429 differs
// by less than one second.
const sublimitRetryMargin = 1.0

// peekedBody replays the bytes read to classify a request, followed by
// whatever was left unread, and still closes the original body.
type peekedBody struct {
	io.Reader
	io.Closer
}

// renamesChannel reports whether a channel PATCH changes the name or the topic,
// the two fields @discordjs/rest treats as sublimited. The body is restored
// for the request to be forwarded untouched.
func renamesChannel(req *http.Request) bool {
	if req.Body == nil || req.Body == http.NoBody {
		return false
	}

	peeked, err := io.ReadAll(io.LimitReader(req.Body, maxSublimitPeek+1))
	req.Body = peekedBody{io.MultiReader(bytes.NewReader(peeked), req.Body), req.Body}
	if err != nil || len(peeked) > maxSublimitPeek {
		return false
	}

	var fields map[string]json.RawMessage
	if json.Unmarshal(peeked, &fields) != nil {
		return false
	}
	_, name := fields["name"]
	_, topic := fields["topic"]
	return name || topic
}

// sublimitPath returns the queue a request belongs to once sublimits are
// accounted for.
func sublimitPath(req *http.Request, path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	isChannel := len(parts) == 2 && parts[0] == MajorChannels
	if req.Method == http.MethodPatch && isChannel && renamesChannel(req) {
		return path + renameSuffix
	}
	return path
}

// sublimitHold tells how long a queue must hold after a 429 that reads as a
// sublimit: Retry-After well beyond the bucket's reset, on a bucket scoped to
// the user. Zero means an ordinary 429, which the bucket's reset covers.
func sublimitHold(header http.Header, scope string, resetAfter time.Duration) time.Duration {
	if scope != "user" {
		return 0
	}
	retryAfter, err := strconv.ParseFloat(header.Get("Retry-After"), 64)
	if err != nil || retryAfter <= resetAfter.Seconds()+sublimitRetryMargin {
		return 0
	}
	return time.Duration(retryAfter * float64(time.Second))
}
