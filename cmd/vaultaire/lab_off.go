//go:build !lab

package main

import (
	"github.com/FairForge/vaultaire/internal/engine"
	"go.uber.org/zap"
)

// registerLabDrivers is a no-op in the product binary (lab_on.go, built
// with -tags lab, registers extra local-backed drivers for live proofs).
func registerLabDrivers(*engine.CoreEngine, *zap.Logger) {}
