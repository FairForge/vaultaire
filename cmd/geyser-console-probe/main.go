// geyser-console-probe: two-step Geyser console probe (email-MFA login).
// `login` triggers the emailed MFA code and saves the challenge hash;
// `-code N dump` verifies, saves the session, and dumps tape collections
// (tapeUsage), sites, datacenters, events, the first bucket matching
// -bucket with its per-object Location (-prefix), and /tasks; `raw <path>`
// GETs any console path with the saved session. First used 2026-09-23 for
// the LA2 stuck-restore investigation. Tooling only — not wired into the
// server. State file (session token) is 0600 in $HOME.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/FairForge/vaultaire/internal/drivers"
	"go.uber.org/zap"
)

func main() {
	stateFile := flag.String("state", os.ExpandEnv("$HOME/.geyser-console-probe.json"), "challenge/session state file")
	code := flag.String("code", "", "MFA code (dump step)")
	bucketPrefix := flag.String("bucket", "la2bench", "bucket name prefix to browse")
	browsePrefix := flag.String("prefix", "canary-20260919/", "object prefix to browse")
	flag.Parse()
	if flag.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "usage: geyser-console-probe login | -code N dump | dump | raw <path> | restore-cache <bucketId> <path> [versionId] | presign <bucketId> <path>")
		os.Exit(2)
	}
	logger := zap.NewNop()
	cfg := drivers.GeyserProvisioningConfig{}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	type state struct {
		Hash, AccessToken, UserID string
	}
	var st state
	if b, err := os.ReadFile(*stateFile); err == nil {
		_ = json.Unmarshal(b, &st)
	}
	save := func() {
		b, _ := json.Marshal(st)
		_ = os.WriteFile(*stateFile, b, 0o600)
	}

	switch flag.Arg(0) {
	case "login":
		email, pw := os.Getenv("GEYSER_CONSOLE_EMAIL"), os.Getenv("GEYSER_CONSOLE_PASSWORD")
		if email == "" || pw == "" {
			fmt.Fprintln(os.Stderr, "GEYSER_CONSOLE_EMAIL / GEYSER_CONSOLE_PASSWORD required")
			os.Exit(2)
		}
		c := drivers.NewGeyserAdminClient("", "", cfg, logger)
		ch, err := c.Login(ctx, email, pw)
		if err != nil {
			fmt.Fprintln(os.Stderr, "login:", err)
			os.Exit(1)
		}
		st.Hash = ch.Hash
		save()
		fmt.Printf("challenge issued (responseType=%s totp=%v) — MFA code sent by email; run: dump -code <code>\n", ch.ResponseType, ch.TOTPUser)
	case "raw":
		if st.AccessToken == "" || flag.NArg() < 2 {
			fmt.Fprintln(os.Stderr, "need a saved session and a path")
			os.Exit(2)
		}
		c := drivers.NewGeyserAdminClient(st.AccessToken, st.UserID, cfg, logger)
		raw, err := c.RawGet(ctx, flag.Arg(1))
		if err != nil {
			fmt.Fprintln(os.Stderr, "raw:", err)
			os.Exit(1)
		}
		_, _ = os.Stdout.Write(raw)
		fmt.Println()
	case "restore-cache":
		// restore-cache <bucketId> <path> [versionId] — console-side recall to cache.
		if st.AccessToken == "" || flag.NArg() < 3 {
			fmt.Fprintln(os.Stderr, "need a saved session, bucket id and path")
			os.Exit(2)
		}
		c := drivers.NewGeyserAdminClient(st.AccessToken, st.UserID, cfg, logger)
		ver := ""
		if flag.NArg() > 3 {
			ver = flag.Arg(3)
		}
		if err := c.RestoreToCache(ctx, flag.Arg(1), flag.Arg(2), ver); err != nil {
			fmt.Fprintln(os.Stderr, "restore-cache:", err)
			os.Exit(1)
		}
		fmt.Println("restore-to-cache accepted")
	case "presign":
		if st.AccessToken == "" || flag.NArg() < 3 {
			fmt.Fprintln(os.Stderr, "need a saved session, bucket id and path")
			os.Exit(2)
		}
		c := drivers.NewGeyserAdminClient(st.AccessToken, st.UserID, cfg, logger)
		u, err := c.PresignDownload(ctx, flag.Arg(1), flag.Arg(2))
		if err != nil {
			fmt.Fprintln(os.Stderr, "presign:", err)
			os.Exit(1)
		}
		fmt.Println(u)
	case "rawpost", "rawput", "rawdelete":
		// rawpost <path> <json> | rawput <path> <json> | rawdelete <path>
		if st.AccessToken == "" || flag.NArg() < 2 {
			fmt.Fprintln(os.Stderr, "need a saved session and a path")
			os.Exit(2)
		}
		c := drivers.NewGeyserAdminClient(st.AccessToken, st.UserID, cfg, logger)
		method := map[string]string{"rawpost": "POST", "rawput": "PUT", "rawdelete": "DELETE"}[flag.Arg(0)]
		var payload any
		if flag.NArg() > 2 {
			var m map[string]any
			if err := json.Unmarshal([]byte(flag.Arg(2)), &m); err != nil {
				fmt.Fprintln(os.Stderr, "bad json:", err)
				os.Exit(2)
			}
			payload = m
		}
		raw, err := c.RawDo(ctx, method, flag.Arg(1), payload)
		if err != nil {
			fmt.Fprintln(os.Stderr, "raw:", err)
			os.Exit(1)
		}
		_, _ = os.Stdout.Write(raw)
		fmt.Println()
	case "dump":
		var c *drivers.GeyserAdminClient
		if st.AccessToken != "" && *code == "" {
			c = drivers.NewGeyserAdminClient(st.AccessToken, st.UserID, cfg, logger)
		} else {
			if st.Hash == "" || *code == "" {
				fmt.Fprintln(os.Stderr, "need a saved challenge (run login) and -code")
				os.Exit(2)
			}
			c = drivers.NewGeyserAdminClient("", "", cfg, logger)
			if err := c.VerifyMFA(ctx, st.Hash, *code); err != nil {
				fmt.Fprintln(os.Stderr, "verify:", err)
				os.Exit(1)
			}
			st.AccessToken, st.UserID = c.SessionCookies()
			st.Hash = ""
			save()
		}
		out := map[string]any{}
		if v, err := c.GetTapeCollections(ctx); err == nil {
			out["tapeCollections"] = v
		} else {
			out["tapeCollections_err"] = err.Error()
		}
		if v, err := c.GetSites(ctx); err == nil {
			out["sites"] = v
		} else {
			out["sites_err"] = err.Error()
		}
		if v, err := c.GetDatacenters(ctx); err == nil {
			out["datacenters"] = v
		} else {
			out["datacenters_err"] = err.Error()
		}
		if v, err := c.GetEvents(ctx); err == nil {
			if len(v) > 60 {
				v = v[:60]
			}
			out["events"] = v
		} else {
			out["events_err"] = err.Error()
		}
		buckets, err := c.ListBuckets(ctx)
		if err != nil {
			out["buckets_err"] = err.Error()
		}
		for _, b := range buckets {
			if strings.HasPrefix(b.Name, *bucketPrefix) {
				out["bucket"] = b
				if st, err := c.GetBucketStatus(ctx, b.ID); err == nil {
					out["bucketStatus"] = st
				}
				if entries, err := c.BrowseBucket(ctx, b.ID, *browsePrefix); err == nil {
					out["browse"] = entries
				} else {
					out["browse_err"] = err.Error()
				}
			}
		}
		if raw, err := c.RawGet(ctx, "/tasks"); err == nil {
			out["tasks"] = json.RawMessage(raw)
		} else {
			out["tasks_err"] = err.Error()
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(out)
	}
}
