// internal/apikeys/generator_test.go
package apikeys

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestKeyGenerator_Generate(t *testing.T) {
	t.Run("generates unique API keys", func(t *testing.T) {
		generator := NewKeyGenerator()

		key1, err := generator.Generate()
		require.NoError(t, err)
		assert.NotEmpty(t, key1.AccessKey)
		assert.NotEmpty(t, key1.SecretKey)
		assert.NotEmpty(t, key1.ID)

		// Keys should be unique
		key2, err := generator.Generate()
		require.NoError(t, err)
		assert.NotEqual(t, key1.AccessKey, key2.AccessKey)
		assert.NotEqual(t, key1.SecretKey, key2.SecretKey)
		assert.NotEqual(t, key1.ID, key2.ID)
	})

	t.Run("generates keys with correct format", func(t *testing.T) {
		generator := NewKeyGenerator()

		key, err := generator.Generate()
		require.NoError(t, err)

		// Access key format: VLT_XXXXXXXXXXXXXXXXXXXX
		assert.Regexp(t, `^VLT_[A-Z0-9]{20}$`, key.AccessKey)

		// Secret key: 40 character base64url
		assert.Len(t, key.SecretKey, 40)

		// ID is UUID
		assert.Regexp(t, `^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$`, key.ID)
	})
}
