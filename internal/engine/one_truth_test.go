package engine

// WP-R6-1 Part B: the engine keeps no record of where an object is (the
// row is the one truth), and the dormant `wasabi` backend is never a write
// fall-over destination nor read unless a row names it.

import (
	"bytes"
	"errors"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestObjectBackends_Bounded(t *testing.T) {
	// Before (origin/main 9f3d0c1): CoreEngine.objectBackends, a sync.Map
	// with one entry per object ever PUT, GET or hinted in the process
	// (R6-09: never trimmed but by Delete; prod held at least every chunk
	// stored since boot), and a *LocationStore writing object_locations on
	// every Put and reading it on a map miss (R6-14/15, R8-19).
	typ := reflect.TypeOf(CoreEngine{})
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		assert.NotEqual(t, reflect.TypeOf(sync.Map{}), f.Type, "field %s: no per-object route map", f.Name)
		assert.NotContains(t, f.Type.String(), "LocationStore", "field %s: no second source of truth", f.Name)
	}

	// 1,000 objects written and read: the engine holds nothing per object —
	// a read without the row's backend on its context asks the primary.
	e := NewEngine(nil, nopLogger(), &Config{DefaultBackend: "idrive"})
	idrive, lyve := newMemDriver("idrive"), newMemDriver("lyve")
	e.AddDriver("idrive", idrive)
	e.AddDriver("lyve", lyve)
	for i := 0; i < 1000; i++ {
		_, err := e.Put(tenantCtx(), "c", string(rune('a'+i%26))+strings.Repeat("x", i%7), bytes.NewReader([]byte("v")), WithStorageClass("RESILIENT"))
		require.NoError(t, err)
	}
	_, err := e.Get(tenantCtx(), "c", "a")
	assert.True(t, IsObjectMiss(err), "nothing remembered: the primary was asked (%v)", err)
	rc, err := e.Get(WithRecordedBackend(tenantCtx(), "lyve"), "c", "a")
	require.NoError(t, err)
	_ = rc.Close()
}

func TestWasabi_NotACandidateUnlessRecorded(t *testing.T) {
	// Before: wasabi was a general-purpose write candidate — a STANDARD PUT
	// whose primary failed fell over to it (a 90-day minimum per object on
	// a dormant partner account) — and a read fan-out asked it.
	e := NewEngine(nil, nopLogger(), &Config{DefaultBackend: "idrive"})
	idrive, lyve, wasabi := newMemDriver("idrive"), newMemDriver("lyve"), newMemDriver("wasabi")
	idrive.putErr = errors.New("dial tcp: connection refused")
	e.AddDriver("idrive", idrive)
	e.AddDriver("lyve", lyve)
	e.AddDriver("wasabi", wasabi)
	e.SetPrimary("idrive")

	assert.NotContains(t, e.buildWriteCandidateList("idrive"), "wasabi", "never a fall-over destination")
	used, err := e.Put(tenantCtx(), "c", "k", bytes.NewReader([]byte("v")))
	require.NoError(t, err)
	assert.Equal(t, "lyve", used)
	lyve.putErr = errors.New("dial tcp: connection refused")
	_, err = e.Put(tenantCtx(), "c", "k2", bytes.NewReader([]byte("v")))
	assert.ErrorIs(t, err, ErrAllBackendsUnavailable, "503 rather than a write to the dormant account")
	assert.Empty(t, wasabi.data, "nothing written to wasabi")

	// A row that names it stays readable.
	wasabi.data["c/old"] = []byte("written the day wasabi was the primary")
	rc, err := e.Get(WithRecordedBackend(tenantCtx(), "wasabi"), "c", "old")
	require.NoError(t, err)
	b, _ := io.ReadAll(rc)
	assert.Equal(t, "written the day wasabi was the primary", string(b))

	// And it can be the primary again (STORAGE_MODE=wasabi): every write is
	// its own.
	require.NoError(t, e.CheckPrimaryEligible("wasabi"))
	e.SetPrimary("wasabi")
	used, err = e.Put(tenantCtx(), "c", "k3", bytes.NewReader([]byte("v")))
	require.NoError(t, err)
	assert.Equal(t, "wasabi", used)
}
