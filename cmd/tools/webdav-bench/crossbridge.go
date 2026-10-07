package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/FairForge/vaultaire/internal/drivers"
	"github.com/FairForge/vaultaire/internal/engine"
)

// reportURL is the server(s) a run measures.
func reportURL(cfg config) string {
	if len(cfg.URLs) > 0 {
		return strings.Join(cfg.URLs, ",")
	}
	return cfg.URL
}

// --- crossbridge ------------------------------------------------------------

// suiteCrossbridge measures cross-bridge staleness (-urls, ≥ 2 bridges of
// one folder): each round writes an object through bridge i and polls
// bridge j until it serves those bytes, then overwrites it through i and
// waits until j serves the new bytes, then deletes it through i and waits
// until j answers not found. The rows' latencies are those waits (live on
// SLC, 2026-10-07: new 1.5–13 s, overwrite ~30 s — once 310 s —, delete
// ~30 s). A wait that passes -crossbridge-timeout is an error. Rounds pair
// every bridge with every other, -crossbridge-conc at a time.
func (b *bench) suiteCrossbridge() {
	n := len(b.bridges)
	if n < 2 {
		b.fail("crossbridge: needs -urls with two bridges or more")
		return
	}
	var mu sync.Mutex
	var newLat, owLat, delLat []time.Duration
	var errs [3]int
	add := func(which int, l *[]time.Duration, d time.Duration, err error) {
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			errs[which]++
			return
		}
		*l = append(*l, d)
	}
	var next atomic.Int64
	next.Store(-1)
	var wg sync.WaitGroup
	start := time.Now()
	for w := 0; w < b.cfg.CrossConc; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				r := int(next.Add(1))
				if r >= b.cfg.CrossN || b.ctx.Err() != nil {
					return
				}
				i := r % n
				j := (i + 1 + (r/n)%(n-1)) % n
				b.crossRound(r, i, j, add, &newLat, &owLat, &delLat)
			}
		}()
	}
	wg.Wait()
	wall := time.Since(start)
	cs := fmt.Sprintf("%s ×%d %db", sizeName(b.cfg.CrossSize), b.cfg.CrossN, n)
	b.addRow(row("crossbridge", cs, "visible new", newLat, errs[0], wall, 0))
	b.addRow(row("crossbridge", cs, "visible overwrite", owLat, errs[1], wall, 0))
	b.addRow(row("crossbridge", cs, "visible delete", delLat, errs[2], wall, 0))
	b.dropFolder("crossbridge")
}

// crossRound is one round: write/overwrite/delete via bridge i, each
// waited for on bridge j.
func (b *bench) crossRound(r, i, j int, add func(int, *[]time.Duration, time.Duration, error), newLat, owLat, delLat *[]time.Duration) {
	key := fmt.Sprintf("crossbridge/k%04d", r)
	size := b.cfg.CrossSize
	s1, s2 := seedFor(b.cfg.Seed, key+"#v1"), seedFor(b.cfg.Seed, key+"#v2")
	h1, h2 := expectedHash(s1, size), expectedHash(s2, size)
	wr, rd := b.bridges[i], b.bridges[j]
	label := fmt.Sprintf("crossbridge %s %d→%d", key, i, j)

	put := func(seed uint64) error {
		return b.op(func(ctx context.Context) error {
			return wr.Put(ctx, b.container, key, newGen(seed, size), engine.WithContentLength(size))
		})
	}
	if err := put(s1); err != nil {
		b.fail("%s PUT v1: %v", label, err)
		add(0, newLat, 0, err)
		return
	}
	d, err := b.waitFor(rd, key, h1, h2, "v1")
	add(0, newLat, d, err)
	if err != nil {
		b.fail("%s new: %v", label, err)
	}
	if err := put(s2); err != nil {
		b.fail("%s PUT v2: %v", label, err)
		add(1, owLat, 0, err)
		return
	}
	d, err = b.waitFor(rd, key, h1, h2, "v2")
	add(1, owLat, d, err)
	if err != nil {
		b.fail("%s overwrite: %v", label, err)
	}
	if err := b.op(func(ctx context.Context) error { return wr.Delete(ctx, b.container, key) }); err != nil {
		b.fail("%s DELETE: %v", label, err)
		add(2, delLat, 0, err)
		return
	}
	d, err = b.waitFor(rd, key, h1, h2, "missing")
	add(2, delLat, d, err)
	if err != nil {
		b.fail("%s delete: %v", label, err)
	}
}

// waitFor polls d until key reads as want ("v1", "v2", "missing") and
// returns how long that took; bytes that were never written are a
// MISMATCH.
func (b *bench) waitFor(d *drivers.WebDAVDriver, key string, h1, h2 []byte, want string) (time.Duration, error) {
	start := time.Now()
	last := ""
	for {
		s, err := b.readStateVia(d, key, h1, h2)
		if err == nil {
			if s == want {
				return time.Since(start), nil
			}
			if s == "other" {
				b.mismatch("crossbridge %s: bytes that were never written", key)
				return 0, fmt.Errorf("bytes that were never written")
			}
			last = s
		} else {
			last = err.Error()
		}
		if time.Since(start) > b.cfg.CrossTimeout {
			return 0, fmt.Errorf("still %q after %s (want %s)", last, b.cfg.CrossTimeout, want)
		}
		select {
		case <-b.ctx.Done():
			return 0, b.ctx.Err()
		case <-time.After(b.cfg.CrossPoll):
		}
	}
}

// readStateVia is readState through one bridge's own driver.
func (b *bench) readStateVia(d *drivers.WebDAVDriver, key string, h1, h2 []byte) (string, error) {
	ctx, cancel := b.opCtx()
	defer cancel()
	rc, err := d.Get(ctx, b.container, key)
	if isNotFound(err) {
		return "missing", nil
	}
	if err != nil {
		return "", err
	}
	defer func() { _ = rc.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, rc); err != nil {
		return "", fmt.Errorf("read %s: %w", key, err)
	}
	sum := h.Sum(nil)
	switch {
	case bytes.Equal(sum, h1):
		return "v1", nil
	case bytes.Equal(sum, h2):
		return "v2", nil
	}
	return "other", nil
}
