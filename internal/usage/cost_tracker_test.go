package usage

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestCostTracker_NilDB(t *testing.T) {
	logger := zap.NewNop()
	ct := NewCostTracker(nil, logger)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() {
		ct.Start(ctx)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Start with nil DB should return immediately")
	}
}

func TestCostTracker_Aggregate(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	logger := zap.NewNop()
	ct := NewCostTracker(db, logger)

	mock.ExpectQuery("SELECT tenant_id, backend_name, SUM\\(size_bytes\\)").
		WillReturnRows(sqlmock.NewRows([]string{"tenant_id", "backend_name", "sum"}).
			AddRow("t1", "idrive", int64(1099511627776)). // 1 TB
			AddRow("t1", "geyser", int64(2199023255552)). // 2 TB
			AddRow("t2", "idrive", int64(549755813888)))  // 0.5 TB

	mock.ExpectExec("INSERT INTO tenant_cost_daily").
		WithArgs("t1", "idrive", int64(1099511627776), int64(3300000)).
		WillReturnResult(sqlmock.NewResult(0, 1))

	mock.ExpectExec("INSERT INTO tenant_cost_daily").
		WithArgs("t1", "geyser", int64(2199023255552), int64(3100000)).
		WillReturnResult(sqlmock.NewResult(0, 1))

	mock.ExpectExec("INSERT INTO tenant_cost_daily").
		WithArgs("t2", "idrive", int64(549755813888), int64(1650000)).
		WillReturnResult(sqlmock.NewResult(0, 1))

	ct.Aggregate(context.Background())

	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestComputeCostMicrocents(t *testing.T) {
	tests := []struct {
		name     string
		backend  string
		bytes    int64
		expected int64
	}{
		{"1 TB on idrive", "idrive", 1099511627776, 3300000},
		{"1 TB on geyser", "geyser", 1099511627776, 1550000},
		{"0 bytes", "idrive", 0, 0},
		{"free backend", "local", 1099511627776, 0},
		{"unknown backend", "unknown", 1099511627776, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ComputeCostMicrocents(tt.backend, tt.bytes)
			assert.Equal(t, tt.expected, got)
		})
	}
}
