package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/go-chi/chi/v5"
	"github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FairForge/vaultaire/internal/dashboard/handlers"
)

// Stage 1a Prompt 41 item 5 (RP57-1, a one-way door): a bucket named after
// a web path — legal, docs, login, dashboard, robots.txt, favicon.ico … —
// is refused at CreateBucket on every surface (S3 400 InvalidBucketName
// naming the reservation, the management API's invalid_bucket_name, the
// dashboard's form), at create ONLY: a bucket that exists under such a name
// keeps working. The list is derived from the router, here as in the
// product: a new page is reserved the day it is mounted.

// webPathNames derives the reserved names the way bucket_reserved.go does,
// independently: every route's first segment, a valid bucket name, from
// chi.Walk — never a typed list.
func webPathNames(t *testing.T, srv *Server) []string {
	t.Helper()
	seen := map[string]bool{}
	require.NoError(t, chi.Walk(srv.router, func(_ string, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		seg := strings.ToLower(strings.TrimSuffix(strings.SplitN(strings.TrimPrefix(route, "/"), "/", 2)[0], "*"))
		if validateBucketName(seg) {
			seen[seg] = true
		}
		return nil
	}))
	var out []string
	for n := range seen {
		out = append(out, n)
	}
	require.Greater(t, len(out), 20, "the walk found the web paths")
	for _, must := range []string{"api", "docs", "legal", "status", "login", "register", "changelog", "abuse", "dashboard", "auth", "cdn", "translog", "health", "ready", "version", "metrics", "llms.txt", "robots.txt", "sitemap.xml", "favicon.ico", "og.png", "openapi.json", "static"} {
		assert.Contains(t, out, must, "the review's list is a subset of the derived one")
	}
	return out
}

// withProductionRouter gives the SigV4 fixture's bare server the production
// router (NewServer's), the one the reserved list is derived from.
func (f *bypassFixture) withProductionRouter(t *testing.T) {
	t.Helper()
	real, _ := gateServer(t, prodTodayHosts)
	f.srv.router = real.router
}

func TestReservedBucketNames_EveryWebPathIsRefusedAtCreateOnEverySurface(t *testing.T) {
	f := setupBypassFixture(t)
	f.withProductionRouter(t)
	c, ctx := f.root(), context.Background()
	names := webPathNames(t, f.srv)
	assert.ElementsMatch(t, names, f.srv.reservedBucketNameList(), "the product's list IS the router's")

	for _, name := range names {
		// S3, through real SigV4.
		_, err := c.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(name)})
		if !assert.Error(t, err, "CreateBucket %q", name) {
			_, _ = c.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: aws.String(name)})
			continue
		}
		code, s3code := s3Status(t, err)
		assert.Equal(t, http.StatusBadRequest, code, name)
		assert.Equal(t, "InvalidBucketName", s3code, name)
		assert.Contains(t, err.Error(), "is reserved: it is a path of this service's web host", name)
		// A case variant is the same name (the router is lower-case; a
		// bucket name is lower-case by the validator anyway).
		_, err = c.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(name)})
		assert.Error(t, err, "%q: nothing was created", name)

		// The management API.
		w := f.mgmtCreate(t, name)
		assert.Equal(t, http.StatusBadRequest, w.Code, "management create %q: %s", name, w.Body.String())
		var env struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
				Param   string `json:"param"`
			} `json:"error"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &env), w.Body.String())
		assert.Equal(t, "invalid_bucket_name", env.Error.Code, name)
		assert.Contains(t, env.Error.Message, "reserved", name)
		assert.Equal(t, "name", env.Error.Param)

		// The dashboard's creator.
		res, err := f.srv.dashboardBucketCreator()(ctx, f.tenantID, name, "")
		require.NoError(t, err, name)
		assert.Equal(t, handlers.BucketCreateReservedName, res.State, "dashboard create %q", name)
	}
	var n int
	require.NoError(t, f.db.QueryRow(`SELECT count(*) FROM buckets WHERE tenant_id = $1 AND name = ANY($2::text[])`, f.tenantID, pq.Array(names)).Scan(&n))
	assert.Equal(t, 0, n, "no row was written for any reserved name")
}

// mgmtCreate posts the management API's create with the fixture's tenant.
func (f *bypassFixture) mgmtCreate(t *testing.T, name string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/manage/buckets", strings.NewReader(fmt.Sprintf(`{"name":%q}`, name)))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(context.WithValue(req.Context(), tenantIDKey, f.tenantID))
	w := httptest.NewRecorder()
	f.srv.handleMgmtCreateBucket(w, req)
	return w
}

// At create only: a bucket that exists under a reserved name (prod has
// none — the note records the SQL read) keeps every operation, including
// the idempotent re-create and its deletion.
func TestReservedBucketNames_AnExistingBucketKeepsWorking(t *testing.T) {
	f := setupBypassFixture(t)
	f.withProductionRouter(t)
	c, ctx := f.root(), context.Background()
	const name = "docs"
	require.True(t, f.srv.reservedBucketName(name))
	f.exec(`INSERT INTO buckets (tenant_id, name, region) VALUES ($1, $2, 'us-central-1')`, f.tenantID, name)
	t.Cleanup(func() { f.exec(`DELETE FROM buckets WHERE tenant_id = $1 AND name = $2`, f.tenantID, name) })

	_, err := c.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(name)})
	require.NoError(t, err, "HeadBucket")
	_, err = c.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(name)})
	assert.NoError(t, err, "the idempotent re-create of an owned bucket is not refused")
	_, err = c.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(name), Key: aws.String("k"), Body: strings.NewReader("v")})
	require.NoError(t, err, "PutObject")
	out, err := c.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(name), Key: aws.String("k")})
	require.NoError(t, err, "GetObject")
	_ = out.Body.Close()
	_, err = c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(name), Key: aws.String("k")})
	require.NoError(t, err, "DeleteObject")
	_, err = c.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: aws.String(name)})
	assert.NoError(t, err, "DeleteBucket")

	// And a name that is no web path is created as before.
	other := "p41-not-a-page-" + f.tenantID[:8]
	_, err = c.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(other)})
	require.NoError(t, err)
	_, err = c.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: aws.String(other)})
	require.NoError(t, err)
}
