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
// sublimit: a wait well beyond the bucket's reset, on a bucket scoped to the
// user. Zero means an ordinary 429, which the bucket's reset covers.
//
// A shared 429 reads the same way: a limit on the resource, not the bot, that
// the headers never announce. A bucketmap run found one on prune, which
// Discord refuses for fifteen minutes after a first one (30040, 899 s to wait)
// while its bucket announces a thousand requests left. Upstream let the next
// prune through into the same refusal.
func sublimitHold(retryAfter time.Duration, scope string, resetAfter time.Duration) time.Duration {
	if scope != "user" && scope != "shared" {
		return 0
	}
	if retryAfter.Seconds() <= resetAfter.Seconds()+sublimitRetryMargin {
		return 0
	}
	return retryAfter
}

// refusalWait is how long a 429 asks to wait: the longer of Retry-After and
// the body's retry_after.
//
// On a shared 429, Discord writes Retry-After: 1 whatever the wait, and only
// the body has it: every shared refusal bucketmap captured, 154 of them, did
// so, 899 s for a prune, 59.7 s for a webhook creation. Reading the header
// alone, the hold above never fired against Discord, and the proxy came back
// a second later into the same refusal. The body stays readable for the
// checks that follow.
func refusalWait(resp *http.Response) time.Duration {
	var wait float64
	if v, err := strconv.ParseFloat(resp.Header.Get("Retry-After"), 64); err == nil {
		wait = v
	}
	if resp.Body != nil {
		raw, err := io.ReadAll(io.LimitReader(resp.Body, maxSublimitPeek))
		resp.Body = io.NopCloser(bytes.NewReader(raw))
		var body struct {
			RetryAfter float64 `json:"retry_after"`
		}
		if err == nil && json.Unmarshal(raw, &body) == nil && body.RetryAfter > wait {
			wait = body.RetryAfter
		}
	}
	return time.Duration(wait * float64(time.Second))
}
