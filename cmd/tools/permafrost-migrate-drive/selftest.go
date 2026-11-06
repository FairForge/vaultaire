package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"strings"

	"github.com/FairForge/vaultaire/internal/drivers"
)

// runSelftest records the quickXorHash vectors from LIVE Graph answers and
// proves the cross-drive copy, the verification and permanentDelete of a
// folder on the slot's own drives, on a scratch folder (never vaultaire/):
//
//  1. a scratch folder on a drive OTHER than the destination (the source of
//     a migration), one file per QuickXorSelftestSizes, its body
//     drivers.QuickXorSelftestData(size); Graph's hash compared with the Go
//     port's for each;
//  2. the folder copied to the destination with the Graph copy action and
//     verified like a migration (count, bytes, quickXorHash);
//  3. both scratch folders permanentDeleted and checked gone.
//
// A port that disagrees with Graph stops the run (exit 1) before anything
// is copied: never a hash check that fails on good data.
func runSelftest(ctx context.Context, w io.Writer, g *graph, upn string, o options) error {
	ds, err := g.drives(ctx, upn)
	if err != nil {
		return err
	}
	var dst, src *drive
	for i := range ds {
		if ds[i].Name == oneDriveName && (o.to == "" || o.to == ds[i].ID) {
			dst = &ds[i]
		}
	}
	for i := range ds {
		if dst != nil && ds[i].ID != dst.ID && (o.from == "" || o.from == ds[i].ID) {
			src = &ds[i]
			break
		}
	}
	if dst == nil || src == nil {
		return fmt.Errorf("the self-test needs two drives (a %s library and another): found %d", oneDriveName, len(ds))
	}
	say(w, "selftest on %s: %s → %s\n", upn, src.Name, dst.Name)
	for _, d := range []*drive{src, dst} {
		if it, err := g.itemAt(ctx, d.ID, selftestFolder); err != nil {
			return err
		} else if it != nil {
			say(w, "a scratch folder left by a cut run on %s: removed\n", d.Name)
			if err := g.permanentDelete(ctx, d.ID, it.ID); err != nil {
				return err
			}
		}
	}

	say(w, "\n// quickXorHash vectors (Graph's answer on upload; Go port alongside):"+"\n")
	failed := 0
	for _, n := range drivers.QuickXorSelftestSizes {
		body := drivers.QuickXorSelftestData(n)
		it, err := g.putSmall(ctx, src.ID, fmt.Sprintf("%s/qx-%d.bin", selftestFolder, n), body, "fail")
		if err != nil {
			return fmt.Errorf("upload %d bytes: %w", n, err)
		}
		h := drivers.NewQuickXorHash()
		_, _ = h.Write(body)
		port := base64.StdEncoding.EncodeToString(h.Sum(nil))
		graphHash := ""
		if it.File != nil {
			graphHash = it.File.Hashes.QuickXorHash
		}
		mark := "match"
		if graphHash != port {
			mark = "DIFFER (port " + port + ")"
			failed++
		}
		say(w, "{size: %d, graph: %q}, // %s\n", n, graphHash, mark)
	}
	if failed > 0 {
		return fmt.Errorf("the quickXor port disagrees with Graph on %d of %d vectors — keep the verify on count + bytes and open a follow-up (scratch folder left on %s: %s)",
			failed, len(drivers.QuickXorSelftestSizes), src.Name, selftestFolder)
	}

	srcIt, err := g.itemAt(ctx, src.ID, selftestFolder)
	if err != nil || srcIt == nil {
		return fmt.Errorf("scratch folder on %s: %v", src.Name, err)
	}
	root, err := g.root(ctx, dst.ID)
	if err != nil {
		return err
	}
	say(w, "\ncross-drive copy %s/%s → %s\n", src.Name, selftestFolder, dst.Name)
	if err := copyAndWait(ctx, w, g, src.ID, srcIt.ID, dst.ID, root.ID, selftestFolder, o.pollEvery); err != nil {
		return fmt.Errorf("cross-drive copy: %w", err)
	}
	dstIt, err := g.itemAt(ctx, dst.ID, selftestFolder)
	if err != nil || dstIt == nil {
		return fmt.Errorf("the copy on %s: %v", dst.Name, err)
	}
	p := &plan{dst: *dst, src: src, srcTree: srcIt, dstTree: dstIt}
	if _, err := verifyPlan(ctx, w, g, p, exact); err != nil {
		return err
	}

	for _, d := range []struct {
		drv *drive
		it  *item
	}{{src, srcIt}, {dst, dstIt}} {
		if err := g.permanentDelete(ctx, d.drv.ID, d.it.ID); err != nil {
			return fmt.Errorf("permanentDelete the scratch folder on %s: %w", d.drv.Name, err)
		}
		gone, err := g.itemAt(ctx, d.drv.ID, selftestFolder)
		if err != nil {
			return err
		}
		if gone != nil {
			return fmt.Errorf("the scratch folder on %s is still there after permanentDelete", d.drv.Name)
		}
	}
	say(w, "scratch folders permanentDeleted on %s\n", strings.Join([]string{src.Name, dst.Name}, " and "))
	return nil
}
