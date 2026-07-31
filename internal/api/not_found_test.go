package api

import (
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/stretchr/testify/assert"
)

func TestIsObjectMissingErr(t *testing.T) {
	tests := []struct {
		name    string
		err     error
		missing bool
	}{
		{"nil", nil, false},
		{
			"typed engine NotFoundError, wrapped",
			fmt.Errorf("get t1_b/k: %w", engine.ErrNotFound("t1_b", "k")),
			true,
		},
		{
			"os.ErrNotExist, wrapped",
			fmt.Errorf("get local: %w", os.ErrNotExist),
			true,
		},
		{
			"local driver path error text",
			errors.New("open /tmp/vaultaire-data/t1_b/k: no such file or directory"),
			true,
		},
		{
			"aws-sdk GetObject miss (iDrive/Lyve/S3 drivers)",
			errors.New("get t1_b/k: idrive get t1/b/k: operation error S3: GetObject, https response error StatusCode: 404, RequestID: X, HostID: Y, api error NoSuchKey: The specified key does not exist."),
			true,
		},
		{
			"aws-sdk HeadObject miss",
			errors.New("get t1_b/k: api error NotFound: Not Found"),
			true,
		},
		{
			"bare 404 status text",
			errors.New("unexpected status code: 404"),
			true,
		},
		{
			"connection refused is a backend failure",
			errors.New("get t1_b/k: dial tcp 1.2.3.4:443: connect: connection refused"),
			false,
		},
		{
			"timeout is a backend failure",
			errors.New("get t1_b/k: dial tcp: i/o timeout"),
			false,
		},
		{
			"all backends unavailable is a backend failure",
			fmt.Errorf("get t1_b/k: %w", engine.ErrAllBackendsUnavailable),
			false,
		},
		{
			"5xx from backend is a backend failure",
			errors.New("api error InternalServerError: Internal Server Error, StatusCode: 500"),
			false,
		},
		{
			"quota is not a miss",
			fmt.Errorf("put t1_b/k: %w", engine.ErrQuotaExceeded),
			false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.missing, isObjectMissingErr(tt.err))
		})
	}
}
