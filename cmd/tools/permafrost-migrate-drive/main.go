// cmd/tools/permafrost-migrate-drive — moves one permafrost account's
// vaultaire/ tree from the drive it was written on to the user's OneDrive
// library (WP-FLEET-1, D-F1-4).
//
// The driver took /users/{upn}/drives[0] until WP-FLEET-1, and two of prod's
// three fleet users list a hidden PersonalCacheLibrary first: their tree sits
// there. The driver now chooses the drive named OneDrive, and prod pins each
// slot's TENANT_N_DRIVE_ID to where its tree is today; this tool moves the
// tree, one account per run, and is never a boot action.
//
//	-dry-run (default)  every drive of the user: quota, and vaultaire/'s files, folders, bytes, marker
//	-copy               Graph copy of vaultaire/ to the OneDrive library (conflict = fail: an existing
//	                    copy is verified, a partial one filled), then verified folder by folder —
//	                    count, bytes, quickXorHash per file; any difference = exit 2
//	-verify             the verification alone
//	-switch             verify again, then write drive_id on BOTH markers (the old gains moved_to);
//	                    prints the TENANT_N_DRIVE_ID line to set. With no tree elsewhere it records
//	                    drive_id on the one marker
//	-retire -confirm-retire
//	                    the old tree permanentDeleted, after its marker names the move and the new
//	                    tree is verified to hold every old item
//	-selftest           uploads QuickXorSelftestData of every QuickXorSelftestSizes into a scratch
//	                    folder on the OTHER drive, compares Graph's quickXorHash with the Go port,
//	                    copies the folder cross-drive, verifies the copy, permanentDeletes both
//
// Needs TENANT_N_ID / _CLIENT_ID / _SECRET / _USER of -slot N in the
// environment (`set -a; . ~/vaultaire-bench/.env.bench; set +a` on SLC).
// Exit 0 done, 1 refused / usage / error, 2 the trees differ.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
)

const (
	rootFolder     = "vaultaire"
	oneDriveName   = "OneDrive"
	selftestFolder = "vaultaire-quickxor-selftest"
	maxDiffsShown  = 10
)

// errDiffer is a verification that found differences (exit 2).
var errDiffer = errors.New("the trees differ")

type options struct {
	slot          int
	mode          string
	from, to      string // drive ids; "" = chosen
	confirmRetire bool
	pollEvery     time.Duration
}

func main() {
	var o options
	dry := flag.Bool("dry-run", false, "list every drive of the user and what vaultaire/ holds on it (the default)")
	cp := flag.Bool("copy", false, "copy vaultaire/ to the OneDrive library and verify it")
	verify := flag.Bool("verify", false, "verify the copy only")
	sw := flag.Bool("switch", false, "verify, then write drive_id on both markers")
	retire := flag.Bool("retire", false, "permanentDelete the old tree (needs -confirm-retire)")
	selftest := flag.Bool("selftest", false, "quickXorHash vectors from live Graph + a cross-drive copy round trip")
	flag.IntVar(&o.slot, "slot", 0, "the TENANT_N slot (1..15)")
	flag.StringVar(&o.from, "from", "", "the source drive id (default: the drive other than the destination that holds vaultaire/)")
	flag.StringVar(&o.to, "to", "", "the destination drive id (default: the drive named OneDrive)")
	flag.BoolVar(&o.confirmRetire, "confirm-retire", false, "with -retire: really delete")
	flag.DurationVar(&o.pollEvery, "poll", 5*time.Second, "copy monitor poll interval")
	flag.Parse()

	modes := map[string]bool{"dry-run": *dry, "copy": *cp, "verify": *verify, "switch": *sw, "retire": *retire, "selftest": *selftest}
	for m, on := range modes {
		if on {
			if o.mode != "" {
				fatalf("one mode per run (-%s and -%s)", o.mode, m)
			}
			o.mode = m
		}
	}
	if o.mode == "" {
		o.mode = "dry-run"
	}
	if o.slot < 1 || o.slot > 15 {
		fatalf("-slot N (1..15) is required")
	}
	upn, g, err := graphForSlot(o.slot)
	if err != nil {
		fatalf("%v", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	start := time.Now()
	err = run(ctx, os.Stdout, g, upn, o)
	fmt.Printf("\n%s: %d Graph requests, %d throttled, %s\n", o.mode, g.requests.Load(), g.throttled.Load(), time.Since(start).Round(time.Second))
	switch {
	case errors.Is(err, errDiffer):
		fmt.Println("RESULT: the trees differ — nothing switched or deleted")
		os.Exit(2)
	case err != nil:
		fmt.Printf("RESULT: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("RESULT: ok")
}

func fatalf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "permafrost-migrate-drive: "+format+"\n", a...)
	os.Exit(1)
}

// graphForSlot is the slot's user and an app-only Graph client.
func graphForSlot(n int) (string, *graph, error) {
	p := fmt.Sprintf("TENANT_%d_", n)
	tid, cid, sec, upn := os.Getenv(p+"ID"), os.Getenv(p+"CLIENT_ID"), os.Getenv(p+"SECRET"), os.Getenv(p+"USER")
	if tid == "" || cid == "" || sec == "" || upn == "" {
		return "", nil, fmt.Errorf("%sID, %sCLIENT_ID, %sSECRET and %sUSER must be set", p, p, p, p)
	}
	cred, err := azidentity.NewClientSecretCredential(tid, cid, sec, nil)
	if err != nil {
		return "", nil, fmt.Errorf("credential: %w", err)
	}
	token := func(ctx context.Context) (string, error) {
		tok, err := cred.GetToken(ctx, policy.TokenRequestOptions{Scopes: []string{"https://graph.microsoft.com/.default"}})
		if err != nil {
			return "", err
		}
		return tok.Token, nil
	}
	return upn, newGraph("https://graph.microsoft.com/v1.0", token), nil
}

// plan is the run's two drives.
type plan struct {
	drives   []drive
	dst      drive
	src      *drive // nil = no tree on another drive
	dstTree  *item  // vaultaire/ on dst, nil = none
	srcTree  *item
	slotUser string
}

func findPlan(ctx context.Context, g *graph, upn string, o options) (*plan, error) {
	ds, err := g.drives(ctx, upn)
	if err != nil {
		return nil, fmt.Errorf("drives of %s: %w", upn, err)
	}
	p := &plan{drives: ds, slotUser: upn}
	found := false
	for _, d := range ds {
		if (o.to != "" && d.ID == o.to) || (o.to == "" && d.Name == oneDriveName) {
			if found {
				return nil, fmt.Errorf("two drives named %s — pass -to", oneDriveName)
			}
			p.dst, found = d, true
		}
	}
	if !found {
		return nil, fmt.Errorf("no destination drive (named %s, or -to) among %d drives", oneDriveName, len(ds))
	}
	if p.dstTree, err = g.itemAt(ctx, p.dst.ID, rootFolder); err != nil {
		return nil, fmt.Errorf("vaultaire/ on %s: %w", p.dst.Name, err)
	}
	for i := range ds {
		d := ds[i]
		if d.ID == p.dst.ID || (o.from != "" && d.ID != o.from) {
			continue
		}
		it, err := g.itemAt(ctx, d.ID, rootFolder)
		if err != nil {
			return nil, fmt.Errorf("vaultaire/ on %s: %w", d.Name, err)
		}
		if it == nil {
			continue
		}
		if p.src != nil {
			return nil, fmt.Errorf("vaultaire/ on two drives besides %s (%s, %s) — pass -from", p.dst.Name, p.src.Name, d.Name)
		}
		p.src, p.srcTree = &ds[i], it
	}
	if o.from != "" && p.src == nil {
		return nil, fmt.Errorf("-from %s holds no vaultaire/", o.from)
	}
	return p, nil
}

func run(ctx context.Context, w io.Writer, g *graph, upn string, o options) error {
	if o.mode == "selftest" {
		return runSelftest(ctx, w, g, upn, o)
	}
	p, err := findPlan(ctx, g, upn, o)
	if err != nil {
		return err
	}
	say(w, "slot %d %s → destination %s %s\n", o.slot, upn, p.dst.Name, p.dst.ID)
	switch o.mode {
	case "dry-run":
		return runDryRun(ctx, w, g, p)
	case "copy":
		return runCopy(ctx, w, g, p, o)
	case "verify":
		_, err := verifyPlan(ctx, w, g, p, exact)
		return err
	case "switch":
		return runSwitch(ctx, w, g, p, o)
	case "retire":
		return runRetire(ctx, w, g, p, o)
	}
	return fmt.Errorf("unknown mode %s", o.mode)
}

func gib(b int64) string { return fmt.Sprintf("%.2f GiB", float64(b)/(1<<30)) }

func runDryRun(ctx context.Context, w io.Writer, g *graph, p *plan) error {
	for _, d := range p.drives {
		q := d.Quota
		say(w, "\ndrive %-22s %-8s %s\n  quota: used %s, recycle bin (deleted) %s, remaining %s of %s, state %s\n",
			d.Name, d.DriveType, d.ID, gib(q.Used), gib(q.Deleted), gib(q.Remaining), gib(q.Total), q.State)
		it, err := g.itemAt(ctx, d.ID, rootFolder)
		if err != nil {
			return err
		}
		if it == nil {
			say(w, "  vaultaire/: none"+"\n")
			continue
		}
		t, err := walk(ctx, g, d.ID, it.ID)
		if err != nil {
			return err
		}
		files, folders, bytes := t.totals()
		say(w, "  vaultaire/: %d files, %d folders, %s (%d bytes)\n", files, folders, gib(bytes), bytes)
		m, err := readMarker(ctx, g, d.ID)
		switch {
		case err != nil:
			say(w, "  marker: unreadable: %v\n", err)
		case m == nil:
			say(w, "  marker: none"+"\n")
		default:
			say(w, "  marker: index %d of %d, drive_id %q, moved_to %q\n", m.Index, len(m.Fleet), m.DriveID, m.MovedTo)
		}
	}
	switch {
	case p.src == nil && p.dstTree != nil:
		say(w, "\nthe tree is on %s already: no copy; -switch records drive_id on its marker\n", p.dst.Name)
	case p.src != nil && p.dstTree == nil:
		say(w, "\nnext: -copy (%s → %s)\n", p.src.Name, p.dst.Name)
	case p.src != nil:
		say(w, "\nboth drives hold vaultaire/: -verify, then -switch or -retire\n")
	}
	return nil
}

// verifyPlan walks both trees and compares them; errDiffer with the first
// differences named.
func verifyPlan(ctx context.Context, w io.Writer, g *graph, p *plan, mode compareMode) ([]diff, error) {
	if p.src == nil || p.dstTree == nil {
		return nil, fmt.Errorf("nothing to verify: the source or the destination holds no vaultaire/")
	}
	src, err := walk(ctx, g, p.src.ID, p.srcTree.ID)
	if err != nil {
		return nil, fmt.Errorf("walk %s: %w", p.src.Name, err)
	}
	dst, err := walk(ctx, g, p.dst.ID, p.dstTree.ID)
	if err != nil {
		return nil, fmt.Errorf("walk %s: %w", p.dst.Name, err)
	}
	sf, sd, sb := src.totals()
	df, dd, db := dst.totals()
	say(w, "source      %-22s %d files, %d folders, %d bytes\n", p.src.Name, sf, sd, sb)
	say(w, "destination %-22s %d files, %d folders, %d bytes\n", p.dst.Name, df, dd, db)
	ds, err := compare(ctx, src, dst, mode, graphHasher(g))
	if err != nil {
		return nil, err
	}
	if len(ds) == 0 {
		say(w, "verified: every file's count, bytes and quickXorHash match\n")
		return nil, nil
	}
	say(w, "%d differences, the first %d:\n", len(ds), min(len(ds), maxDiffsShown))
	for _, d := range ds[:min(len(ds), maxDiffsShown)] {
		say(w, "  %s\n", d)
	}
	return ds, errDiffer
}

func runCopy(ctx context.Context, w io.Writer, g *graph, p *plan, o options) error {
	if p.src == nil {
		return fmt.Errorf("no vaultaire/ on a drive other than %s: nothing to copy", p.dst.Name)
	}
	if p.dstTree == nil {
		root, err := g.root(ctx, p.dst.ID)
		if err != nil {
			return fmt.Errorf("root of %s: %w", p.dst.Name, err)
		}
		say(w, "copy %s/vaultaire → %s/vaultaire\n", p.src.Name, p.dst.Name)
		if err := copyAndWait(ctx, w, g, p.src.ID, p.srcTree.ID, p.dst.ID, root.ID, rootFolder, o.pollEvery); err != nil && !errors.Is(err, errExists) {
			return err
		}
		if p.dstTree, err = g.itemAt(ctx, p.dst.ID, rootFolder); err != nil || p.dstTree == nil {
			return fmt.Errorf("vaultaire/ on %s after the copy: %v", p.dst.Name, err)
		}
	} else {
		say(w, "vaultaire/ exists on %s: verified, not copied again (a resumed run)\n", p.dst.Name)
	}
	for round := 1; ; round++ {
		ds, err := verifyPlan(ctx, w, g, p, exact)
		if !errors.Is(err, errDiffer) || !onlyMissing(ds) || round > 3 {
			return err
		}
		// A copy cut short, or source writes since: copy what is missing.
		dst, err := walk(ctx, g, p.dst.ID, p.dstTree.ID)
		if err != nil {
			return err
		}
		src, err := walk(ctx, g, p.src.ID, p.srcTree.ID)
		if err != nil {
			return err
		}
		tops := topMissing(ds)
		say(w, "fill round %d: copying %d missing items\n", round, len(tops))
		for _, path := range tops {
			parentID := p.dstTree.ID
			if pp := parentOf(path); pp != "" {
				parentID = dst.items[pp].id
			}
			err := copyAndWait(ctx, w, g, p.src.ID, src.items[path].id, p.dst.ID, parentID, baseOf(path), o.pollEvery)
			if err != nil && !errors.Is(err, errExists) {
				return fmt.Errorf("fill %s: %w", path, err)
			}
		}
	}
}

func copyAndWait(ctx context.Context, w io.Writer, g *graph, srcDrive, itemID, dstDrive, dstParent, name string, every time.Duration) error {
	monitor, err := g.copyItem(ctx, srcDrive, itemID, dstDrive, dstParent, name)
	if err != nil {
		return err
	}
	started := time.Now()
	return g.pollCopy(ctx, monitor, every, func(st copyStatus) {
		say(w, "  %s %s %.0f%% (%s)\n", name, st.Status, st.PercentageComplete, time.Since(started).Round(time.Second))
	})
}

func runSwitch(ctx context.Context, w io.Writer, g *graph, p *plan, o options) error {
	if p.dstTree == nil {
		return fmt.Errorf("no vaultaire/ on %s: run -copy first", p.dst.Name)
	}
	if p.src != nil {
		if _, err := verifyPlan(ctx, w, g, p, exact); err != nil {
			return err
		}
	}
	dstMarker, err := readMarkerBytes(ctx, g, p.dst.ID)
	if err != nil {
		return fmt.Errorf("marker on %s: %w", p.dst.Name, err)
	}
	base := dstMarker
	var srcMarker []byte
	if p.src != nil {
		if srcMarker, err = readMarkerBytes(ctx, g, p.src.ID); err != nil {
			return fmt.Errorf("marker on %s: %w", p.src.Name, err)
		}
		base = srcMarker // the tree's own marker is the authority
	}
	if base == nil {
		return fmt.Errorf("no fleet marker to switch (vaultaire/%s)", markerPathFor())
	}
	if err := writeMarker(ctx, g, p.dst.ID, base, p.dst.ID, ""); err != nil {
		return fmt.Errorf("marker on %s: %w", p.dst.Name, err)
	}
	say(w, "marker on %s: drive_id %s\n", p.dst.Name, p.dst.ID)
	if p.src != nil {
		if err := writeMarker(ctx, g, p.src.ID, srcMarker, p.dst.ID, p.dst.ID); err != nil {
			return fmt.Errorf("marker on %s: %w", p.src.Name, err)
		}
		say(w, "marker on %s: drive_id %s, moved_to %s\n", p.src.Name, p.dst.ID, p.dst.ID)
	}
	say(w, "\nset in /opt/vaultaire/configs/.env (backup first), then sudo vaultaire-switch restart:\nTENANT_%d_DRIVE_ID=%s\n", o.slot, p.dst.ID)
	return nil
}

func runRetire(ctx context.Context, w io.Writer, g *graph, p *plan, o options) error {
	if p.src == nil {
		return fmt.Errorf("no old tree: vaultaire/ is on %s only", p.dst.Name)
	}
	if p.dstTree == nil {
		return fmt.Errorf("no vaultaire/ on %s: refusing to retire the only copy", p.dst.Name)
	}
	m, err := readMarker(ctx, g, p.src.ID)
	if err != nil {
		return fmt.Errorf("marker on %s: %w", p.src.Name, err)
	}
	if m == nil || m.MovedTo != p.dst.ID {
		return fmt.Errorf("the marker on %s does not name the move to %s: run -switch first", p.src.Name, p.dst.Name)
	}
	if _, err := verifyPlan(ctx, w, g, p, superset); err != nil {
		return err
	}
	if !o.confirmRetire {
		return fmt.Errorf("verified; pass -confirm-retire to permanentDelete %s/vaultaire (no recycle bin)", p.src.Name)
	}
	if err := g.permanentDelete(ctx, p.src.ID, p.srcTree.ID); err != nil {
		return fmt.Errorf("permanentDelete %s/vaultaire: %w", p.src.Name, err)
	}
	say(w, "retired: %s/vaultaire permanently deleted\n", p.src.Name)
	return nil
}

func markerPathFor() string { return strings.TrimPrefix(markerPath, rootFolder+"/") }

// say writes one line of the run's report (stdout; a write error is not
// the run's).
func say(w io.Writer, format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }
