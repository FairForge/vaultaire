package engine

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestMigrator_MigrateObject(t *testing.T) {
	migrator := NewMigrator()
	source := &TestDriver{}
	dest := &TestDriver{}

	// Simulate migration
	err := migrator.MigrateObject(context.Background(),
		source, dest, "container", "object")

	assert.NoError(t, err)
	assert.Equal(t, int32(1), atomic.LoadInt32(&dest.putCalls))
}

func TestMigrator_BulkMigration(t *testing.T) {
	migrator := NewMigrator()
	source := &TestDriver{}
	dest := &TestDriver{}

	// Migrate entire container
	stats, err := migrator.MigrateContainer(context.Background(),
		source, dest, "container", MigrationOptions{
			Workers:    5,
			VerifyMode: true,
		})

	assert.NoError(t, err)
	assert.Greater(t, stats.ObjectsProcessed, 0)
	assert.Equal(t, stats.Failed, 0)
}
