package engine

import (
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
)

// An object that changed under a resumed read is the object's state: never a
// breaker charge (drivers/sia_read.go). An exhausted resume budget stays a
// failure (one charge per read, by the caller).
func TestIsBackendFailure_ObjectChangedIsNotAFailure(t *testing.T) {
	assert.False(t, isBackendFailure(fmt.Errorf("sia resume of t/c/k at 1024: %w", ErrObjectChanged)))
	assert.True(t, isBackendFailure(fmt.Errorf("sia read resume budget spent: %w", io.ErrUnexpectedEOF)))
}

type noHedgeDriver struct {
	mockDriver
	no bool
}

func (d *noHedgeDriver) NoHedge() bool { return d.no }

func TestIsNoHedge_OnlyADriverThatDeclaresIt(t *testing.T) {
	assert.False(t, IsNoHedge(&mockDriver{}), "no declaration: hedged")
	assert.False(t, IsNoHedge(&noHedgeDriver{}), "declares false: hedged")
	assert.True(t, IsNoHedge(&noHedgeDriver{no: true}))
	assert.False(t, errors.Is(ErrObjectChanged, ErrAllBackendsUnavailable), "never a 503 by itself")
}
