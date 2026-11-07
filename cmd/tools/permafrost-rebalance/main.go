// cmd/tools/permafrost-rebalance — lays the permafrost fleet out under its
// current placement (WP-FLEET-5 PR B, D-F5-5): the data of a fleet whose
// markers record another placement (prod's version 1, `hash % slots`; or an
// accepted order change, RB3-1) is moved file by file to the home the current
// placement gives it, verified by quickXorHash, the source permanentDeleted,
// and the markers rewritten last.
//
//	-dry-run (default)  read the markers, walk every account, print per account what moves where and how much
//	-run                set placement_target on every marker, then move every file not at its home
//	                    (-concurrency at once, through the driver's pacing); resumable — run it again after
//	                    the live slots have re-checked (an hour, or `vaultaire-switch restart`) to catch
//	                    the writes they placed meanwhile
//	-commit             walk again; with nothing left to move, rewrite every marker: placed_under = the
//	                    fleet's digest, placement_version = -target, no target. Exit 2 while files remain
//	-target N           the placement version to lay out under (default 2)
//	-concurrency N      moves in flight (default 4)
//	-max-moves N        stop -run after N moves (0 = all): a bounded first run
//
// The fleet comes from the environment as the server reads it: `TENANT_N_*`
// slots or `PERMAFROST_FLEET_FILE`, with `PERMAFROST_ACCOUNT_*` /
// `PERMAFROST_TENANT_GBH` pacing (`set -a; . /opt/vaultaire/configs/.env;
// set +a` on SLC, or `.env.bench`). A fleet whose order is not the one its
// markers record is refused: accept it first (`PERMAFROST_FLEET_ACCEPT`, a
// restart). Exit 0 done, 1 refused / error, 2 -commit with files remaining.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/FairForge/vaultaire/internal/drivers"
	"go.uber.org/zap"
)

func main() {
	var (
		dry         = flag.Bool("dry-run", false, "plan only (the default)")
		run         = flag.Bool("run", false, "set the target and move every file not at its home")
		commit      = flag.Bool("commit", false, "rewrite the markers once nothing is left to move")
		target      = flag.Int("target", 2, "the placement version to lay out under")
		concurrency = flag.Int("concurrency", 4, "moves in flight")
		maxMoves    = flag.Int("max-moves", 0, "stop -run after this many moves (0 = all)")
	)
	flag.Parse()
	modes := 0
	for _, b := range []bool{*dry, *run, *commit} {
		if b {
			modes++
		}
	}
	if modes > 1 {
		fatalf("one of -dry-run, -run, -commit")
	}
	if *concurrency < 1 || *concurrency > 64 {
		fatalf("-concurrency 1..64")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	logger, _ := zap.NewProduction()
	defer func() { _ = logger.Sync() }()
	d, err := drivers.NewOneDriveFleetDriver(logger)
	if err != nil {
		fatalf("fleet: %v", err)
	}

	switch {
	case *run:
		err = doRun(ctx, os.Stdout, d, *target, *concurrency, *maxMoves)
	case *commit:
		err = doCommit(ctx, os.Stdout, d, *target)
	default:
		err = doPlan(ctx, os.Stdout, d, *target)
	}
	switch {
	case errors.Is(err, drivers.ErrRebalanceIncomplete):
		say(os.Stderr, "refused: %v\n", err)
		os.Exit(2)
	case err != nil:
		fatalf("%v", err)
	}
}

func fatalf(format string, a ...any) {
	say(os.Stderr, format+"\n", a...)
	os.Exit(1)
}

func say(w io.Writer, format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }

func gib(b int64) string { return fmt.Sprintf("%.2f GiB", float64(b)/(1<<30)) }

// printPlan writes the plan: the markers' state, then one line per account.
func printPlan(w io.Writer, p *drivers.RebalancePlan) {
	say(w, "fleet %s  state=%s  laid-out-under=v%d  target-set=v%d  placed=%t  → version %d\n",
		p.Digest, p.State, p.Laid, p.Target, p.Placed, p.Version)
	say(w, "%-5s %-24s %8s %12s %8s %8s\n", "idx", "account", "files", "bytes", "out", "in")
	for _, a := range p.Accounts {
		say(w, "%-5d %-24s %8d %12s %8d %8d\n", a.Index, a.Name, a.Files, gib(a.Bytes), a.MovesOut, a.MovesIn)
	}
	say(w, "total %d files, %s; %d to move (%s)\n", p.Files, gib(p.Bytes), len(p.Moves), gib(p.MoveBytes()))
	if len(p.Strays) > 0 {
		say(w, "%d files where neither placement puts them (moved like the others):\n", len(p.Strays))
		for i, s := range p.Strays {
			if i == 20 {
				say(w, "  … %d more\n", len(p.Strays)-20)
				break
			}
			say(w, "  %s\n", s)
		}
	}
}

func doPlan(ctx context.Context, w io.Writer, d *drivers.OneDriveDriver, target int) error {
	p, err := d.RebalancePlan(ctx, target)
	if err != nil {
		return err
	}
	printPlan(w, p)
	if len(p.Moves) == 0 && !p.Placed {
		say(w, "nothing to move; -commit rewrites the markers\n")
	}
	return nil
}

func doRun(ctx context.Context, w io.Writer, d *drivers.OneDriveDriver, target, concurrency, maxMoves int) error {
	p, err := d.RebalancePlan(ctx, target)
	if err != nil {
		return err
	}
	printPlan(w, p)
	if p.Target != target {
		if err := d.RebalanceSetTarget(ctx, target); err != nil {
			return err
		}
		say(w, "placement_target %d written on every marker (the live slots follow at their next check)\n", target)
	}
	moves := p.Moves
	if maxMoves > 0 && len(moves) > maxMoves {
		moves = moves[:maxMoves]
	}
	// Largest first: the long transfers start early, the small ones fill in.
	sort.SliceStable(moves, func(a, b int) bool { return moves[a].Size > moves[b].Size })

	var (
		done, failed atomic.Int64
		bytes        atomic.Int64
		wg           sync.WaitGroup
		mu           sync.Mutex
		errs         []string
	)
	queue := make(chan drivers.RebalanceMove)
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for m := range queue {
				if err := d.RebalanceMove(ctx, m); err != nil {
					failed.Add(1)
					mu.Lock()
					if len(errs) < 20 {
						errs = append(errs, err.Error())
					}
					mu.Unlock()
					continue
				}
				done.Add(1)
				bytes.Add(m.Size)
			}
		}()
	}
	for _, m := range moves {
		if ctx.Err() != nil {
			break
		}
		queue <- m
	}
	close(queue)
	wg.Wait()
	say(w, "moved %d of %d (%s); failed %d\n", done.Load(), len(moves), gib(bytes.Load()), failed.Load())
	for _, e := range errs {
		say(w, "  %s\n", e)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if failed.Load() > 0 {
		return fmt.Errorf("%d moves failed; run again", failed.Load())
	}
	say(w, "run again after the live slots have re-checked (an hour, or a restart), then -commit\n")
	return nil
}

func doCommit(ctx context.Context, w io.Writer, d *drivers.OneDriveDriver, target int) error {
	p, err := d.RebalanceCommit(ctx, target)
	if p != nil {
		printPlan(w, p)
	}
	if err != nil {
		return err
	}
	say(w, "committed: every marker placed under %s, placement_version %d — the live slots are authoritative at their next check\n", p.Digest, target)
	return nil
}
