package config

// StorageModeOrder is the auto-detect order of the primary backend when
// STORAGE_MODE is unset: the first entry whose env key is set wins. iDrive
// stays first — it is the long-term primary — so a box that already runs on
// iDrive is unchanged by adding a Wasabi pair; switching the primary to the
// interim Wasabi (2026-10-03) is the explicit `STORAGE_MODE=wasabi` line.
// Lyve, R2 and permafrost are never auto-selected (tier / public / fleet
// roles). Used by cmd/vaultaire/main.go (the engine) and the dashboard's
// Deps.StorageMode, which used to re-derive it from a list that lacked the
// iDrive branch (docs/CONFIG.md, WP-R1-4).
var StorageModeOrder = []struct{ Mode, EnvKey string }{
	{"idrive", "IDRIVE_ACCESS_KEY"},
	{"wasabi", "WASABI_ACCESS_KEY"},
	{"quotaless", "QUOTALESS_ACCESS_KEY"},
	{"s3", "S3_ACCESS_KEY"},
	{"geyser", "GEYSER_ACCESS_KEY"},
}

// DetectStorageMode is the primary backend: STORAGE_MODE when set, else the
// first configured backend in StorageModeOrder, else "local".
func DetectStorageMode(getenv func(string) string) string {
	if mode := getenv("STORAGE_MODE"); mode != "" {
		return mode
	}
	for _, c := range StorageModeOrder {
		if getenv(c.EnvKey) != "" {
			return c.Mode
		}
	}
	return "local"
}
