package database

import (
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// WP-R1-10 (R1-16, R9-06): the boot DB contract. NewPostgres pings, bounded,
// so a dead Postgres is an error at boot — it used to log "connected" and
// every DB call failed afterwards — and main exits non-zero, so
// vaultaire-switch keeps the old slot.
func TestBoot_DeadDatabaseFailsFast(t *testing.T) {
	refused := func(t *testing.T) (string, int) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		host, p, _ := net.SplitHostPort(ln.Addr().String())
		require.NoError(t, ln.Close()) // nothing listens there now
		port, _ := strconv.Atoi(p)
		return host, port
	}
	for name, addr := range map[string]func(*testing.T) (string, int){
		"nothing listening":         refused,
		"accepts and never answers": blackhole,
	} {
		t.Run(name, func(t *testing.T) {
			host, port := addr(t)
			start := time.Now()
			pg, err := NewPostgres(Config{Host: host, Port: port, Database: "x", User: "x"}, zap.NewNop())
			require.Error(t, err, "a dead database is a boot error")
			assert.Nil(t, pg)
			assert.Less(t, time.Since(start), 12*time.Second)
		})
	}
}

// blackhole is a TCP listener that accepts every connection and never sends
// a byte — a Postgres wedged in its startup.
func blackhole(t *testing.T) (string, int) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { _ = c.Close() })
		}
	}()
	host, p, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(p)
	return host, port
}
