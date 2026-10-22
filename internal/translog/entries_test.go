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
	return KeyCeremony{Ceremony: CeremonyMasterKeyInstall, KeyFingerprint: "sha256:" + strings.Repeat("0f", 32), Participants: 1,
		Statement: "F-14 install: master key generated on the box, sealed with systemd-creds (TPM), 2-of-3 escrow shares written."}
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
	samples := map[string]Entry{KindServerBuild: validBuild(), KindKeyCeremony: validCeremony()}
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
			// Free text: also inside a sentence.
			if rv.Type().Field(i).Name == "Statement" {
				for _, bad := range identifying {
					cp := validCeremony()
					cp.Statement = "Installed for " + bad + " today."
					_, err := Encode(cp, time.Now())
					assert.Error(t, err, "Statement accepted %q inside a sentence", bad)
				}
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
	c.Ceremony = "whatever"
	_, err = Encode(c, time.Now())
	assert.Error(t, err, "ceremonies are an enum")
	c = validCeremony()
	c.KeyFingerprint = strings.Repeat("0f", 32)
	_, err = Encode(c, time.Now())
	assert.Error(t, err, "a fingerprint says its hash")
	c = validCeremony()
	c.Participants = 0
	_, err = Encode(c, time.Now())
	assert.Error(t, err)
	c = validCeremony()
	c.Statement = strings.Repeat("x", 2001)
	_, err = Encode(c, time.Now())
	assert.Error(t, err)
	c = validCeremony()
	c.Statement = "line\x00"
	_, err = Encode(c, time.Now())
	assert.Error(t, err)

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
