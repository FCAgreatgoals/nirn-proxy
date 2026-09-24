package lib

import (
	"encoding/json"
	"io"
	"net/http"
)

// codeUnknownWebhook is the JSON error code of a webhook that no longer exists.
const codeUnknownWebhook = 10015

// isUnknownWebhook reports whether a 404 means the webhook itself is gone.
//
// Discord asks not to use a webhook again once it returns a 404, and nirn
// honours that by answering every later request on the route itself. Upstream
// did so on any 404 under /webhooks/, including a deleted webhook message
// (10008 Unknown Message): editing or fetching one message that had been
// removed then made every later request on the route fail with a fake Unknown
// Webhook until the queue was swept. Only 10015 means the webhook is gone.
func isUnknownWebhook(resp *http.Response) bool {
	if resp == nil || resp.Body == nil {
		return false
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return false
	}
	var payload struct {
		Code int `json:"code"`
	}
	return json.Unmarshal(body, &payload) == nil && payload.Code == codeUnknownWebhook
}
