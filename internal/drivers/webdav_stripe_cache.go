package drivers

import (
	"container/list"
	"strings"
	"sync"
	"time"
)

// The manifest cache of striped objects. Without it every read of a striped
// object paid a probe of the plain file (a miss) and a manifest GET — two
// Sync round trips (~0.7 s each) — before its first piece (live 2026-10-07:
// 1 MiB ranges p50 1.55 s against 0.72 s unstriped, single-stream TTFB
// 3.76 s). Entries are filled by this process's striped Put and by every
// manifest read, dropped by this process's Put/Delete of the key, and
// expire after webdavManifestCacheTTL.
//
// Another writer (the other app slot) is not seen until a read finds a
// piece of the cached generation gone: the overwrite or delete deleted it
// (errStripePieceGone) — the entry is dropped, the object is resolved again
// from the bridge (plain file, then manifest) and the new manifest cached.
// Wrong bytes are impossible (a cached manifest names one generation, whose
// whole pieces are sha256-verified); a stale entry serves the previous
// version only while its pieces still exist — an overwrite elsewhere whose
// deletion of the old pieces failed — and for at most the TTL.

const (
	webdavManifestCacheEntries = 4096
	webdavManifestCacheTTL     = 10 * time.Minute
)

type manifestCache struct {
	mu  sync.Mutex
	max int
	ttl time.Duration
	now func() time.Time
	ll  *list.List // front = most recently used
	idx map[string]*list.Element
}

type manifestCacheEntry struct {
	key string
	man *stripeManifest
	exp time.Time
}

func newManifestCache(max int, ttl time.Duration, now func() time.Time) *manifestCache {
	return &manifestCache{max: max, ttl: ttl, now: now, ll: list.New(), idx: map[string]*list.Element{}}
}

// manifestCacheKey is the object's resource names (tenant folder, container,
// key segments, leaf).
func manifestCacheKey(names []string) string { return strings.Join(names, "/") }

func (c *manifestCache) get(key string) (*stripeManifest, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.idx[key]
	if !ok {
		return nil, false
	}
	e := el.Value.(*manifestCacheEntry)
	if !c.now().Before(e.exp) {
		c.ll.Remove(el)
		delete(c.idx, key)
		return nil, false
	}
	c.ll.MoveToFront(el)
	return e.man, true
}

func (c *manifestCache) put(key string, man *stripeManifest) {
	if c == nil || man == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	exp := c.now().Add(c.ttl)
	if el, ok := c.idx[key]; ok {
		e := el.Value.(*manifestCacheEntry)
		e.man, e.exp = man, exp
		c.ll.MoveToFront(el)
		return
	}
	c.idx[key] = c.ll.PushFront(&manifestCacheEntry{key: key, man: man, exp: exp})
	for c.ll.Len() > c.max {
		last := c.ll.Back()
		c.ll.Remove(last)
		delete(c.idx, last.Value.(*manifestCacheEntry).key)
	}
}

func (c *manifestCache) drop(key string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.idx[key]; ok {
		c.ll.Remove(el)
		delete(c.idx, key)
	}
}

func (c *manifestCache) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ll.Len()
}
