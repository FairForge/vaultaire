package drivers

import (
	"context"
	"testing"

	"github.com/FairForge/vaultaire/internal/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestSiaConfigFromEnv_Defaults(t *testing.T) {
	endpoint, bucket := SiaConfigFromEnv(func(string) string { return "" })
	assert.Equal(t, "http://127.0.0.1:9000", endpoint)
	assert.Equal(t, "vaultaire", bucket)
}

func TestSiaConfigFromEnv_Overrides(t *testing.T) {
	env := map[string]string{
		"SIA_ENDPOINT": "http://127.0.0.1:9100",
		"SIA_BUCKET":   "stored",
	}
	endpoint, bucket := SiaConfigFromEnv(func(k string) string { return env[k] })
	assert.Equal(t, "http://127.0.0.1:9100", endpoint)
	assert.Equal(t, "stored", bucket)
}

func TestNewSiaDriver_NameAndKeyShape(t *testing.T) {
	d, err := NewSiaDriver("ak", "sk", "http://127.0.0.1:9000", "vaultaire", zap.NewNop())
	require.NoError(t, err)
	assert.Equal(t, "sia", d.Name())

	// The fixed-bucket key shape: t-<tenant>/<container>/<artifact>.
	ctx := common.WithTenantID(context.Background(), "acme")
	assert.Equal(t, "t-acme/photos/cat.jpg", d.ObjectKey(ctx, "photos", "cat.jpg"))
}
