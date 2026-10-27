//go:build ignore

// goldenbody writes crypto.GoldenBody(seed, n) to a file: the lab
// proof's bodies (edge/s3-ingest/lab/codec-proof.sh), so a Worker upload can
// be held to the golden chunk boundaries. Run by name (go run
// edge/s3-ingest/lab/goldenbody.go): the build tag keeps it out of the module.
package main

import (
	"fmt"
	"os"
	"strconv"

	"github.com/FairForge/vaultaire/internal/crypto"
)

func main() {
	if len(os.Args) != 4 {
		fmt.Fprintln(os.Stderr, "usage: goldenbody <seed> <bytes> <file>")
		os.Exit(2)
	}
	seed, err := strconv.ParseUint(os.Args[1], 10, 64)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	n, err := strconv.Atoi(os.Args[2])
	if err != nil || n < 0 {
		fmt.Fprintln(os.Stderr, "bytes: a non-negative integer")
		os.Exit(2)
	}
	if err := os.WriteFile(os.Args[3], crypto.GoldenBody(seed, n), 0o600); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
