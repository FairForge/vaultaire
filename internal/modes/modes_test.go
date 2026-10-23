package modes

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/api/landing"
	"github.com/FairForge/vaultaire/internal/testutil"
	_ "github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// forbidden are words no mode sentence may say (contract line 8: "zero-
// knowledge" waits for an external audit; line 1: Vault is a tier).
var forbidden = []string{"zero-knowledge", "zero knowledge", "zero‑knowledge", "zeroknowledge"}

func TestModesJSON_ThreeModesNoForbiddenWords(t *testing.T) {
	f := All()

	// Exactly the three modes, in the contract's order.
	require.Len(t, f.Modes, 3)
	assert.Equal(t, []string{Private, Standard, Public}, []string{f.Modes[0].ID, f.Modes[1].ID, f.Modes[2].ID})
	for _, m := range f.Modes {
		assert.NotEmpty(t, m.Name, m.ID)
		assert.False(t, strings.EqualFold(m.Name, "vault") || strings.Contains(strings.ToLower(m.ID), "vault"), "Vault is a tier, never a mode")
		for field, s := range map[string]Sentence{"server_holds": m.ServerHolds, "customer": m.Customer, "how": m.How} {
			assert.NotEmpty(t, s.Text, "%s.%s", m.ID, field)
			if s.RequiresMasterKey {
				assert.NotEmpty(t, s.WithoutMasterKey, "%s.%s is true only with the key: it needs its pre-key sentence", m.ID, field)
			}
		}
	}
	// Every surface names a default and only known modes.
	for _, name := range []string{"developer", "consumer", "public"} {
		s, ok := f.Surfaces[name]
		require.True(t, ok, name)
		_, known := Get(s.Default)
		assert.True(t, known, "%s default %q", name, s.Default)
	}
	assert.Equal(t, Standard, f.Surfaces["developer"].Default)
	assert.Equal(t, Standard, f.Surfaces["consumer"].Default, "recoverable until F-19")
	assert.Equal(t, Public, f.Surfaces["public"].Default)

	// No forbidden word anywhere in the file — the raw bytes, so a key or a
	// note cannot carry one either.
	raw := strings.ToLower(string(landing.ModesJSON()))
	for _, w := range forbidden {
		assert.NotContains(t, raw, w)
	}
	// "keys we hold" is a with-key sentence only.
	for _, m := range f.Modes {
		for _, s := range []Sentence{m.ServerHolds, m.Customer, m.How} {
			if strings.Contains(strings.ToLower(s.Text), "keys we hold") {
				assert.True(t, s.RequiresMasterKey, "%q is true only while the master key is loaded", s.Text)
			}
			assert.NotContains(t, strings.ToLower(s.WithoutMasterKey), "keys we hold")
		}
	}
}

func TestValidate_RefusesWhatTheTestRefuses(t *testing.T) {
	good := All()
	cases := map[string]func(f *File){
		"two modes":       func(f *File) { f.Modes = f.Modes[:2] },
		"empty name":      func(f *File) { f.Modes[0].Name = "" },
		"vault as a mode": func(f *File) { f.Modes[1].Name = "Vault" },
		"zero-knowledge":  func(f *File) { f.Modes[0].Customer.Text = "Zero-knowledge storage." },
		"no pre-key text": func(f *File) { f.Modes[1].Customer.WithoutMasterKey = "" },
		"unknown default": func(f *File) { f.Surfaces["developer"] = Surface{Default: "secret"} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			b, err := json.Marshal(good)
			require.NoError(t, err)
			var f File
			require.NoError(t, json.Unmarshal(b, &f))
			mutate(&f)
			assert.Error(t, Validate(f))
		})
	}
	assert.NoError(t, Validate(good))
}

func TestSentence_RenderFollowsTheMasterKey(t *testing.T) {
	std, ok := Get(Standard)
	require.True(t, ok)
	assert.Contains(t, std.Customer.Render(true), "keys we hold")
	assert.NotContains(t, std.Customer.Render(false), "keys we hold")
	assert.Equal(t, std.Customer.WithoutMasterKey, std.Customer.Render(false))
	pub, _ := Get(Public)
	assert.Equal(t, pub.Customer.Text, pub.Customer.Render(false), "a sentence true either way renders the same")
}

// ── the column (migration 102) and the one write door ─────────────────────

type dbFixture struct {
	db     *sql.DB
	tenant string
}

func newDB(t *testing.T) *dbFixture {
	t.Helper()
	db, err := sql.Open("postgres", testutil.DSN())
	require.NoError(t, err)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		t.Skipf("test database unreachable (%v) — create it with `make test-db`", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	tenant := fmt.Sprintf("modes-%d-%d", time.Now().UnixNano(), rand.Intn(1_000_000))
	_, err = db.Exec(`INSERT INTO tenants (id, name, email) VALUES ($1, 'Modes', $2)`, tenant, tenant+"@test.local")
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM buckets WHERE tenant_id = $1`, tenant)
		_, _ = db.Exec(`DELETE FROM tenants WHERE id = $1`, tenant)
	})
	return &dbFixture{db: db, tenant: tenant}
}

func (f *dbFixture) bucket(t *testing.T, name string) {
	t.Helper()
	_, err := f.db.Exec(`INSERT INTO buckets (tenant_id, name, visibility) VALUES ($1, $2, 'private')`, f.tenant, name)
	require.NoError(t, err)
}

func (f *dbFixture) columns(t *testing.T, name string) (visibility string, ssec bool, mode string) {
	t.Helper()
	require.NoError(t, f.db.QueryRow(`SELECT visibility, ssec_required, mode FROM buckets WHERE tenant_id = $1 AND name = $2`,
		f.tenant, name).Scan(&visibility, &ssec, &mode))
	return
}

func ptr[T any](v T) *T { return &v }

func TestBucketMode_DerivedFromVisibilityAndSSEC(t *testing.T) {
	f := newDB(t)
	ctx := context.Background()
	cases := []struct {
		visibility string
		ssec       bool
		want       string // "" = refused
	}{
		{VisibilityPrivate, false, Standard}, // the name trap: 'private' visibility is Standard
		{VisibilityPrivate, true, Private},
		{VisibilityPublic, false, Public},
		{VisibilityPublic, true, ""}, // a public bucket cannot require a key (096)
	}
	for _, c := range cases {
		t.Run(fmt.Sprintf("%s/ssec=%v", c.visibility, c.ssec), func(t *testing.T) {
			name := fmt.Sprintf("b-%s-%v", strings.ReplaceAll(c.visibility, "-", ""), c.ssec)
			f.bucket(t, name)

			got, err := SetBucket(ctx, f.db, f.tenant, name, Change{Visibility: ptr(c.visibility), SSECRequired: ptr(c.ssec)})

			if c.want == "" {
				assert.True(t, errors.Is(err, ErrPublicSSEC), "got %v", err)
				vis, ssec, mode := f.columns(t, name)
				assert.Equal(t, VisibilityPrivate, vis, "nothing written")
				assert.False(t, ssec)
				assert.Equal(t, Standard, mode)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, c.want, got)
			_, _, mode := f.columns(t, name)
			assert.Equal(t, c.want, mode, "the generated column")
			assert.Equal(t, c.want, FromColumns(c.visibility, c.ssec), "Go agrees with migration 102")
		})
	}
}

func TestSetBucket_PartialChange_RuleSeesTheOtherColumn(t *testing.T) {
	f := newDB(t)
	ctx := context.Background()
	f.bucket(t, "pub")
	_, err := SetBucket(ctx, f.db, f.tenant, "pub", Change{Visibility: ptr(VisibilityPublic)})
	require.NoError(t, err)

	// Requiring a key on the public bucket (the management PATCH sends only
	// ssec_required) is refused; making it Standard first then allows it.
	_, err = SetBucket(ctx, f.db, f.tenant, "pub", Change{SSECRequired: ptr(true)})
	assert.ErrorIs(t, err, ErrPublicSSEC)
	got, err := SetBucket(ctx, f.db, f.tenant, "pub", Change{Visibility: ptr(VisibilityPrivate), SSECRequired: ptr(true)})
	require.NoError(t, err)
	assert.Equal(t, Private, got)
}

func TestSetBucket_Refusals(t *testing.T) {
	f := newDB(t)
	ctx := context.Background()
	_, err := SetBucket(ctx, f.db, f.tenant, "nope", Change{SSECRequired: ptr(true)})
	assert.ErrorIs(t, err, ErrNoBucket)
	f.bucket(t, "b")
	_, err = SetBucket(ctx, f.db, f.tenant, "b", Change{Visibility: ptr("authenticated-read")})
	assert.ErrorIs(t, err, ErrBadVisibility)
	_, err = SetBucket(ctx, f.db, "someone-else", "b", Change{Visibility: ptr(VisibilityPublic)})
	assert.ErrorIs(t, err, ErrNoBucket, "another tenant's bucket is not found")
}

// TestModesJSON_NoUnqualifiedCannotRead is RP37-4: a mode sentence may say
// we cannot read (or decrypt) only together with "at rest" or a client-side
// clause. SSE-C, the developer surface's Private, sends the key with every
// request and we decrypt to serve the GET: "can't read at rest", never
// "can't read" (the owner's rule of 2026-10-14; contract line 8).
func TestModesJSON_NoUnqualifiedCannotRead(t *testing.T) {
	// Arrange
	f := All()
	var texts []string
	for _, m := range f.Modes {
		for _, s := range []Sentence{m.ServerHolds, m.Customer, m.How} {
			texts = append(texts, s.Text, s.WithoutMasterKey)
		}
	}
	for _, s := range f.Surfaces {
		texts = append(texts, s.Note)
	}

	// Act
	var bad []string
	for _, text := range texts {
		bad = append(bad, UnqualifiedCannotRead(text)...)
	}

	// Assert
	assert.Empty(t, bad, "a 'cannot read' with neither 'at rest' nor a client-side clause")
	// The rule itself: what it refuses and what it lets through.
	assert.NotEmpty(t, UnqualifiedCannotRead("We store your files encrypted and cannot read them."))
	assert.NotEmpty(t, UnqualifiedCannotRead("Your files as ciphertext we can't decrypt."))
	assert.Empty(t, UnqualifiedCannotRead("We cannot read them at rest."))
	assert.Empty(t, UnqualifiedCannotRead("Encrypted on your device, so we cannot read them."))
	require.Error(t, Validate(File{Modes: []Mode{
		{ID: Private, Name: "P", ServerHolds: Sentence{Text: "x"}, Customer: Sentence{Text: "We cannot read them."}, How: Sentence{Text: "x"}},
		{ID: Standard, Name: "S", ServerHolds: Sentence{Text: "x"}, Customer: Sentence{Text: "x"}, How: Sentence{Text: "x"}},
		{ID: Public, Name: "U", ServerHolds: Sentence{Text: "x"}, Customer: Sentence{Text: "x"}, How: Sentence{Text: "x"}},
	}}), "Validate refuses an unqualified 'cannot read'")
}

// TestModesJSON_DeletedBytesSentence is RP37-8: the privacy page's retention
// sentence covers the bytes of deleted files — the collector's day on disk
// providers, and never freed on tape.
func TestModesJSON_DeletedBytesSentence(t *testing.T) {
	text := strings.ToLower(All().DeletedBytes.Text)
	require.NotEmpty(t, text)
	assert.Contains(t, text, "up to a day")
	assert.Contains(t, text, "tape")
}
