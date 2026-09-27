package lib

import (
	"net/http"
	"strings"

	"github.com/FCAgreatgoals/bucketmap/routes"
)

// Discord counts some routes together, in one bucket: a channel's edit with
// its permission overwrites and the guild's channel order, a webhook token's
// every route, a command's global and guild edits, and more. Runs of bucketmap
// against Discord (github.com/FCAgreatgoals/bucketmap) found eighteen such
// families, which its route index carries.
//
// Upstream keys a queue by its own reading of the path, so two routes of one
// family land in two queues and send into the same bucket side by side: an
// anti-raid lockdown that rewrites a channel's permissions and then edits it
// is exactly that. Routes of one family share a queue per value of their
// major parameter, the counter Discord keeps.

// familyPath returns the queue a request belongs to when its route belongs to
// a family, or path unchanged.
func familyPath(req *http.Request, path string) string {
	// Reactions are already one queue per channel, and renames are set apart
	// on purpose, behind their sublimit.
	if strings.Contains(path, "/reactions/") || strings.HasSuffix(path, renameSuffix) {
		return path
	}
	route, ok := routes.Match(req.Method, req.URL.Path)
	if !ok || route.Family == "" {
		return path
	}
	return "/!family/" + route.Family + "/" + route.MajorValue(req.URL.Path)
}
