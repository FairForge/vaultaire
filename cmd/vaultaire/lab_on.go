//go:build lab

package main

import (
	"context"
	"io"
	"os"
	"strings"

	"github.com/FairForge/vaultaire/internal/drivers"
	"github.com/FairForge/vaultaire/internal/engine"
	"go.uber.org/zap"
)

// registerLabDrivers registers extra local-backed drivers from
// VAULTAIRE_LAB_LOCAL_DRIVERS ("name=/dir,name2=/dir2") — a lab build only
// (-tags lab), for live proofs that need a second backend on one machine
// (WP-R6-1: a stale copy planted on another backend is never served). Never
// in the product binary.
func registerLabDrivers(eng *engine.CoreEngine, logger *zap.Logger) {
	for _, spec := range strings.Split(os.Getenv("VAULTAIRE_LAB_LOCAL_DRIVERS"), ",") {
		name, dir, ok := strings.Cut(strings.TrimSpace(spec), "=")
		if !ok || name == "" || dir == "" {
			continue
		}
		if err := os.MkdirAll(dir, 0o750); err != nil {
			logger.Fatal("lab driver directory", zap.String("dir", dir), zap.Error(err))
		}
		if err := eng.AddDriver(name, drivers.NewLocalDriver(dir, logger)); err != nil {
			logger.Fatal("lab driver registration", zap.String("name", name), zap.Error(err))
		}
		logger.Warn("LAB BUILD: extra local driver registered", zap.String("name", name), zap.String("dir", dir))
	}
	labWrap(eng, logger)
}

// labLoggedDriver logs every storage call (VAULTAIRE_LAB_LOG_CALLS=1): a
// live proof counts backend calls from the log.
type labLoggedDriver struct {
	engine.Driver
	name   string
	logger *zap.Logger
}

func (d labLoggedDriver) log(op, c, a string) {
	d.logger.Info("LAB backend call", zap.String("backend", d.name), zap.String("op", op), zap.String("container", c), zap.String("artifact", a))
}

func (d labLoggedDriver) Get(ctx context.Context, c, a string) (io.ReadCloser, error) {
	d.log("get", c, a)
	return d.Driver.Get(ctx, c, a)
}

func (d labLoggedDriver) Put(ctx context.Context, c, a string, r io.Reader, o ...engine.PutOption) error {
	d.log("put", c, a)
	return d.Driver.Put(ctx, c, a, r, o...)
}

func (d labLoggedDriver) Delete(ctx context.Context, c, a string) error {
	d.log("delete", c, a)
	return d.Driver.Delete(ctx, c, a)
}

func (d labLoggedDriver) Exists(ctx context.Context, c, a string) (bool, error) {
	d.log("exists", c, a)
	return d.Driver.Exists(ctx, c, a)
}

func init() {
	labWrap = func(eng *engine.CoreEngine, logger *zap.Logger) {
		if os.Getenv("VAULTAIRE_LAB_LOG_CALLS") != "1" {
			return
		}
		for _, name := range eng.GetDriverNames() {
			d, _ := eng.GetDriver(name)
			if err := eng.AddDriver(name, labLoggedDriver{Driver: d, name: name, logger: logger}); err != nil {
				logger.Fatal("lab driver wrap", zap.String("name", name), zap.Error(err))
			}
		}
	}
}

var labWrap func(*engine.CoreEngine, *zap.Logger)
