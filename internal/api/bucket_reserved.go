package api

import (
	"net/http"
	"sort"
	"strings"
	"sync"

	"github.com/go-chi/chi/v5"
)

// Reserved bucket names (Stage 1a Prompt 41 item 5, RP57-1 — a one-way
// door). The web host serves pages at the root — /docs, /legal/*, /login,
// /status, /robots.txt … — and the S3 API serves buckets there too. Since
// Prompt 40 a request carrying S3 credentials is the S3 API's on every host
// (Architecture Decision 8), so a signed GetObject of bucket "legal" no
// longer gets the privacy page; what was left was the reverse shadow: a
// bucket NAMED after a page (RP55-3, RP57-5's residue — an edge that cached
// /favicon.ico by extension served the file to a signed GET of bucket
// favicon.ico). The names are refused at CREATE only: a bucket that
// already exists under one keeps working (GET/HEAD/DELETE unchanged,
// idempotent re-create too). AWS reserves nothing; docs/API.md states the
// deviation.
//
// The list is DERIVED from the router, never typed: every first path
// segment of a route registered ahead of the S3 catch-all (the web pages,
// the dashboard, the API prefixes, the web files), whatever its method — a
// new page is reserved the day it is mounted. Only a segment that is a
// valid bucket name could ever be a bucket, so the rest are kept out of
// the list (.well-known cannot be a bucket).
type reservedBucketNames struct {
	once  sync.Once
	names map[string]bool
}

// reservedBucketName reports whether name is a web path's first segment.
func (s *Server) reservedBucketName(name string) bool {
	s.reservedNames.once.Do(func() {
		s.reservedNames.names = routeFirstSegments(s.router)
	})
	return s.reservedNames.names[strings.ToLower(name)]
}

// reservedBucketNameList is the sorted list (docs, tests, the refusal's
// message).
func (s *Server) reservedBucketNameList() []string {
	s.reservedBucketName("")
	out := make([]string, 0, len(s.reservedNames.names))
	for n := range s.reservedNames.names {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// routeFirstSegments walks the router and collects the first path segment
// of every route but the catch-all ("/*") and the root ("/"), lower-cased,
// keeping those a bucket could be named.
func routeFirstSegments(r chi.Router) map[string]bool {
	names := map[string]bool{}
	if r == nil {
		return names
	}
	_ = chi.Walk(r, func(_ string, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		seg := strings.SplitN(strings.TrimPrefix(route, "/"), "/", 2)[0]
		seg = strings.ToLower(strings.TrimSuffix(seg, "*"))
		if seg == "" || strings.HasPrefix(seg, "{") || !validateBucketName(seg) {
			return nil
		}
		names[seg] = true
		return nil
	})
	return names
}

// reservedBucketNameMessage is the refusal's text on every surface.
func reservedBucketNameMessage(name string) string {
	return "The bucket name " + name + " is reserved: it is a path of this service's web host (docs/API.md § Reserved bucket names). Choose another name."
}
