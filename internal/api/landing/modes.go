package landing

import _ "embed"

// modesJSON is the ONE mode table (keystone K3, plan Phase 41.0): beside
// prices.json, read through internal/modes — never parsed here, never typed
// elsewhere.
//
//go:embed modes.json
var modesJSON []byte

// ModesJSON returns the embedded mode table's bytes (internal/modes parses
// and validates them).
func ModesJSON() []byte { return modesJSON }

// produceJSON is the "what we can produce" page's one source (plan 43.4):
// read through internal/produce, its rows held to the code by
// internal/api/produce_test.go.
//
//go:embed produce.json
var produceJSON []byte

// ProduceJSON returns the embedded produce table's bytes.
func ProduceJSON() []byte { return produceJSON }
