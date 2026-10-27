package api

// The registry's read side for the edge-call gate (WP-EDGE-3 step 7): a
// node's token is looked up by its sha256 in data_plane_hosts, through a
// 15 s cache keyed by the hash — an admit is one map read, a registry
// write invalidates at once on this process (the other slot's cache ages
// out within 15 s).

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"time"
)

const dataPlaneHostCacheTTL = 15 * time.Second

type cachedDataPlaneHost struct {
	host dataPlaneHost
	ok   bool // false = no row holds this hash (a negative entry)
	at   time.Time
}

// dataPlaneHostCache caches registry rows by token hash.
type dataPlaneHostCache struct {
	db  *sql.DB
	mu  sync.Mutex
	gen int
	m   map[string]cachedDataPlaneHost
	now func() time.Time
}

func newDataPlaneHostCache(db *sql.DB) *dataPlaneHostCache {
	return &dataPlaneHostCache{db: db, m: map[string]cachedDataPlaneHost{}, now: time.Now}
}

// dataPlaneHosts is the Server's cache (made on first use: tests build
// Servers as literals).
func (s *Server) dataPlaneHosts() *dataPlaneHostCache {
	s.dpHostsOnce.Do(func() { s.dpHosts = newDataPlaneHostCache(s.db) })
	return s.dpHosts
}

func (c *dataPlaneHostCache) invalidate() {
	c.mu.Lock()
	c.m = map[string]cachedDataPlaneHost{}
	c.gen++
	c.mu.Unlock()
}

// byTokenHash is the row whose token_hash is hash (ok false: none).
func (c *dataPlaneHostCache) byTokenHash(ctx context.Context, hash string) (dataPlaneHost, bool, error) {
	c.mu.Lock()
	e, hit := c.m[hash]
	gen := c.gen
	c.mu.Unlock()
	if hit && c.now().Sub(e.at) < dataPlaneHostCacheTTL {
		return e.host, e.ok, nil
	}
	if c.db == nil {
		return dataPlaneHost{}, false, nil
	}
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
		c.m[hash] = cachedDataPlaneHost{host: h, ok: ok, at: c.now()}
	}
	c.mu.Unlock()
	return h, ok, nil
}
