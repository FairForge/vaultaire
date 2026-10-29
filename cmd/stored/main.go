// Command stored is the Stored CLI (plan 26.4). Its first verb is
// `stored listen` (WP-DX-1, Stage 1a Prompt 50): the account's events,
// test mode's included, replayed to a developer's local endpoint.
//
//	stored listen --forward-to localhost:3000/webhooks
//
// It reads STORED_TOKEN (a management JWT: POST /auth/login) and STORED_API
// (default https://stored.cloud).
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	switch os.Args[1] {
	case "listen":
		if err := runListen(ctx, os.Args[2:], os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "stored listen:", err)
			os.Exit(1)
		}
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: stored listen --forward-to localhost:<port>/<path> [--cursor-file f] [--secret whsec_…]")
}
