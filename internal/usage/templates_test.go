// internal/usage/templates_test.go
package usage

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestQuotaTemplates_GetTemplate(t *testing.T) {
	templates := NewQuotaTemplates()

	tests := []struct {
		name     string
		tier     string
		wantErr  bool
		expected QuotaTemplate
	}{
		{
			name:    "starter tier",
			tier:    "starter",
			wantErr: false,
			expected: QuotaTemplate{
				Name:           "starter",
				StorageLimit:   1073741824,  // 1GB
				BandwidthLimit: 10737418240, // 10GB
				MaxObjects:     10000,
				MaxBuckets:     3,
			},
		},
		{
			name:    "professional tier",
			tier:    "professional",
			wantErr: false,
			expected: QuotaTemplate{
				Name:           "professional",
				StorageLimit:   107374182400,  // 100GB
				BandwidthLimit: 1073741824000, // 1TB
				MaxObjects:     1000000,
				MaxBuckets:     50,
			},
		},
		{
			name:    "enterprise tier",
			tier:    "enterprise",
			wantErr: false,
			expected: QuotaTemplate{
				Name:           "enterprise",
				StorageLimit:   10737418240000,  // 10TB
				BandwidthLimit: 107374182400000, // 100TB
				MaxObjects:     -1,              // unlimited
				MaxBuckets:     -1,              // unlimited
			},
		},
		{
			name:    "unknown tier",
			tier:    "unknown",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			template, err := templates.GetTemplate(tt.tier)

			if tt.wantErr {
				assert.Error(t, err)
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.expected.Name, template.Name)
			assert.Equal(t, tt.expected.StorageLimit, template.StorageLimit)
			assert.Equal(t, tt.expected.BandwidthLimit, template.BandwidthLimit)
		})
	}
}

func TestQuotaTemplates_ApplyTemplate(t *testing.T) {
	db := setupTestDB(t)
	defer func() { _ = db.Close() }()

	qm := NewQuotaManager(db)
	require.NoError(t, qm.InitializeSchema(context.Background()))

	templates := NewQuotaTemplates()

	// Apply professional template to tenant
	err := templates.ApplyTemplate(context.Background(), qm, "tenant-123", "professional")
	require.NoError(t, err)

	// Verify quota was set correctly
	used, limit, err := qm.GetUsage(context.Background(), "tenant-123")
	require.NoError(t, err)
	assert.Equal(t, int64(0), used)
	assert.Equal(t, int64(107374182400), limit) // 100GB
}
