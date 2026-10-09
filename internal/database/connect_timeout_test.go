package database

import (
	"context"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestNewPostgres_AServerThatAcceptsAndNeverAnswersFailsTheConnectInSeconds(t *testing.T) {
	// Arrange: a TCP listener that accepts every connection and never sends
	// a byte — a Postgres wedged in its startup. lib/pq ignores the context
	// during the startup handshake. Before: the connect never returned (the
	// test gave up at 15 s).
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
	host, portStr, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(portStr)
	pg, err := NewPostgres(Config{Host: host, Port: port, Database: "x", User: "x"}, zap.NewNop())
	require.NoError(t, err)
	t.Cleanup(func() { _ = pg.db.Close() })

	// Act
	start := time.Now()
	done := make(chan error, 1)
	go func() { done <- pg.db.PingContext(context.Background()) }()

	// Assert
	select {
	case err := <-done:
		assert.Error(t, err)
		assert.Less(t, time.Since(start), 8*time.Second)
	case <-time.After(15 * time.Second):
		t.Fatal("the connect did not return in 15 s")
	}
}

func TestConfigDSN_CarriesTheConnectTimeoutAndNeverAnEmptyPassword(t *testing.T) {
	assert.Equal(t, "host=h port=5 user=u dbname=d sslmode=disable connect_timeout=5",
		Config{Host: "h", Port: 5, User: "u", Database: "d"}.DSN())
	assert.Equal(t, "host=h port=5 user=u password=p dbname=d sslmode=require connect_timeout=5",
		Config{Host: "h", Port: 5, User: "u", Password: "p", Database: "d", SSLMode: "require"}.DSN())
}
