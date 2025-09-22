// +build !integration

package api

import "testing"

func skipTestIfNoDB(t *testing.T) {
	t.Skip("Skipping database test in unit test mode")
}
