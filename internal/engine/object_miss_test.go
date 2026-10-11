package engine

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// Prompt 2b.5 F1.1: an error that says the backend could not be asked is
// never a miss, whatever the key or a bridge's body text says inside it.
// (The API's isObjectMissingErr refused ErrAllBackendsUnavailable first; the
// engine's twin read the text and called these misses.)
func TestIsObjectMiss_AnUnavailableVerdictIsNeverAMiss(t *testing.T) {
	cases := []string{
		// the webdav driver's text for a key named with NotFound, bridge down
		"sync get t-a/c/reports/NotFound.pdf: bridge 0 (the key's) failed (unexpected status 502 Bad Gateway: upstream unreachable): bridge down",
		// a 5xx body excerpt that happens to carry an SDK-ish token
		"sync put t-a/c/k: bridge 2 failed (unexpected status 502: upstream NotFound): bridge down",
		"local: open /data/c/NotFound/k: permission denied",
	}
	for _, text := range cases {
		for _, verdict := range []error{ErrAllBackendsUnavailable, ErrPartiallyUnavailable} {
			err := fmt.Errorf("%s: %w: %w", text, verdict, NotFoundError{Container: "c", Artifact: "k"})
			assert.False(t, IsObjectMiss(err), "%v is never a miss: %q", verdict, text)
		}
		err := fmt.Errorf("%s: %w", text, ErrAllBackendsUnavailable)
		if !strings.Contains(text, "permission denied") {
			assert.True(t, isBackendFailure(err), "an unavailable verdict is a failure: %q", text)
		}
	}
}
