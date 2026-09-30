package sitestats

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNormalize_BoundsEveryDimension(t *testing.T) {
	long := make([]byte, 300)
	for i := range long {
		long[i] = 'a'
	}
	h := Normalize(Hit{
		Kind: " view ", Name: "/docs/rclone", Referrer: "https://WWW.LowEndTalk.com/discussion/1?x=1",
		UTMSource: string(long), Country: "us", Device: "toaster",
	})
	assert.Equal(t, "view", h.Kind)
	assert.Equal(t, "lowendtalk.com", h.Referrer)
	assert.Len(t, h.UTMSource, maxLabel)
	assert.Equal(t, "US", h.Country)
	assert.Equal(t, "", h.Device, "unknown device classes are dropped")
}

func TestReferrerHost(t *testing.T) {
	cases := map[string]string{
		"":                                  "",
		"https://news.ycombinator.com/item": "news.ycombinator.com",
		"www.reddit.com":                    "reddit.com",
		"https://stored.ge/#pricing":        "",
		"https://www.stored.cloud/x":        "",
		"javascript:alert(1)":               "",
		"http://":                           "",
		"host with spaces":                  "",
		"t.co/abc":                          "t.co",
	}
	for in, want := range cases {
		assert.Equal(t, want, ReferrerHost(in), in)
	}
}

func TestCountryCode(t *testing.T) {
	assert.Equal(t, "DE", CountryCode("de"))
	assert.Equal(t, "T1", CountryCode("T1"))
	assert.Equal(t, "", CountryCode("DEU"))
	assert.Equal(t, "", CountryCode("d\x00"))
}

func TestDeviceClass(t *testing.T) {
	assert.Equal(t, "phone", DeviceClass("Mozilla/5.0 (Linux; Android 10; K) AppleWebKit/537.36 Chrome/153.0.0.0 Mobile Safari/537.36"))
	assert.Equal(t, "phone", DeviceClass("Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) Mobile/15E148"))
	assert.Equal(t, "tablet", DeviceClass("Mozilla/5.0 (iPad; CPU OS 17_0 like Mac OS X)"))
	assert.Equal(t, "desktop", DeviceClass("Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) Firefox/155.0"))
}

func TestIsBot(t *testing.T) {
	assert.True(t, IsBot(""))
	assert.True(t, IsBot("curl/8.7.1"))
	assert.True(t, IsBot("Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)"))
	assert.True(t, IsBot("Mozilla/5.0 HeadlessChrome/120.0"))
	assert.False(t, IsBot("Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/152.0.0.0 Safari/537.36"))
}

func TestPagePath(t *testing.T) {
	for p, want := range map[string]string{
		"/": "/", "/docs": "/docs", "/docs/": "/docs", "/docs/rclone": "/docs/rclone", "/legal/privacy": "/legal/privacy",
		"/changelog": "/changelog", "/register": "/register",
	} {
		got, ok := PagePath(p)
		assert.True(t, ok, p)
		assert.Equal(t, want, got, p)
	}
	for _, p := range []string{"", "/health", "/metrics", "/dashboard", "/admin/stats", "/api/v1/user", "/bucket/key", "/static/js/x.js", "/legal/", "/docs/api/x/" + string(make([]byte, 100))} {
		_, ok := PagePath(p)
		assert.False(t, ok, p)
	}
}

func TestEventAllowed(t *testing.T) {
	assert.True(t, EventAllowed("builder.edit"))
	assert.True(t, EventAllowed("uc.backup"))
	assert.False(t, EventAllowed("uc.anything"))
	assert.False(t, EventAllowed(""))
	assert.False(t, EventAllowed("<script>"))
}

func TestVisitorHash_RotatesDaily(t *testing.T) {
	c := New(nil)
	day := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return day }
	a := c.VisitorHash("203.0.113.9", "Mozilla/5.0")
	assert.Equal(t, a, c.VisitorHash("203.0.113.9", "Mozilla/5.0"), "stable within a day")
	assert.NotEqual(t, a, c.VisitorHash("203.0.113.10", "Mozilla/5.0"))
	assert.Len(t, a, 24)
	c.now = func() time.Time { return day.Add(24 * time.Hour) }
	assert.NotEqual(t, a, c.VisitorHash("203.0.113.9", "Mozilla/5.0"), "a new salt every UTC day")
}

func TestRecord_FoldsAndDropsPastTheKeyCap(t *testing.T) {
	c := New(nil)
	for i := 0; i < maxKeys; i++ {
		c.Record(Hit{Kind: KindView, Name: "/", UTMSource: "s" + string(rune('a'+i%26)) + string(rune('a'+(i/26)%26)) + string(rune('a'+(i/676)%26))})
	}
	keys, _ := c.Pending()
	require.Equal(t, maxKeys, keys)
	c.Record(Hit{Kind: KindView, Name: "/", UTMSource: "one-more-campaign"})
	keys, _ = c.Pending()
	assert.Equal(t, maxKeys+1, keys, "past the cap a new key is folded onto its page without campaign dims")
	c.Record(Hit{Kind: KindView, Name: "/", UTMSource: "another-campaign"})
	keys, _ = c.Pending()
	assert.Equal(t, maxKeys+1, keys, "folded keys share one row")
	c.Record(Hit{Kind: "", Name: "/"})
	c.Record(Hit{Kind: KindView, Name: ""})
	keys, _ = c.Pending()
	assert.Equal(t, maxKeys+1, keys, "a hit without a kind or name is ignored")
}

func TestFlush_UpsertsCountsAndUniquesThenPrunes(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	c := New(nil)
	c.now = func() time.Time { return time.Date(2026, 9, 30, 23, 59, 0, 0, time.UTC) }
	c.Record(Hit{Kind: KindView, Name: "/", Referrer: "lowendtalk.com", Country: "de", Device: "desktop"})
	c.Record(Hit{Kind: KindView, Name: "/", Referrer: "lowendtalk.com", Country: "de", Device: "desktop"})
	c.RecordVisitor("203.0.113.9", "Mozilla/5.0")

	mock.ExpectBegin()
	mock.ExpectExec(`INSERT INTO site_stats_daily`).
		WithArgs("2026-09-30", "view", "/", "lowendtalk.com", "", "", "", "DE", "desktop", int64(2)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO site_visitors_daily`).WithArgs("2026-09-30", sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO site_stats_daily \(day, kind, name, referrer, utm_source, utm_medium, utm_campaign, country, device, n\)\s+SELECT \$1, 'uniques'`).
		WithArgs("2026-09-30").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`DELETE FROM site_visitors_daily WHERE day < \$1`).WithArgs("2026-09-29").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectCommit()

	require.NoError(t, c.Flush(context.Background(), db))
	require.NoError(t, mock.ExpectationsWereMet())
	keys, visitors := c.Pending()
	assert.Zero(t, keys)
	assert.Zero(t, visitors)
}

func TestFlush_KeepsTheBufferWhenTheWriteFails(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	c := New(nil)
	c.Record(Hit{Kind: KindEvent, Name: "builder.edit"})
	mock.ExpectBegin()
	mock.ExpectExec(`INSERT INTO site_stats_daily`).WillReturnError(assert.AnError)
	mock.ExpectRollback()

	require.Error(t, c.Flush(context.Background(), db))
	keys, _ := c.Pending()
	assert.Equal(t, 1, keys, "the count is merged back for the next flush")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestFlush_NilDBDropsTheBuffer(t *testing.T) {
	c := New(nil)
	c.Record(Hit{Kind: KindView, Name: "/"})
	require.NoError(t, c.Flush(context.Background(), nil))
	keys, _ := c.Pending()
	assert.Zero(t, keys)
}
