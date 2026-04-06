package dashboard

import "embed"

// Templates holds all HTML templates embedded into the binary.
//
//go:embed all:templates
var Templates embed.FS

// Static holds CSS, JS, and other static assets embedded into the binary.
//
//go:embed static/*
var Static embed.FS
