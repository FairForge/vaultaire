package translog

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func validBuild() ServerBuild {
	return ServerBuild{GitSHA: "a2d388f9f1aa", BinarySHA256: strings.Repeat("ab", 32), BuildTime: "2026-10-22T06:32:16Z", GoVersion: "go1.26.10"}
}

func validCeremony() KeyCeremony {
	return KeyCeremony{Action: ActionEscrowSplit, KeyRole: KeyMasterKey, KeyFingerprint: "sha256:" + strings.Repeat("0f", 32),
		Participants: 1, Shares: 3, Threshold: 2, Reference: "https://github.com/FairForge/vaultaire/blob/" + strings.Repeat("ab", 20) + "/docs/ENCRYPTION.md"}
}

func validAdminRead() AdminRead {
	return AdminRead{Scope: ScopeTenantDetail, ActorRole: ActorRoleAdmin, Nonce: strings.Repeat("0a", 16), Commitment: strings.Repeat("c3", 32)}
}

// forbiddenFieldTokens: a field whose name has one of these words between
// underscores would be the start of a tenant, a person or an object in a
// public log.
var forbiddenFieldTokens = map[string]bool{
	"tenant": true, "tenants": true, "user": true, "users": true, "email": true, "mail": true, "ip": true,
	"addr": true, "address": true, "object": true, "bucket": true, "customer": true, "account": true,
	"name": true, "subject": true, "host": true, "agent": true, "path": true, "id": true,
}

// TestTranslog_EntriesNeverNameATenant: the log is public. Every kind is a
// typed struct whose JSON fields are exactly its allow-list, no allowed
// field is named after a person, tenant or object, every string a caller
// can choose is checked for what would name one, and an encoded body
// carries nothing else.
func TestTranslog_EntriesNeverNameATenant(t *testing.T) {
	samples := map[string]Entry{KindServerBuild: validBuild(), KindKeyCeremony: validCeremony(), KindAdminRead: validAdminRead()}
	require.ElementsMatch(t, Kinds(), keys(samples), "every kind has a sample here")

	for kind, e := range samples {
		assert.Equal(t, kind, e.Kind())
		// The struct's JSON fields are the allow-list, exactly.
		var got []string
		rt := reflect.TypeOf(e)
		for i := 0; i < rt.NumField(); i++ {
			tag := strings.Split(rt.Field(i).Tag.Get("json"), ",")[0]
			require.NotEmpty(t, tag, "%s.%s has no json name", kind, rt.Field(i).Name)
			got = append(got, tag)
		}
		assert.ElementsMatch(t, AllowedFields(kind), got, "%s: fields vs allow-list", kind)
		for _, f := range got {
			for _, tok := range strings.Split(f, "_") {
				assert.False(t, forbiddenFieldTokens[tok], "%s.%s names a person, tenant or object", kind, f)
			}
		}

		// The encoded body: kind, v, at, entry — and the entry's keys are the allow-list.
		body, err := Encode(e, time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC))
		require.NoError(t, err, kind)
		var top map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(body, &top))
		assert.ElementsMatch(t, []string{"kind", "v", "at", "entry"}, keys(top))
		var inner map[string]any
		require.NoError(t, json.Unmarshal(top["entry"], &inner))
		assert.ElementsMatch(t, AllowedFields(kind), keys(inner))
	}

	// Every string field refuses what would identify a tenant or a person.
	identifying := []string{
		"alice@example.com", "203.0.113.7", "2001:db8::1", "fe80::1%eth0",
		"7c9e6679-7425-40de-944b-e07fc1f90ae7", "tenant-7c9e6679", "t-acme/photos/2026/img.jpg",
	}
	for kind, e := range samples {
		rv := reflect.ValueOf(e)
		for i := 0; i < rv.NumField(); i++ {
			if rv.Field(i).Kind() != reflect.String {
				continue
			}
			for _, bad := range identifying {
				cp := reflect.New(rv.Type()).Elem()
				cp.Set(rv)
				cp.Field(i).SetString(bad)
				_, err := Encode(cp.Interface().(Entry), time.Now())
				assert.Error(t, err, "%s.%s accepted %q", kind, rv.Type().Field(i).Name, bad)
			}
		}
	}
}

func TestTranslog_EntryValidation(t *testing.T) {
	b := validBuild()
	b.GitSHA = "dev"
	_, err := Encode(b, time.Now())
	assert.NoError(t, err, "an unstamped build is still a build")
	b.BinarySHA256 = "abc"
	_, err = Encode(b, time.Now())
	assert.Error(t, err)

	c := validCeremony()
	c.Action = "whatever"
	_, err = Encode(c, time.Now())
	assert.Error(t, err, "actions are an enum")
	c = validCeremony()
	c.KeyRole = "another_key"
	_, err = Encode(c, time.Now())
	assert.Error(t, err, "key roles are an enum")
	c = validCeremony()
	c.KeyFingerprint = strings.Repeat("0f", 32)
	_, err = Encode(c, time.Now())
	assert.Error(t, err, "a fingerprint says its hash")
	c = validCeremony()
	c.Participants = 0
	_, err = Encode(c, time.Now())
	assert.Error(t, err)
	for name, mutate := range map[string]func(*KeyCeremony){
		"escrow_split without shares": func(c *KeyCeremony) { c.Shares, c.Threshold = 0, 0 },
		"one share":                   func(c *KeyCeremony) { c.Shares, c.Threshold = 1, 1 },
		"threshold over shares":       func(c *KeyCeremony) { c.Threshold = 4 },
		"threshold 0 with shares":     func(c *KeyCeremony) { c.Threshold = 0 },
		"sizes outside log_restored":  func(c *KeyCeremony) { c.SignedSize = 10 },
		"new_origin outside":          func(c *KeyCeremony) { c.NewOrigin = true },
		"another host":                func(c *KeyCeremony) { c.Reference = "https://example.com/x" },
		"a short commit":              func(c *KeyCeremony) { c.Reference = "https://github.com/FairForge/vaultaire/commit/abcdef1" },
		"a docs anchor":               func(c *KeyCeremony) { c.Reference += "#jane-doe" },
		"log_restored of the master key": func(c *KeyCeremony) {
			c.Action, c.Shares, c.Threshold, c.SignedSize, c.RestoredSize = ActionLogRestored, 0, 0, 10, 4
		},
		"log_restored not behind": func(c *KeyCeremony) {
			c.Action, c.KeyRole, c.Shares, c.Threshold, c.SignedSize, c.RestoredSize = ActionLogRestored, KeyTranslogSigningKey, 0, 0, 10, 10
		},
	} {
		c := validCeremony()
		mutate(&c)
		_, err := Encode(c, time.Now())
		assert.Error(t, err, name)
	}
	restored := KeyCeremony{Action: ActionLogRestored, KeyRole: KeyTranslogSigningKey, KeyFingerprint: "sha256:" + strings.Repeat("0f", 32),
		Participants: 1, SignedSize: 120, RestoredSize: 96, Reference: "https://github.com/FairForge/vaultaire/pull/912"}
	_, err = Encode(restored, time.Now())
	assert.NoError(t, err, "a log_restored entry")

	assert.Equal(t, "server_build:a2d388f9f1aa:"+strings.Repeat("ab", 32), validBuild().DedupKey())
	assert.Empty(t, validCeremony().DedupKey(), "every ceremony is its own entry")
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestCeremonyEntry_RefusesFreeText (RP37-10): the review's three strings —
// a name with an address, an obfuscated e-mail, a bare tenant id — cannot be
// expressed in a key_ceremony entry: not in any field, not as the removed
// "statement", not appended to a reference.
func TestCeremonyEntry_RefusesFreeText(t *testing.T) {
	freeText := []string{
		"Installed by Isaac Viera with Jane Doe of 12 Main Street",
		"Installed by isaacv17 at gmail dot com",
		"4e1b43062db9d685",
	}
	base := validCeremony()
	rv := reflect.ValueOf(base)
	for _, s := range freeText {
		for i := 0; i < rv.NumField(); i++ {
			if rv.Field(i).Kind() != reflect.String {
				continue
			}
			for _, v := range []string{s, rv.Field(i).String() + s, rv.Field(i).String() + "/" + s} {
				cp := reflect.New(rv.Type()).Elem()
				cp.Set(rv)
				cp.Field(i).SetString(v)
				_, err := Encode(cp.Interface().(Entry), time.Now())
				assert.Error(t, err, "%s accepted %q", rv.Type().Field(i).Name, v)
			}
		}
		raw, err := json.Marshal(base)
		require.NoError(t, err)
		withStatement := append(raw[:len(raw)-1:len(raw)-1], []byte(`,"statement":`+strconvQuote(s)+`}`)...)
		_, err = ParseKeyCeremony(withStatement)
		assert.Error(t, err, "statement %q", s)
		body, err := Encode(base, time.Now())
		require.NoError(t, err)
		forged := strings.Replace(string(body), `"reference":`, `"statement":`+strconvQuote(s)+`,"reference":`, 1)
		_, err = Decode([]byte(forged))
		assert.Error(t, err, "a body carrying a statement does not decode")
	}
	_, err := ParseKeyCeremony(mustJSON(t, base))
	assert.NoError(t, err, "the valid entry parses")
}

func strconvQuote(s string) string { b, _ := json.Marshal(s); return string(b) }

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return b
}
