package dashboard

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// WP-DX-1: the overview's "Test data" card — shown once the account has a
// sandbox: its objects and bytes, its live test keys, the 7-day purge.
func TestOverview_TestDataCard(t *testing.T) {
	with := renderOverview(t, map[string]any{"Page": "dashboard",
		"TestData": map[string]any{"Objects": 3, "BytesFmt": "12 KB", "Keys": 1}})
	assert.Contains(t, with, "Test data")
	assert.Contains(t, with, "12 KB")
	assert.Contains(t, with, "7 days")
	assert.Contains(t, with, "VLT_TEST_")
	without := renderOverview(t, map[string]any{"Page": "dashboard"})
	assert.NotContains(t, without, "Test data")
}
