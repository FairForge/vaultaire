package dashboard

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"

	"github.com/FairForge/vaultaire/internal/dashboard/handlers"
)

// Templates holds all HTML templates embedded into the binary.
//
//go:embed all:templates
var Templates embed.FS

// Static holds CSS, JS, and other static assets embedded into the binary.
//
//go:embed static/*
var Static embed.FS

// init stamps handlers.AssetVersion with a hash of the embedded static
// tree so asset URLs change whenever a stylesheet, script or font does
// (Cloudflare caches /static/ for hours; see handlers/assets.go).
func init() {
	h := sha256.New()
	_ = fs.WalkDir(Static, "static", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, readErr := Static.ReadFile(p)
		if readErr != nil {
			return readErr
		}
		h.Write([]byte(p + "\n"))
		h.Write(b)
		return nil
	})
	handlers.AssetVersion = hex.EncodeToString(h.Sum(nil))[:12]
}
