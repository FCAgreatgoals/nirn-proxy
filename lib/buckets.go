package lib

import (
	"net/http"
	"strings"
	"sync"

	"github.com/FCAgreatgoals/bucketmap/routes"
)

// One bucket and one value of the major parameter make one counter, whatever
// the route: that is how Discord counts, and what runs of bucketmap against it
// showed on every pair of routes answering with one X-RateLimit-Bucket
// (github.com/FCAgreatgoals/bucketmap). A channel's edit with its permission
// overwrites, every route of a webhook token, a command's global and guild
// edits: eighteen such families were found.
//
// Upstream keyed a queue by its own reading of the path and only logged
// X-RateLimit-Bucket, so two routes of one bucket sent into it from two
// queues side by side: an anti-raid lockdown rewriting a channel's
// permissions and then editing it is exactly that.
//
// A queue is now named after the bucket once Discord has answered the route
// with it, per value of the major parameter. Before that answer, the route
// takes the family bucketmap's index gives it, the same on every node of a
// cluster, so that routing stays consistent from the first request; a bucket
// learned later maps onto that family's queue when they are the same.

// bucketKeys remembers, for each route, the bucket Discord answered it with,
// and for each bucket, the queue it runs in.
type bucketKeys struct {
	mu      sync.RWMutex
	ofRoute map[string]string // "METHOD /template" -> X-RateLimit-Bucket
	queueOf map[string]string // X-RateLimit-Bucket -> queue name
}

var learnedBuckets = &bucketKeys{ofRoute: map[string]string{}, queueOf: map[string]string{}}

// bucketQueuePath returns the queue a request belongs to once shared buckets
// are accounted for, or path unchanged.
func bucketQueuePath(req *http.Request, path string) string {
	// Reactions are already one queue per channel, and renames are set apart
	// on purpose, behind their sublimit.
	if strings.Contains(path, "/reactions/") || strings.HasSuffix(path, renameSuffix) {
		return path
	}
	route, ok := routes.Match(req.Method, req.URL.Path)
	if !ok {
		return path
	}
	major := route.MajorValue(req.URL.Path)
	learnedBuckets.mu.RLock()
	name := learnedBuckets.queueOf[learnedBuckets.ofRoute[req.Method+" "+route.Path]]
	learnedBuckets.mu.RUnlock()
	switch {
	case name != "":
	case route.Family != "":
		name = "family/" + route.Family
	default:
		return path
	}
	return "/!bucket/" + name + "/" + major
}

// learnBucket records the bucket Discord answered a route with.
func learnBucket(req *http.Request, header http.Header) {
	bucket := header.Get("X-RateLimit-Bucket")
	if bucket == "" {
		return
	}
	route, ok := routes.Match(req.Method, req.URL.Path)
	if !ok {
		return
	}
	learnedBuckets.mu.Lock()
	defer learnedBuckets.mu.Unlock()
	learnedBuckets.ofRoute[req.Method+" "+route.Path] = bucket
	if learnedBuckets.queueOf[bucket] == "" {
		// A bucket first met on a route of a known family runs in that
		// family's queue, so that requests already queued under the family
		// and those that follow meet in one place.
		if route.Family != "" {
			learnedBuckets.queueOf[bucket] = "family/" + route.Family
		} else {
			learnedBuckets.queueOf[bucket] = bucket
		}
	}
}
