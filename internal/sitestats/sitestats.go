// Package sitestats counts visits to the public site without cookies,
// identifiers or third-party scripts.
//
// What it keeps: daily totals of page views and named clicks on the marketing
// pages, cut by page, referring host, campaign labels (utm_*), country (the
// CF-IPCountry header Cloudflare already sends) and device class. Nothing
// about one visitor is stored. Unique visitors per day are counted the way
// Plausible does it: a hash of (IP, user agent) under a secret that rotates
// every UTC day and never leaves memory, kept only long enough to be counted
// (the hash rows are pruned after two days; the daily count stays).
//
// The collector buffers in memory and a flusher upserts the aggregates every
// few seconds, so counting costs one map increment per request. It is
// best-effort by design: a failed flush is retried on the next tick and the
// site never waits on it.
package sitestats

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
)

// Kinds of row in site_stats_daily.
const (
	KindView    = "view"    // one rendered public page (name = path)
	KindEvent   = "event"   // one named click from the page's beacon
	KindUniques = "uniques" // the day's distinct-visitor count (name empty)
)

// Hit is one countable thing. Every text dimension is bounded by Normalize.
type Hit struct {
	Kind        string
	Name        string
	Referrer    string
	UTMSource   string
	UTMMedium   string
	UTMCampaign string
	Country     string
	Device      string
}

// Bounds on the dimensions so a scripted visitor cannot grow the table.
const (
	maxName  = 80
	maxLabel = 100
	// maxKeys caps distinct (day, dims) keys buffered between flushes. Past
	// it new keys are folded onto their page/event with the referrer and
	// campaign dropped; past twice it they are dropped altogether.
	maxKeys = 4000
	// maxVisitors caps the distinct visitor hashes buffered between flushes.
	maxVisitors = 50000
	// visitorRetentionDays is how long a visitor hash row may live: today
	// and yesterday (a flush just after midnight still carries yesterday).
	visitorRetentionDays = 2
)

// Collector buffers hits and visitor hashes until Flush writes them.
type Collector struct {
	mu       sync.Mutex
	counts   map[key]int64
	visitors map[string]map[string]struct{} // day -> visitor hashes
	salt     [32]byte
	saltDay  string
	now      func() time.Time
	logger   *zap.Logger
}

type key struct {
	day string
	hit Hit
}

// New returns an empty collector. The logger may be nil.
func New(logger *zap.Logger) *Collector {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &Collector{
		counts:   make(map[key]int64),
		visitors: make(map[string]map[string]struct{}),
		now:      time.Now,
		logger:   logger,
	}
}

func (c *Collector) today() string { return c.now().UTC().Format("2006-01-02") }

// Record counts one hit for today. The hit is normalised first.
func (c *Collector) Record(h Hit) {
	h = Normalize(h)
	if h.Kind == "" || (h.Kind != KindUniques && h.Name == "") {
		return
	}
	k := key{day: c.today(), hit: h}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, seen := c.counts[k]; !seen && len(c.counts) >= maxKeys {
		k.hit.Referrer, k.hit.UTMSource, k.hit.UTMMedium, k.hit.UTMCampaign = "", "", "", ""
		if _, seen := c.counts[k]; !seen && len(c.counts) >= 2*maxKeys {
			return
		}
	}
	c.counts[k]++
}

// RecordVisitor notes that a visitor (by daily-salted hash) was seen today.
func (c *Collector) RecordVisitor(ip, userAgent string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	day := c.today()
	c.rotateSaltLocked(day)
	set := c.visitors[day]
	if set == nil {
		set = make(map[string]struct{})
		c.visitors[day] = set
	}
	if len(set) >= maxVisitors {
		return
	}
	set[c.hashLocked(ip, userAgent)] = struct{}{}
}

// VisitorHash returns today's hash for (ip, userAgent). Exposed for tests:
// the same pair hashes the same within a day and differently across days.
func (c *Collector) VisitorHash(ip, userAgent string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rotateSaltLocked(c.today())
	return c.hashLocked(ip, userAgent)
}

func (c *Collector) rotateSaltLocked(day string) {
	if c.saltDay == day {
		return
	}
	if _, err := rand.Read(c.salt[:]); err != nil {
		// crypto/rand failing is a broken host; a time-derived salt still
		// keeps the hash from being reproducible across days.
		copy(c.salt[:], sha256.New().Sum([]byte(day+fmt.Sprint(c.now().UnixNano()))))
	}
	c.saltDay = day
}

func (c *Collector) hashLocked(ip, userAgent string) string {
	h := sha256.New()
	h.Write(c.salt[:])
	h.Write([]byte{0})
	h.Write([]byte(ip))
	h.Write([]byte{0})
	h.Write([]byte(userAgent))
	return hex.EncodeToString(h.Sum(nil))[:24]
}

// Pending reports how many count keys and visitor hashes are buffered.
func (c *Collector) Pending() (keys, visitors int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, set := range c.visitors {
		visitors += len(set)
	}
	return len(c.counts), visitors
}

// Flush writes the buffered aggregates to the database and prunes visitor
// hashes older than yesterday. On error the buffer is merged back so the
// next flush retries; a nil db drops the buffer (dev without Postgres).
func (c *Collector) Flush(ctx context.Context, db *sql.DB) error {
	c.mu.Lock()
	counts, visitors := c.counts, c.visitors
	c.counts = make(map[key]int64)
	c.visitors = make(map[string]map[string]struct{})
	c.mu.Unlock()
	if db == nil || (len(counts) == 0 && len(visitors) == 0) {
		return nil
	}
	if err := c.write(ctx, db, counts, visitors); err != nil {
		c.mu.Lock()
		for k, n := range counts {
			if _, seen := c.counts[k]; seen || len(c.counts) < 2*maxKeys {
				c.counts[k] += n
			}
		}
		for day, set := range visitors {
			cur := c.visitors[day]
			if cur == nil {
				cur = make(map[string]struct{})
				c.visitors[day] = cur
			}
			for v := range set {
				if len(cur) >= maxVisitors {
					break
				}
				cur[v] = struct{}{}
			}
		}
		c.mu.Unlock()
		return err
	}
	return nil
}

func (c *Collector) write(ctx context.Context, db *sql.DB, counts map[key]int64, visitors map[string]map[string]struct{}) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("site stats flush: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for k, n := range counts {
		h := k.hit
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO site_stats_daily (day, kind, name, referrer, utm_source, utm_medium, utm_campaign, country, device, n)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
			ON CONFLICT (day, kind, name, referrer, utm_source, utm_medium, utm_campaign, country, device)
			DO UPDATE SET n = site_stats_daily.n + EXCLUDED.n`,
			k.day, h.Kind, h.Name, h.Referrer, h.UTMSource, h.UTMMedium, h.UTMCampaign, h.Country, h.Device, n); err != nil {
			return fmt.Errorf("site stats flush: upsert %s %q: %w", h.Kind, h.Name, err)
		}
	}
	for day, set := range visitors {
		for v := range set {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO site_visitors_daily (day, visitor) VALUES ($1, $2) ON CONFLICT DO NOTHING`, day, v); err != nil {
				return fmt.Errorf("site stats flush: visitor: %w", err)
			}
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO site_stats_daily (day, kind, name, referrer, utm_source, utm_medium, utm_campaign, country, device, n)
			SELECT $1, 'uniques', '', '', '', '', '', '', '', COUNT(*) FROM site_visitors_daily WHERE day = $1
			ON CONFLICT (day, kind, name, referrer, utm_source, utm_medium, utm_campaign, country, device)
			DO UPDATE SET n = EXCLUDED.n`, day); err != nil {
			return fmt.Errorf("site stats flush: uniques: %w", err)
		}
	}
	cutoff := c.now().UTC().AddDate(0, 0, -(visitorRetentionDays - 1)).Format("2006-01-02")
	if _, err := tx.ExecContext(ctx, `DELETE FROM site_visitors_daily WHERE day < $1`, cutoff); err != nil {
		return fmt.Errorf("site stats flush: prune visitors: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("site stats flush: commit: %w", err)
	}
	return nil
}

// Run flushes every interval until ctx is done, then flushes once more.
// Without a database it returns at once.
func (c *Collector) Run(ctx context.Context, db *sql.DB, every time.Duration) {
	if db == nil {
		return
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			flushCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if err := c.Flush(flushCtx, db); err != nil {
				c.logger.Warn("site stats final flush", zap.Error(err))
			}
			cancel()
			return
		case <-t.C:
			if err := c.Flush(ctx, db); err != nil {
				c.logger.Warn("site stats flush", zap.Error(err))
			}
		}
	}
}

// Normalize bounds and canonicalises every dimension of a hit: lowercase
// referrer host without a leading "www.", labels cut at 100 characters,
// country as two upper-case characters or empty, device from the known set.
func Normalize(h Hit) Hit {
	h.Kind = strings.TrimSpace(h.Kind)
	h.Name = cut(strings.TrimSpace(h.Name), maxName)
	h.Referrer = ReferrerHost(h.Referrer)
	h.UTMSource = cut(strings.TrimSpace(h.UTMSource), maxLabel)
	h.UTMMedium = cut(strings.TrimSpace(h.UTMMedium), maxLabel)
	h.UTMCampaign = cut(strings.TrimSpace(h.UTMCampaign), maxLabel)
	h.Country = CountryCode(h.Country)
	switch h.Device {
	case "desktop", "phone", "tablet":
	default:
		h.Device = ""
	}
	return h
}

func cut(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// ReferrerHost reduces a referrer (a full URL or a bare host) to its lowercase
// host without "www.", or "" when it is empty, unparsable or one of ours.
func ReferrerHost(ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ""
	}
	host := ref
	if strings.Contains(ref, "://") {
		u, err := url.Parse(ref)
		if err != nil || u.Host == "" {
			return ""
		}
		host = u.Hostname()
	} else if i := strings.IndexAny(host, "/?#:"); i >= 0 {
		host = host[:i]
	}
	host = strings.TrimPrefix(strings.ToLower(host), "www.")
	for _, r := range host {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.' || r == '-') {
			return ""
		}
	}
	if host == "" || !strings.Contains(host, ".") || isOwnHost(host) {
		return ""
	}
	return cut(host, maxLabel)
}

func isOwnHost(host string) bool {
	for _, own := range []string{"stored.ge", "stored.cloud", "localhost"} {
		if host == own || strings.HasSuffix(host, "."+own) {
			return true
		}
	}
	return false
}

// CountryCode keeps a two-character upper-case code (Cloudflare uses "XX"
// for unknown and "T1" for Tor); anything else becomes "".
func CountryCode(c string) string {
	c = strings.ToUpper(strings.TrimSpace(c))
	if len(c) != 2 {
		return ""
	}
	for _, r := range c {
		if !(r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			return ""
		}
	}
	return c
}

// DeviceClass is a coarse cut of the user agent: phone, tablet or desktop.
func DeviceClass(userAgent string) string {
	ua := strings.ToLower(userAgent)
	switch {
	case strings.Contains(ua, "ipad") || strings.Contains(ua, "tablet"):
		return "tablet"
	case strings.Contains(ua, "mobi") || strings.Contains(ua, "android"):
		return "phone"
	default:
		return "desktop"
	}
}

// botMarks are substrings (lower-case) of user agents that are not people:
// crawlers, monitors, the CLI tools the docs quote, and headless browsers.
var botMarks = []string{
	"bot", "crawl", "spider", "slurp", "curl/", "wget/", "python", "go-http-client", "java/", "okhttp",
	"http.rb", "scrapy", "headlesschrome", "lighthouse", "pingdom", "uptime", "monitor", "facebookexternalhit",
	"preview", "node-fetch", "axios/", "libwww", "phantomjs", "httpclient",
}

// IsBot reports whether a user agent should not be counted. An empty user
// agent is a bot.
func IsBot(userAgent string) bool {
	ua := strings.ToLower(strings.TrimSpace(userAgent))
	if ua == "" {
		return true
	}
	for _, m := range botMarks {
		if strings.Contains(ua, m) {
			return true
		}
	}
	return false
}

// pagePrefixes are the public pages that count as a view; anything else (the
// dashboard, the S3 API, static files, probes) never does.
var pageExact = map[string]bool{"/": true, "/changelog": true, "/status": true, "/register": true, "/login": true, "/abuse": true, "/docs": true}
var pagePrefixes = []string{"/docs/", "/legal/"}

// PagePath returns the name a request path is counted under and whether it
// is a public page at all. Trailing slashes are dropped and the path is
// bounded.
func PagePath(p string) (string, bool) {
	if p == "" {
		return "", false
	}
	if p != "/" {
		p = strings.TrimRight(p, "/")
	}
	if len(p) > maxName {
		return "", false
	}
	if pageExact[p] {
		return p, true
	}
	for _, pre := range pagePrefixes {
		if strings.HasPrefix(p, pre) && len(p) > len(pre) {
			return p, true
		}
	}
	return "", false
}

// allowedEvents is the closed set of names the beacon may count, so the
// table's cardinality is ours to decide, not a visitor's.
var allowedEvents = map[string]bool{
	"builder.edit": true, "builder.attic": true, "builder.share": true, "builder.save": true, "builder.pack": true,
	"build.view": true, "pricing.view": true, "calc.move": true, "age.scrub": true, "copy": true,
	"waitlist.submit": true, "cta.register": true,
	"uc.backup": true, "uc.app": true, "uc.media": true, "uc.worm": true, "uc.lake": true,
	"res.rclone": true, "res.restic": true, "res.awscli": true, "res.cyberduck": true, "res.juicefs": true, "res.sdk": true,
	"res.presign": true, "res.keys": true, "res.hooks": true, "res.public": true, "res.lock": true,
}

// EventAllowed reports whether the beacon may count an event of this name.
func EventAllowed(name string) bool { return allowedEvents[name] }
