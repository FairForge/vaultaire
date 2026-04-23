package drivers

import "github.com/FairForge/vaultaire/internal/engine"

var (
	_ engine.Driver = (*LocalDriver)(nil)
	_ engine.Driver = (*S3CompatDriver)(nil)
	_ engine.Driver = (*IDriveDriver)(nil)
	_ engine.Driver = (*LyveDriver)(nil)
	_ engine.Driver = (*QuotalessDriver)(nil)
	_ engine.Driver = (*GeyserDriver)(nil)
	_ engine.Driver = (*ThrottledDriver)(nil)
	_ engine.Driver = (*CompressionDriver)(nil)
)
