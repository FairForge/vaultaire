package api

// The registry's read side for the edge-call gate (WP-EDGE-3 step 7): a
// node's token is looked up by its sha256 in data_plane_hosts, through a
// 15 s cache keyed by the hash — an admit is one map read, a registry
// write invalidates at once on this process (the other slot's cache ages
// out within 15 s).
//
// RP74-2: the cache is an LRU of dataPlaneHostCacheMax entries, negative
// entries included, and an insert drops every expired entry first — a
// flood of distinct well-formed tokens costs one point query each and
// never grows the map past the bound. A token of any other shape never
// reaches it (isNodeTokenShape, in the gate).

import (
	"container/list"
	"context"
	"database/sql"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

const (
	dataPlaneHostCacheTTL = 15 * time.Second
	dataPlaneHostCacheMax = 1024
)

type cachedDataPlaneHost struct {
	host dataPlaneHost
	ok   bool // false = no row holds this hash (a negative entry)
	at   time.Time
}

type dataPlaneHostCacheEntry struct {
	hash string
	e    cachedDataPlaneHost
}

// dataPlaneHostCache caches registry rows by token hash.
type dataPlaneHostCache struct {
	db  *sql.DB
	mu  sync.Mutex
	gen int
	m   map[string]*list.Element // of *dataPlaneHostCacheEntry
	lru *list.List               // front = most recently used
	now func() time.Time
	// names caches rows read by host name (the slc row, RP74-7): a handful
	// of platform names, never a caller's input.
	names map[string]cachedDataPlaneHost

	lookups atomic.Int64 // database reads (the RP74-2 test counts them)
}

func newDataPlaneHostCache(db *sql.DB) *dataPlaneHostCache {
	return &dataPlaneHostCache{db: db, m: map[string]*list.Element{}, lru: list.New(), now: time.Now}
}

// dataPlaneHosts is the Server's cache (made on first use: tests build
// Servers as literals).
func (s *Server) dataPlaneHosts() *dataPlaneHostCache {
	s.dpHostsOnce.Do(func() { s.dpHosts = newDataPlaneHostCache(s.db) })
	return s.dpHosts
}

func (c *dataPlaneHostCache) invalidate() {
	c.mu.Lock()
	c.m = map[string]*list.Element{}
	c.lru.Init()
	c.names = nil
	c.gen++
	c.mu.Unlock()
}

func (c *dataPlaneHostCache) size() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.m)
}

// get is the entry under hash, marked most recently used.
func (c *dataPlaneHostCache) get(hash string) (cachedDataPlaneHost, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.m[hash]
	if !ok {
		return cachedDataPlaneHost{}, false
	}
	c.lru.MoveToFront(el)
	return el.Value.(*dataPlaneHostCacheEntry).e, true
}

// put stores e under hash: expired entries are dropped first, then the
// least recently used while the cache is full.
func (c *dataPlaneHostCache) put(hash string, e cachedDataPlaneHost) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.putLocked(hash, e)
}

func (c *dataPlaneHostCache) putLocked(hash string, e cachedDataPlaneHost) {
	if el, ok := c.m[hash]; ok {
		el.Value.(*dataPlaneHostCacheEntry).e = e
		c.lru.MoveToFront(el)
		return
	}
	now := c.now()
	for el := c.lru.Back(); el != nil; {
		prev := el.Prev()
		if ent := el.Value.(*dataPlaneHostCacheEntry); now.Sub(ent.e.at) >= dataPlaneHostCacheTTL {
			c.lru.Remove(el)
			delete(c.m, ent.hash)
		}
		el = prev
	}
	for len(c.m) >= dataPlaneHostCacheMax {
		el := c.lru.Back()
		c.lru.Remove(el)
		delete(c.m, el.Value.(*dataPlaneHostCacheEntry).hash)
	}
	c.m[hash] = c.lru.PushFront(&dataPlaneHostCacheEntry{hash: hash, e: e})
}

// touch updates the cached row under hash after its heartbeat (RP74-11:
// a heartbeat changes no token, so nothing is flushed); an entry not
// cached is left to the next lookup.
func (c *dataPlaneHostCache) touch(hash, health string, seen time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.m[hash]
	if !ok {
		return
	}
	ent := el.Value.(*dataPlaneHostCacheEntry)
	if !ent.e.ok {
		return
	}
	ent.e.host.Health = health
	ent.e.host.LastSeen = &seen
}

// putName caches the row of a platform host name (ok false: no row).
func (c *dataPlaneHostCache) putName(name string, h dataPlaneHost, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.names == nil {
		c.names = map[string]cachedDataPlaneHost{}
	}
	c.names[name] = cachedDataPlaneHost{host: h, ok: ok, at: c.now()}
}

// byName is the row of a platform host (slc, the Worker's), through the
// same 15 s cache (ok false: no row).
func (c *dataPlaneHostCache) byName(ctx context.Context, name string) (dataPlaneHost, bool, error) {
	c.mu.Lock()
	e, hit := c.names[name]
	gen := c.gen
	c.mu.Unlock()
	if hit && c.now().Sub(e.at) < dataPlaneHostCacheTTL {
		return e.host, e.ok, nil
	}
	if c.db == nil {
		return dataPlaneHost{}, false, nil
	}
	c.lookups.Add(1)
	h, err := readDataPlaneHost(ctx, c.db, name)
	ok := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return dataPlaneHost{}, false, err
	}
	c.mu.Lock()
	if c.gen == gen {
		if c.names == nil {
			c.names = map[string]cachedDataPlaneHost{}
		}
		c.names[name] = cachedDataPlaneHost{host: h, ok: ok, at: c.now()}
	}
	c.mu.Unlock()
	return h, ok, nil
}

// byTokenHash is the row whose token_hash is hash (ok false: none).
func (c *dataPlaneHostCache) byTokenHash(ctx context.Context, hash string) (dataPlaneHost, bool, error) {
	c.mu.Lock()
	gen := c.gen
	c.mu.Unlock()
	if e, hit := c.get(hash); hit && c.now().Sub(e.at) < dataPlaneHostCacheTTL {
		return e.host, e.ok, nil
	}
	if c.db == nil {
		return dataPlaneHost{}, false, nil
	}
	c.lookups.Add(1)
	var name string
	err := c.db.QueryRowContext(ctx, `SELECT host FROM data_plane_hosts WHERE token_hash = $1`, hash).Scan(&name)
	var h dataPlaneHost
	ok := false
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return dataPlaneHost{}, false, err
	default:
		if h, err = readDataPlaneHost(ctx, c.db, name); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return dataPlaneHost{}, false, err
		}
		ok = err == nil
	}
	c.mu.Lock()
	if c.gen == gen {
		c.putLocked(hash, cachedDataPlaneHost{host: h, ok: ok, at: c.now()})
	}
	c.mu.Unlock()
	return h, ok, nil
}
