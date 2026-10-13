package lib

import (
	"context"
	"encoding/json"
	"errors"
	lru "github.com/hashicorp/golang-lru"
	"github.com/hashicorp/memberlist"
	"github.com/sirupsen/logrus"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type QueueType int64

const (
	Bot QueueType = iota
	NoAuth
	Bearer
)

// Some routes that have @me on the path don't really spread out through the cluster, causing issues
// and exacerbating tail latency hits from Discord. Only routes with no ratelimit headers should be put here
var pathsToRouteLocally = map[uint64]struct{}{
	HashCRC64("/users/@me/channels"): {},
	HashCRC64("/users/@me"):          {},
}

type QueueManager struct {
	sync.RWMutex
	queues                   map[string]*RequestQueue
	bearerQueues             *lru.Cache
	bearerMu                 sync.RWMutex
	bufferSize               int
	cluster                  *memberlist.Memberlist
	clusterGlobalRateLimiter *ClusterGlobalRateLimiter
	orderedClusterMembers    []string
	nameToAddressMap         map[string]string
	localNodeName            string
	localNodeIP              string
	localNodeProxyListenAddr string
	aside                    asidePeers
}

func onEvictLruItem(key interface{}, value interface{}) {
	go value.(*RequestQueue).destroy()
}

func NewQueueManager(bufferSize int, maxBearerLruSize int) *QueueManager {
	bearerMap, err := lru.NewWithEvict(maxBearerLruSize, onEvictLruItem)

	if err != nil {
		panic(err)
	}

	q := &QueueManager{
		queues:                   make(map[string]*RequestQueue),
		bearerQueues:             bearerMap,
		bufferSize:               bufferSize,
		cluster:                  nil,
		clusterGlobalRateLimiter: NewClusterGlobalRateLimiter(),
	}

	return q
}

func (m *QueueManager) Shutdown() {
	if m.cluster != nil {
		m.cluster.Leave(30 * time.Second)
	}
}

func (m *QueueManager) reindexMembers() {
	if m.cluster == nil {
		logger.Warn("reindexMembers called but cluster is nil")
		return
	}

	m.Lock()
	defer m.Unlock()
	m.bearerMu.Lock()
	defer m.bearerMu.Unlock()

	members := m.cluster.Members()
	var orderedMembers []string
	nameToAddressMap := make(map[string]string)
	for _, m := range members {
		orderedMembers = append(orderedMembers, m.Name)
		nameToAddressMap[m.Name] = nodeProxyAddr(m)
	}
	sort.Strings(orderedMembers)

	m.orderedClusterMembers = orderedMembers
	m.nameToAddressMap = nameToAddressMap
}

func (m *QueueManager) onNodeJoin(node *memberlist.Node) {
	m.aside.clear(nodeProxyAddr(node))
	// Running in goroutine prevents a deadlock inside memberlist
	go m.reindexMembers()
}
func (m *QueueManager) onNodeLeave(node *memberlist.Node) {
	// Running in goroutine prevents a deadlock inside memberlist
	go m.reindexMembers()
}

func (m *QueueManager) GetEventDelegate() *NirnEvents {
	return &NirnEvents{
		OnJoin:  m.onNodeJoin,
		OnLeave: m.onNodeLeave,
	}
}

func (m *QueueManager) SetCluster(cluster *memberlist.Memberlist, proxyPort string) {
	m.cluster = cluster
	m.localNodeName = cluster.LocalNode().Name
	m.localNodeIP = cluster.LocalNode().Addr.String()
	m.localNodeProxyListenAddr = m.localNodeIP + ":" + proxyPort
	m.reindexMembers()
}

func (m *QueueManager) calculateRoute(pathHash uint64) string {
	if m.cluster == nil {
		// Route to self, proxy in stand-alone mode
		return ""
	}

	if pathHash == 0 {
		return ""
	}

	if _, ok := pathsToRouteLocally[pathHash]; ok {
		return ""
	}

	m.RLock()
	defer m.RUnlock()
	m.bearerMu.RLock()
	defer m.bearerMu.RUnlock()

	members := m.aside.filter(m.orderedClusterMembers, m.nameToAddressMap, time.Now())

	if len(members) == 0 {
		return ""
	}

	addr := m.nameToAddressMap[hrwOwner(pathHash, members)]
	if addr == m.localNodeProxyListenAddr {
		return ""
	}
	return addr
}

func (m *QueueManager) routeRequest(addr string, req *http.Request) (*http.Response, error) {
	// The encoded path, for the same reason as in ProcessRequest: the node
	// that owns the bucket must receive the path the client sent.
	nodeReq, err := http.NewRequestWithContext(req.Context(), req.Method, "http://"+addr+req.URL.EscapedPath()+"?"+req.URL.RawQuery, req.Body)
	if err != nil {
		return nil, err
	}
	nodeReq.Header = req.Header.Clone()
	nodeReq.Header.Set("nirn-routed-to", addr)

	logger.WithFields(logrus.Fields{
		"to":     addr,
		"path":   req.URL.Path,
		"method": req.Method,
	}).Trace("Routing request to node in cluster")
	resp, err := peerClient.Do(nodeReq)
	logger.WithFields(logrus.Fields{
		"to":     addr,
		"path":   req.URL.Path,
		"method": req.Method,
	}).Trace("Received response from node")
	if err == nil {
		RequestsRoutedSent.Inc()
	} else {
		RequestsRoutedError.Inc()
	}

	return resp, err
}

func Generate429(resp *http.ResponseWriter) {
	writeProxy429(resp, time.Second, "You are being rate limited.")
}

// writeProxy429 answers with a 429 of the proxy's own, shaped like Discord's,
// that asks the client to wait wait.
func writeProxy429(resp *http.ResponseWriter, wait time.Duration, message string) {
	writer := *resp
	seconds := wait.Seconds()
	writer.Header().Set("generated-by-proxy", "true")
	writer.Header().Set("x-ratelimit-scope", "user")
	writer.Header().Set("x-ratelimit-limit", "1")
	writer.Header().Set("x-ratelimit-remaining", "0")
	writer.Header().Set("x-ratelimit-reset", strconv.FormatFloat(float64(time.Now().Add(wait).UnixMilli())/1000, 'f', 3, 64))
	// Upstream wrote "x-ratelimit-after", a header Discord never sends, so a
	// client pacing itself on X-RateLimit-Reset-After found nothing here.
	writer.Header().Set("x-ratelimit-reset-after", strconv.FormatFloat(seconds, 'f', 3, 64))
	// Retry-After is in whole seconds, rounded up so that it never asks for
	// less than the body does.
	writer.Header().Set("retry-after", strconv.FormatInt(int64(math.Ceil(seconds)), 10))
	writer.Header().Set("content-type", "application/json")
	writer.WriteHeader(429)
	body, _ := json.Marshal(map[string]any{"global": false, "message": message, "retry_after": math.Round(seconds*1000) / 1000})
	writer.Write(body)
}

// bindGlobal gives a queue the way to spend its bot's global limit, which it
// does right before each request goes to Discord: spent when the request
// arrived, the limit let the requests that waited in their bucket's queue
// reach Discord together, far more of them in a second than it allows.
func (m *QueueManager) bindGlobal(q *RequestQueue) {
	if q.identifier == "NoAuth" {
		return
	}
	var botHash uint64 = 0
	if q.user != nil {
		botHash = HashCRC64(q.user.Id)
	}
	spend := func(ctx context.Context, reserve int) error {
		globalRouteTo := ""
		if q.queueType != Bearer {
			globalRouteTo = m.calculateRoute(botHash)
		}
		for globalRouteTo != "" {
			err := m.clusterGlobalRateLimiter.FireGlobalRequest(ctx, globalRouteTo, botHash, q.botLimit, reserve)
			if err == nil {
				return nil
			}
			if !isDialError(err) {
				ErrorCounter.Inc()
				return err
			}
			m.setAside(globalRouteTo, logger.WithField("function", "FireGlobalRequest"))
			globalRouteTo = m.calculateRoute(botHash)
		}
		m.clusterGlobalRateLimiter.Take(botHash, q.botLimit, reserve)
		return nil
	}
	if q.user == nil {
		q.takeGlobal = spend
		return
	}
	// Identifying the bot cost a request the limit never saw: the first
	// request out spends it too.
	var identified sync.Once
	q.takeGlobal = func(ctx context.Context, reserve int) error {
		var err error
		identified.Do(func() { err = spend(ctx, noReserve) })
		if err != nil {
			return err
		}
		return spend(ctx, reserve)
	}
}

func (m *QueueManager) getOrCreateBotQueue(token string) (*RequestQueue, error) {
	m.RLock()
	q, ok := m.queues[token]
	m.RUnlock()

	if !ok {
		m.Lock()
		defer m.Unlock()
		// Check if it wasn't created while we didn't hold the lock
		q, ok = m.queues[token]
		if !ok {
			var err error
			q, err = NewRequestQueue(ProcessRequest, token, m.bufferSize)

			if err != nil {
				return nil, err
			}
			m.bindGlobal(q)

			m.queues[token] = q
		}
	}

	return q, nil
}

func (m *QueueManager) getOrCreateBearerQueue(token string) (*RequestQueue, error) {
	m.bearerMu.RLock()
	q, ok := m.bearerQueues.Get(token)
	m.bearerMu.RUnlock()

	if !ok {
		m.bearerMu.Lock()
		defer m.bearerMu.Unlock()
		// Check if it wasn't created while we didn't hold the lock
		q, ok = m.bearerQueues.Get(token)
		if !ok {
			var err error
			q, err = NewRequestQueue(ProcessRequest, token, 5)

			if err != nil {
				return nil, err
			}
			m.bindGlobal(q.(*RequestQueue))

			m.bearerQueues.Add(token, q)
		}
	}

	return q.(*RequestQueue), nil
}

func (m *QueueManager) DiscordRequestHandler(resp http.ResponseWriter, req *http.Request) {
	reqStart := time.Now()
	metricsPath := GetMetricsPath(req.URL.Path)
	ConnectionsOpen.With(map[string]string{"route": metricsPath, "method": req.Method}).Inc()
	defer ConnectionsOpen.With(map[string]string{"route": metricsPath, "method": req.Method}).Dec()

	token := req.Header.Get("Authorization")
	routingHash, path, queueType := m.GetRequestRoutingInfo(req, token)

	m.fulfillRequest(&resp, req, queueType, path, routingHash, token, reqStart)
}

func (m *QueueManager) GetRequestRoutingInfo(req *http.Request, token string) (routingHash uint64, path string, queueType QueueType) {
	path = GetOptimisticBucketPath(req.URL.Path, req.Method)
	// Before the routing hash: a request set apart by a sublimit lands in its
	// own queue, on whichever node owns it.
	path = sublimitPath(req, path)
	path = interactionQueuePath(req, path)
	path = bucketQueuePath(req, path)
	queueType = NoAuth
	routingHash = HashCRC64(path)

	switch {
	case HasAuthPrefix(token, "Bearer"):
		queueType = Bearer
		routingHash = HashCRC64(token)
	case token != "" && !HasAuthPrefix(token, "Basic"):
		queueType = Bot
	}
	return
}

func (m *QueueManager) fulfillRequest(resp *http.ResponseWriter, req *http.Request, queueType QueueType, path string, pathHash uint64, token string, reqStart time.Time) {
	logEntry := logger.WithField("clientIp", req.RemoteAddr)
	forwdFor := req.Header.Get("X-Forwarded-For")
	if forwdFor != "" {
		logEntry = logEntry.WithField("forwardedFor", forwdFor)
	}
	// Refused here, before the request is relayed or queued; the queue reads
	// it again when the request goes out.
	_, err := parseReserve(req.Header)
	if err == nil {
		_, err = parseMaxWait(req.Header)
	}
	if err != nil {
		(*resp).Header().Set("content-type", "application/json")
		(*resp).WriteHeader(400)
		(*resp).Write([]byte(`{"message": "` + err.Error() + `", "code": 0}`))
		return
	}
	routeTo := m.calculateRoute(pathHash)

	routeToHeader := req.Header.Get("nirn-routed-to")
	req.Header.Del("nirn-routed-to")

	if routeToHeader != "" {
		RequestsRoutedRecv.Inc()
	}

	if routeTo != "" && routeToHeader == "" && m.relay(resp, req, pathHash, routeTo, logEntry) {
		return
	}

	var q *RequestQueue
	if queueType == Bearer {
		q, err = m.getOrCreateBearerQueue(token)
	} else {
		q, err = m.getOrCreateBotQueue(token)
	}

	if err != nil {
		if strings.HasPrefix(err.Error(), "429") {
			Generate429(resp)
			logEntry.WithFields(logrus.Fields{"function": "getOrCreateQueue", "queueType": queueType}).Warn(err)
		} else {
			(*resp).WriteHeader(500)
			(*resp).Write([]byte(err.Error()))
			ErrorCounter.Inc()
			logEntry.WithFields(logrus.Fields{"function": "getOrCreateQueue", "queueType": queueType}).Error(err)
		}
		return
	}

	err = q.Queue(req, resp, path, pathHash)
	if err != nil {
		log := logEntry.WithField("function", "Queue")
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			log.WithField("waitedFor", time.Since(reqStart)).Warn(err)
		} else {
			log.Error(err)
		}
	}
}

// relay sends the request to the node that owns its bucket and copies the
// answer back. A peer that refuses the connection is set aside and the next
// owner tried; relay returns false once that owner is this node, for the
// request to be served here.
func (m *QueueManager) relay(resp *http.ResponseWriter, req *http.Request, pathHash uint64, routeTo string, logEntry *logrus.Entry) bool {
	body := &relayBody{ReadCloser: req.Body}
	if req.Body != nil && req.Body != http.NoBody {
		req.Body = body
	}
	for routeTo != "" {
		res, err := m.routeRequest(routeTo, req)
		if err == nil {
			err = CopyResponseToResponseWriter(res, resp)
			if err != nil {
				logEntry.WithField("function", "CopyResponseToResponseWriter").Error(err)
			}
			return true
		}
		if isDialError(err) && !body.started {
			m.setAside(routeTo, logEntry)
			routeTo = m.calculateRoute(pathHash)
			continue
		}
		logEntry = logEntry.WithField("function", "routeRequest")
		if !errors.Is(err, context.Canceled) {
			logEntry.Error(err)
		} else {
			logEntry.Warn(err)
		}
		// if it's a context canceled on the client it won't get the 429 anyway, if it's within the cluster we should retry
		Generate429(resp)
		return true
	}
	if body.ReadCloser != nil && req.Body == body {
		req.Body = body.ReadCloser
	}
	return false
}

func (m *QueueManager) HandleGlobal(w http.ResponseWriter, r *http.Request) {
	botHashStr := r.Header.Get("bot-hash")
	botLimitStr := r.Header.Get("bot-limit")

	botHash, err := strconv.ParseUint(botHashStr, 10, 64)
	if err != nil {
		w.WriteHeader(400)
		return
	}

	botLimit, err := strconv.ParseUint(botLimitStr, 10, 64)
	if err != nil {
		w.WriteHeader(400)
		return
	}

	reserve, err := parseReserve(r.Header)
	if err != nil {
		w.WriteHeader(400)
		return
	}

	m.clusterGlobalRateLimiter.Take(botHash, uint(botLimit), reserve)
	logger.Trace("Returned OK for global request")
}

func (m *QueueManager) CreateMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/", m.DiscordRequestHandler)
	mux.HandleFunc("/nirn/global", m.HandleGlobal)
	mux.HandleFunc("/nirn/healthz", func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(200)
	})
	return mux
}
