package resource

import (
	"container/list"
	"net/http"
	"strings"
	"sync"

	"github.com/Shoal-Technology/shoal/api/v1alpha1"
)

// Redelivered webhooks (issue #13). A git host may send the same push more
// than once. A push whose commit already has a live Build of its config is
// answered as a duplicate and starts nothing. When the host sends a delivery
// id, a redelivery is recognised before the store is read.

// deliveryIDHeaders carry a unique id per delivery, by host. A generic sender
// has none, so its redeliveries are caught by the commit check alone.
var deliveryIDHeaders = []string{"X-GitHub-Delivery", "X-Gitea-Delivery", "X-Gitlab-Event-UUID"}

// maxDeliveryIDLen bounds the delivery id a request may carry. A longer one is
// ignored, so it cannot fill the cache with large keys.
const maxDeliveryIDLen = 128

// WebhookDeliveryID returns the delivery id a request carries, or "" if none.
func WebhookDeliveryID(h http.Header) string {
	for _, k := range deliveryIDHeaders {
		if v := strings.TrimSpace(h.Get(k)); v != "" && len(v) <= maxDeliveryIDLen {
			return v
		}
	}
	return ""
}

// FindDuplicateBuild returns the name of a Build of config that already covers
// commit, and true if there is one. A Build covers the commit when it was
// started from it and is New, Pending or Running, or when it started from it or
// resolved to it and is Complete. Failed, Error and Cancelled Builds do not
// count, so a redelivery after a failed run starts a new one.
//
// builds are the Builds in the store. The controller keeps only the last
// MaxBuildHistory finished Builds per config, so the search covers the
// retained history.
func FindDuplicateBuild(config, commit string, builds []*v1alpha1.Build) (string, bool) {
	for _, b := range builds {
		if b.GetSpec().GetConfig() != config {
			continue
		}
		fromCommit := b.GetSpec().GetSource().GetGit().GetRef() == commit
		switch b.GetStatus().GetPhase() {
		case BuildNew, BuildPending, BuildRunning:
			if fromCommit {
				return b.GetMetadata().GetName(), true
			}
		case BuildComplete:
			if fromCommit || b.GetStatus().GetCommit() == commit {
				return b.GetMetadata().GetName(), true
			}
		}
	}
	return "", false
}

// DeliveryCache is a bounded LRU of recent webhook deliveries. It maps a key
// (the config's uid and the delivery id) to the Build the delivery started.
// When full, the least recently used entry is dropped. It is safe for
// concurrent use.
type DeliveryCache struct {
	mu    sync.Mutex
	max   int
	order *list.List // front is the most recently used
	items map[string]*list.Element
}

type deliveryEntry struct {
	key   string
	build string
}

// NewDeliveryCache returns a cache that holds up to max deliveries. A max below
// one is raised to one.
func NewDeliveryCache(max int) *DeliveryCache {
	if max < 1 {
		max = 1
	}
	return &DeliveryCache{max: max, order: list.New(), items: make(map[string]*list.Element)}
}

// Get returns the Build that a delivery started, and marks the delivery as
// recently used.
func (c *DeliveryCache) Get(key string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[key]
	if !ok {
		return "", false
	}
	c.order.MoveToFront(el)
	return el.Value.(*deliveryEntry).build, true
}

// Put records the Build that a delivery started, evicting the least recently
// used entries when the cache is full.
func (c *DeliveryCache) Put(key, build string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		el.Value.(*deliveryEntry).build = build
		c.order.MoveToFront(el)
		return
	}
	c.items[key] = c.order.PushFront(&deliveryEntry{key: key, build: build})
	for c.order.Len() > c.max {
		last := c.order.Back()
		c.order.Remove(last)
		delete(c.items, last.Value.(*deliveryEntry).key)
	}
}
