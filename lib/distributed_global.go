package lib

import (
	"context"
	"errors"
	"github.com/sirupsen/logrus"
	"net/http"
	"strconv"
	"sync"
	"time"
)

type ClusterGlobalRateLimiter struct {
	sync.RWMutex
	globalBucketsMap map[uint64]*globalWindow
}

func NewClusterGlobalRateLimiter() *ClusterGlobalRateLimiter {
	return &ClusterGlobalRateLimiter{
		globalBucketsMap: make(map[uint64]*globalWindow),
	}
}

// Take spends one request of the bot's global limit, waiting for the next
// window while there is none to spend, or none the reserve allows.
func (c *ClusterGlobalRateLimiter) Take(botHash uint64, botLimit uint, reserve int) {
	bucket := c.getOrCreate(botHash, botLimit)
	held := false
	for {
		ok, wait := bucket.take(reserve, time.Now())
		if ok {
			return
		}
		if reserve != noReserve && !held {
			held = true
			ReserveHeld.WithLabelValues("global").Inc()
		}
		logger.WithFields(logrus.Fields{
			"waitTime": wait,
		}).Trace("Failed to grab global token, sleeping for a bit")
		time.Sleep(wait)
	}
}

func (c *ClusterGlobalRateLimiter) getOrCreate(botHash uint64, botLimit uint) *globalWindow {
	c.RLock()
	b, ok := c.globalBucketsMap[botHash]
	c.RUnlock()
	if ok {
		return b
	}
	c.Lock()
	defer c.Unlock()
	// Check if it wasn't created while we didnt hold the exclusive lock
	if b, ok = c.globalBucketsMap[botHash]; ok {
		return b
	}
	b = newGlobalWindow(botLimit, 1*time.Second)
	c.globalBucketsMap[botHash] = b
	return b
}

func (c *ClusterGlobalRateLimiter) FireGlobalRequest(ctx context.Context, addr string, botHash uint64, botLimit uint, reserve int) error {
	globalReq, err := http.NewRequestWithContext(ctx, "GET", "http://"+addr+"/nirn/global", nil)
	if err != nil {
		return err
	}

	globalReq.Header.Set("bot-hash", strconv.FormatUint(botHash, 10))
	globalReq.Header.Set("bot-limit", strconv.FormatUint(uint64(botLimit), 10))
	if reserve != noReserve {
		// The node that owns the bot's global limit applies the reserve.
		globalReq.Header.Set(reserveHeader, strconv.Itoa(reserve))
	}

	// The node handling the request will only return if we grabbed a token or an error was thrown
	resp, err := client.Do(globalReq)
	logger.Trace("Got go-ahead for global")

	if err != nil {
		return err
	}

	if resp.StatusCode != 200 {
		return errors.New("global request failed with status " + resp.Status)
	}

	return nil
}
