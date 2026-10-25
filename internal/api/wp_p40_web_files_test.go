package api

import (
	"bytes"
	"encoding/binary"
	"encoding/xml"
	"image/color"
	"image/png"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FairForge/vaultaire/internal/dashboard"
	"github.com/FairForge/vaultaire/internal/produce"
)

// Stage 1a Prompt 40 Part 0 item 3 (RP56-3, the WP-P39 review): crawlers
// and link unfurlers send Accept */* or image/*, so /robots.txt,
// /sitemap.xml and /favicon.ico answered every one of them with the S3
// API's 403 XML. They are now served to an anonymous GET/HEAD on a web host;
// a signed request is the S3 API's (the credential gate), and an s3., cdn.
// or trust. host never serves them.

// crawlerAccepts are what a crawler, an unfurler and a browser's icon fetch send.
var crawlerAccepts = []string{"", "*/*", "image/avif,image/webp,image/*,*/*;q=0.8", "text/plain"}

func TestWebFiles_ServedToAnAnonymousFetchOnAWebHost(t *testing.T) {
	for _, tc := range []struct {
		name, origin string
		env          map[string]string
		hosts        []string
	}{
		{"prod today", "https://stored.ge", prodTodayHosts, []string{"stored.ge", "stored.cloud", "www.stored.ge"}},
		{"prod target", "https://stored.cloud", prodTargetHosts, []string{"stored.cloud"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, hits := gateServer(t, tc.env)
			h := srv.httpServer.Handler
			for _, host := range tc.hosts {
				for _, accept := range crawlerAccepts {
					for _, f := range []struct{ path, ctype, cache string }{
						{"/robots.txt", "text/plain; charset=utf-8", "public, max-age=3600"},
						{"/sitemap.xml", "application/xml; charset=utf-8", "public, max-age=300"},
						{"/favicon.ico", "image/x-icon", "public, max-age=86400"},
					} {
						for _, m := range []string{http.MethodGet, http.MethodHead} {
							*hits = nil
							w := webReq{method: m, host: host, path: f.path, accept: accept}.do(h)
							msg := m + " " + host + f.path + " Accept " + accept
							require.Equal(t, http.StatusOK, w.Code, msg)
							assert.Equal(t, f.ctype, w.Header().Get("Content-Type"), msg)
							assert.Equal(t, f.cache, w.Header().Get("Cache-Control"), msg)
							assert.Equal(t, "no-store", w.Header().Get("CDN-Cache-Control"),
								"%s: never stored at the edge — a cached copy answers a SIGNED request too (RP57-5, prod stored.cloud 2026-10-25)", msg)
							assert.Empty(t, *hits, msg)
							if m == http.MethodHead {
								assert.Empty(t, w.Body.Bytes(), msg)
								assert.NotEmpty(t, w.Header().Get("Content-Length"), msg)
							}
						}
					}
				}
				w := webReq{host: host, path: "/robots.txt", accept: "*/*"}.do(h)
				assert.Equal(t, "User-agent: *\nAllow: /\nDisallow: /dashboard\nDisallow: /api/\nDisallow: /auth/\nDisallow: /cdn/\n\nSitemap: "+tc.origin+"/sitemap.xml\n", w.Body.String())
			}
		})
	}
}

// A signed or presigned request is the S3 API's (bucket "robots.txt",
// "favicon.ico", "sitemap.xml"); so is anything an S3 client sends, and
// every request on an s3. host. The cdn. and trust. hosts have their own
// routers and never serve them.
func TestWebFiles_NeverOnTheS3PathOrTheOtherHosts(t *testing.T) {
	srv, hits := gateServer(t, prodTargetHosts)
	h := srv.httpServer.Handler
	for _, p := range []string{"/robots.txt", "/sitemap.xml", "/favicon.ico"} {
		for _, host := range []string{"stored.cloud", "stored.ge", "s3.stored.cloud"} {
			*hits = nil
			serve(h, sigV4Signed(t, http.MethodGet, host, p))
			assert.Len(t, *hits, 1, "signed GET %s%s: the S3 API's", host, p)
		}
		for _, hdr := range []map[string]string{{"x-amz-content-sha256": "UNSIGNED-PAYLOAD"}, {"Authorization": "Bearer x"}} {
			w := webReq{host: "stored.cloud", path: p, accept: "*/*", header: hdr}.do(h)
			assert.NotEqual(t, http.StatusOK, w.Code, "%s with %v", p, hdr)
		}
		w := webReq{host: "stored.cloud", path: p + "?X-Amz-Credential=AKID", accept: "*/*"}.do(h)
		assert.NotEqual(t, http.StatusOK, w.Code, "%s presigned-shaped", p)
		w = webReq{host: "s3.stored.cloud", path: p, accept: "*/*"}.do(h)
		assert.Equal(t, http.StatusForbidden, w.Code, "s3.%s: the S3 API's", p)
		assert.Contains(t, w.Body.String(), "<Error>")
		for _, host := range []string{"cdn.stored.cloud", "trust.stored.cloud"} {
			w = webReq{host: host, path: p, accept: "*/*"}.do(h)
			assert.Equal(t, http.StatusNotFound, w.Code, "%s%s", host, p)
		}
		w = webReq{method: http.MethodPut, host: "stored.cloud", path: p}.do(h)
		assert.NotEqual(t, http.StatusOK, w.Code, "PUT %s is CreateBucket's (refused unsigned)", p)
	}
}

type sitemap struct {
	URLs []struct {
		Loc     string `xml:"loc"`
		LastMod string `xml:"lastmod"`
	} `xml:"url"`
}

// The sitemap lists LIVE pages only, as canonical absolute URLs, flag by
// flag: every URL answers a browser 200 HTML on the same server, and a dark
// page is never in it (the TestLegalDraftPages_NothingLinksThem family).
func TestWebFiles_TheSitemapListsLivePagesOnly(t *testing.T) {
	dark := append(append([]string{}, legalDraftPaths...), "/legal/law-enforcement", "/legal/what-we-can-produce")
	for _, tc := range []struct {
		name    string
		flagsOn []string
		absent  []string
		present []string
	}{
		{"every legal flag off", nil, dark, nil},
		{"legal_draft_pages on", []string{dashboard.FlagLegalDraftPages}, []string{"/legal/law-enforcement", "/legal/what-we-can-produce"}, legalDraftPaths},
		{"legal_le_pages on", []string{produce.FlagLegalLEPages}, legalDraftPaths, []string{"/legal/law-enforcement", "/legal/what-we-can-produce"}},
		{"both on", []string{dashboard.FlagLegalDraftPages, produce.FlagLegalLEPages}, nil, dark},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := gateServer(t, prodTargetHosts)
			for _, k := range tc.flagsOn {
				srv.flags.Register(k, true) // the fixture's own default, never a '*' row
			}
			h := srv.httpServer.Handler
			w := webReq{host: "stored.cloud", path: "/sitemap.xml", accept: "*/*"}.do(h)
			require.Equal(t, http.StatusOK, w.Code)
			assert.NotContains(t, w.Body.String(), "<lastmod>", "no lastmod: nothing knows when a page last changed")
			var sm sitemap
			require.NoError(t, xml.Unmarshal(w.Body.Bytes(), &sm), w.Body.String())
			var paths []string
			for _, u := range sm.URLs {
				pu, err := url.Parse(u.Loc)
				require.NoError(t, err)
				assert.Equal(t, "https", pu.Scheme, u.Loc)
				assert.Equal(t, "stored.cloud", pu.Host, "%s: the canonical origin", u.Loc)
				paths = append(paths, pu.Path)
				page := webReq{host: "stored.cloud", path: pu.Path, accept: browserAccept}.do(h)
				assert.Equal(t, http.StatusOK, page.Code, "%s is listed, so it must be live", pu.Path)
				assert.True(t, strings.HasPrefix(page.Header().Get("Content-Type"), "text/html"), "%s: a page", pu.Path)
			}
			for _, want := range []string{"/", "/docs", "/docs/getting-started", "/docs/rclone", "/docs/faq", "/docs/api", "/status", "/changelog",
				"/legal/privacy", "/legal/terms", "/legal/aup"} {
				assert.Contains(t, paths, want)
			}
			for _, p := range tc.absent {
				assert.NotContains(t, paths, p, "a dark page is never in the sitemap")
			}
			for _, p := range tc.present {
				assert.Contains(t, paths, p, "a page whose flag is on is in the sitemap")
			}
		})
	}
}

// iconGrid re-derives the landing's icon from its data: SVG in
// landing.src.html — independently of favicon.py — as a grid of colours.
func iconGrid(t *testing.T) (int, [][]color.NRGBA) {
	t.Helper()
	src, err := os.ReadFile("landing/landing.src.html")
	require.NoError(t, err)
	m := regexp.MustCompile(`<link rel="icon" href="data:image/svg\+xml,([^"]*)"`).FindSubmatch(src)
	require.NotNil(t, m, "the landing's icon link")
	svg, err := url.PathUnescape(string(m[1]))
	require.NoError(t, err)
	vb := regexp.MustCompile(`viewBox="(-?\d+) (-?\d+) (\d+) (\d+)"`).FindStringSubmatch(svg)
	require.NotNil(t, vb)
	atoi := func(s string) int { n, err := strconv.Atoi(s); require.NoError(t, err); return n }
	ox, oy, n := atoi(vb[1]), atoi(vb[2]), atoi(vb[3])
	g := make([][]color.NRGBA, n)
	for i := range g {
		g[i] = make([]color.NRGBA, n)
	}
	hex := func(c string) color.NRGBA {
		c = strings.TrimPrefix(c, "#")
		if len(c) == 3 {
			c = string([]byte{c[0], c[0], c[1], c[1], c[2], c[2]})
		}
		v, err := strconv.ParseUint(c, 16, 32)
		require.NoError(t, err)
		return color.NRGBA{uint8(v >> 16), uint8(v >> 8), uint8(v), 255}
	}
	fill := func(x, y, w, h int, c color.NRGBA) {
		for yy := y - oy; yy < y-oy+h; yy++ {
			for xx := x - ox; xx < x-ox+w; xx++ {
				g[yy][xx] = c
			}
		}
	}
	rects := 0
	for _, r := range regexp.MustCompile(`<rect x="(-?\d+)" y="(-?\d+)" width="(\d+)" height="(\d+)" fill="(#[0-9a-fA-F]+)"`).FindAllStringSubmatch(svg, -1) {
		fill(atoi(r[1]), atoi(r[2]), atoi(r[3]), atoi(r[4]), hex(r[5]))
		rects++
	}
	for _, p := range regexp.MustCompile(`<path fill="(#[0-9a-fA-F]+)" d="([^"]*)"`).FindAllStringSubmatch(svg, -1) {
		for _, r := range regexp.MustCompile(`M(-?\d+) (-?\d+)h(\d+)v(\d+)h-\d+z`).FindAllStringSubmatch(p[2], -1) {
			fill(atoi(r[1]), atoi(r[2]), atoi(r[3]), atoi(r[4]), hex(p[1]))
			rects++
		}
	}
	require.Greater(t, rects, 10, "the icon was parsed")
	return n, g
}

// The checked-in favicon.ico is favicon.py's rendering of the landing's
// icon: decoded here, every PNG of the ICO compared pixel by pixel with the
// SVG. An icon edited without re-running the script fails.
func TestWebFiles_TheFaviconIsTheLandingsIcon(t *testing.T) {
	n, g := iconGrid(t)
	ico := faviconICO
	require.GreaterOrEqual(t, len(ico), 6)
	require.Equal(t, uint16(0), binary.LittleEndian.Uint16(ico[0:]), "ICO reserved")
	require.Equal(t, uint16(1), binary.LittleEndian.Uint16(ico[2:]), "ICO type: icon")
	count := int(binary.LittleEndian.Uint16(ico[4:]))
	require.Equal(t, 3, count)
	var sizes []int
	for i := 0; i < count; i++ {
		e := ico[6+16*i:]
		size := int(binary.LittleEndian.Uint32(e[8:]))
		off := int(binary.LittleEndian.Uint32(e[12:]))
		img, err := png.Decode(bytes.NewReader(ico[off : off+size]))
		require.NoError(t, err, "entry %d is a PNG", i)
		b := img.Bounds()
		require.Equal(t, b.Dx(), b.Dy())
		require.Zero(t, b.Dx()%n, "a whole-pixel scale of the %d-cell grid", n)
		assert.Equal(t, b.Dx()%256, int(e[0]), "the directory names the size")
		scale := b.Dx() / n
		sizes = append(sizes, b.Dx())
		for y := 0; y < b.Dy(); y++ {
			for x := 0; x < b.Dx(); x++ {
				got := color.NRGBAModel.Convert(img.At(x, y)).(color.NRGBA)
				require.Equal(t, g[y/scale][x/scale], got, "%d px at (%d,%d)", b.Dx(), x, y)
			}
		}
	}
	assert.Equal(t, []int{22, 44, 88}, sizes)
}
