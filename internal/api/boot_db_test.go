package api

import (
	"database/sql"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/config"
	"github.com/FairForge/vaultaire/internal/database"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// WP-R1-10: the NewServer prologue (flags, auth and MFA state, the
// backfills) runs under one boot deadline. A Postgres that dies after the
// ping — here one that accepts and never answers — used to hold the boot
// for 5 s per call (connect_timeout) and then serve with users unable to
// sign in; now the prologue returns within its budget and BootError says
// why, so main exits non-zero and vaultaire-switch keeps the old slot.
func TestBoot_DeadDatabaseFailsFast(t *testing.T) {
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
	db, err := sql.Open("postgres", database.Config{Host: host, Port: port, Database: "x", User: "x"}.DSN())
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	start := time.Now()
	srv := NewServer(&config.Config{Server: config.ServerConfig{Port: 0}}, zap.NewNop(), engine.NewEngine(nil, zap.NewNop(), nil), nil, db)
	elapsed := time.Since(start)
	assert.Less(t, elapsed, 12*time.Second, "the prologue is bounded")
	require.Error(t, srv.BootError(), "a hung database is a boot error")
}
