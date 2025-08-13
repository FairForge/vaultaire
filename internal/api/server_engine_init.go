package api

import (
	"github.com/FairForge/vaultaire/internal/drivers"
	"github.com/FairForge/vaultaire/internal/engine"
	"go.uber.org/zap"
)

// InitializeEngine sets up the engine with drivers
func InitializeEngine(logger *zap.Logger) engine.Engine {
	// Create the core engine
	eng := engine.NewEngine(logger)

	// Add local driver for testing
	localDriver := drivers.NewLocalDriver("/tmp/vaultaire")
	eng.AddDriver("local", localDriver)
	eng.SetPrimary("local")

	// Future: Add more drivers
	// quotalessDriver := drivers.NewQuotalessDriver(config)
	// eng.AddDriver("quotaless", quotalessDriver)

	// onedriveDriver := drivers.NewOneDriveDriver(config)
	// eng.AddDriver("onedrive", onedriveDriver)
	// eng.SetBackup("onedrive")

	return eng
}
