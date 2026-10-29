package drivers

import "github.com/FairForge/vaultaire/internal/engine"

var (
	_ engine.Driver = (*LocalDriver)(nil)
	_ engine.Driver = (*IDriveDriver)(nil)
	_ engine.Driver = (*LyveDriver)(nil)
	_ engine.Driver = (*GeyserDriver)(nil)
)
