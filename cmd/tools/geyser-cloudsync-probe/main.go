// geyser-cloudsync-probe answers the ONE question gating the 0×-SLC Vault
// data path (V18.2): does Geyser's cloud-integration accept a custom S3
// endpoint (so RestoreToCloud can target Lyve), or is the AWS type pinned to
// real AWS?
//
// Auth (either):
//
//	GEYSER_ACCESS_TOKEN + GEYSER_USER_ID   — copied from a logged-in browser
//	                                         session (accessToken / userId cookies)
//	GEYSER_CONSOLE_PASSWORD [+ GEYSER_MFA_CODE] — programmatic login as
//	                                         GEYSER_CONSOLE_USER (default
//	                                         partner@fairforge.io). If the MFA
//	                                         code is emailed, run once to fire
//	                                         the email, then re-run with the code.
//
// The create probes use DUMMY credentials ("PROBE"), so even an accepted
// integration can read nothing; anything created is deleted immediately.
// Every response is printed verbatim — the error text IS the finding.
package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/FairForge/vaultaire/internal/drivers"
	"go.uber.org/zap"
)

func trimSpace(s string) string { return strings.TrimSpace(s) }

const (
	consoleBase     = "https://console.geyserdata.com/api"
	stagingBucketID = "632df558-9627-427b-ab86-9f3ff1eaafe9" // stored3lib (5TB Single Copy LA)
	lyveEndpoint    = "https://s3.us-west-1.global.lyve.seagate.com"
)

func rawGet(token, userID, path string) string {
	req, _ := http.NewRequest(http.MethodGet, consoleBase+path, nil)
	req.AddCookie(&http.Cookie{Name: "accessToken", Value: token})
	req.AddCookie(&http.Cookie{Name: "userId", Value: userID})
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return "ERROR: " + err.Error()
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return fmt.Sprintf("HTTP %d: %s", resp.StatusCode, string(b))
}

func main() {
	logger, _ := zap.NewDevelopment()
	ctx := context.Background()
	cfg := drivers.GeyserProvisioningConfig{}

	token := os.Getenv("GEYSER_ACCESS_TOKEN")
	userID := os.Getenv("GEYSER_USER_ID")
	var client *drivers.GeyserAdminClient

	switch {
	case token != "" && userID != "":
		client = drivers.NewGeyserAdminClient(token, userID, cfg, logger)
	case os.Getenv("GEYSER_CONSOLE_PASSWORD") != "":
		email := os.Getenv("GEYSER_CONSOLE_USER")
		if email == "" {
			email = "partner@fairforge.io"
		}
		c := drivers.NewGeyserAdminClient("", "", cfg, logger)
		challenge, err := c.Login(ctx, email, os.Getenv("GEYSER_CONSOLE_PASSWORD"))
		if err != nil {
			log.Fatalf("login: %v", err)
		}
		code := os.Getenv("GEYSER_MFA_CODE")
		if code == "" {
			// Email-MFA flow: the code only exists after Login fires, and the
			// challenge hash must be reused — so wait for the code to appear
			// in a file rather than requiring a re-login.
			codeFile := os.Getenv("GEYSER_MFA_CODE_FILE")
			if codeFile == "" {
				log.Fatal("MFA challenge issued but no GEYSER_MFA_CODE or GEYSER_MFA_CODE_FILE set")
			}
			fmt.Printf("MFA challenge issued (totpUser=%v). Waiting up to 15m for code at %s\n",
				challenge.TOTPUser, codeFile)
			deadline := time.Now().Add(15 * time.Minute)
			for {
				if b, rerr := os.ReadFile(codeFile); rerr == nil {
					code = trimSpace(string(b))
					if code != "" {
						break
					}
				}
				if time.Now().After(deadline) {
					log.Fatal("timed out waiting for MFA code file")
				}
				time.Sleep(2 * time.Second)
			}
			fmt.Println("code received, verifying…")
		}
		if err := c.VerifyMFA(ctx, challenge.Hash, code); err != nil {
			log.Fatalf("verify MFA: %v", err)
		}
		client = c
	default:
		log.Fatal("Set GEYSER_ACCESS_TOKEN + GEYSER_USER_ID (browser cookies), " +
			"or GEYSER_CONSOLE_PASSWORD (+ GEYSER_MFA_CODE) for programmatic login.")
	}

	client.StartKeepalive(ctx)
	defer client.StopKeepalive()
	time.Sleep(500 * time.Millisecond)

	bucketID := os.Getenv("GEYSER_BUCKET_ID")
	if bucketID == "" {
		bucketID = stagingBucketID
	}

	fmt.Println("== 0. session sanity")
	status, err := client.GetBucketStatus(ctx, bucketID)
	if err != nil {
		log.Fatalf("session/bucket check failed (token expired?): %v", err)
	}
	fmt.Printf("   bucket %q status=%s\n\n", status.Name, status.Status)

	// The cookies the raw GETs need — same values the client holds.
	if token == "" {
		token, userID = client.SessionCookies()
	}

	fmt.Println("== 1. supported regions per integration type (read-only)")
	for _, p := range []string{"/supportedregions", "/supportedregions/wasabi", "/supportedregions/oracle"} {
		fmt.Printf("-- GET %s\n   %s\n", p, rawGet(token, userID, p))
	}

	fmt.Println("\n== 2. existing integrations on staging bucket")
	ints, err := client.ListCloudIntegrations(ctx, bucketID)
	fmt.Printf("   list: %+v err=%v\n", ints, err)

	fmt.Println("\n== 3. create probes (dummy creds, deleted on success)")
	probes := []drivers.CreateCloudIntegrationRequest{
		{CloudIntegrationType: "AWS", Region: "us-west-1", Bucket: "stored-us-west-1",
			AccessKey: "PROBE", SecretKey: "PROBE", Endpoint: lyveEndpoint},
		{CloudIntegrationType: "AWS", Region: "us-west-1", Bucket: "stored-us-west-1",
			AccessKey: "PROBE", SecretKey: "PROBE"}, // no endpoint — baseline validation shape
		{CloudIntegrationType: "WASABI", Region: "us-west-1", Bucket: "stored-us-west-1",
			AccessKey: "PROBE", SecretKey: "PROBE", Endpoint: lyveEndpoint},
	}
	for i, p := range probes {
		fmt.Printf("-- probe %d: type=%s endpoint=%q\n", i+1, p.CloudIntegrationType, p.Endpoint)
		created, err := client.CreateCloudIntegration(ctx, bucketID, p)
		if err != nil {
			fmt.Printf("   rejected: %v\n", err)
			continue
		}
		fmt.Printf("   ACCEPTED: %+v\n", created)
		if created != nil && created.ID != "" {
			if derr := client.DeleteCloudIntegration(ctx, bucketID, created.ID); derr != nil {
				fmt.Printf("   !! cleanup delete failed (id=%s): %v — remove via console\n", created.ID, derr)
			} else {
				fmt.Printf("   cleaned up (deleted %s)\n", created.ID)
			}
		}
	}
	fmt.Println("\nDone. The verdict is in the responses above: an 'endpoint' field that is",
		"accepted (or an error complaining about credentials rather than the field)",
		"means Geyser can target Lyve; 'unknown field'/enum errors mean AWS-pinned.")

	if os.Getenv("GEYSER_FUNCTIONAL") == "1" {
		runFunctional(ctx, client, bucketID)
	}
}

// runFunctional is the decisive test: real scoped Lyve creds, a real
// RestoreToCloud, and direct verification that the object lands on Lyve —
// plus the cloudSync ingest-leg probe. Requires (set by the wrapper script):
//
//	LYVE_PROBE_BUCKET   public-read Lyve bucket (us-west-1 homed)
//	LYVE_PROBE_AK/SK    scoped creds (Put/Get/List on that bucket only)
//	GEYSER_STAGED_KEY   object already staged on Geyser's staging disk
//	LYVE_SYNC_SRC_KEY   object already present in the Lyve probe bucket
func runFunctional(ctx context.Context, client *drivers.GeyserAdminClient, bucketID string) {
	lyveBucket := os.Getenv("LYVE_PROBE_BUCKET")
	ak, sk := os.Getenv("LYVE_PROBE_AK"), os.Getenv("LYVE_PROBE_SK")
	stagedKey := os.Getenv("GEYSER_STAGED_KEY")
	syncSrcKey := os.Getenv("LYVE_SYNC_SRC_KEY")
	if lyveBucket == "" || ak == "" || sk == "" || stagedKey == "" {
		log.Fatal("functional mode needs LYVE_PROBE_BUCKET, LYVE_PROBE_AK/SK, GEYSER_STAGED_KEY")
	}

	fmt.Println("\n== F1. RESTORE LEG: RestoreToCloud into Lyve, verified by public GET")
	restored := false
	for _, typ := range []string{"AWS", "WASABI"} {
		fmt.Printf("-- integration type=%s endpoint=%s bucket=%s\n", typ, lyveEndpoint, lyveBucket)
		integ, err := client.CreateCloudIntegration(ctx, bucketID, drivers.CreateCloudIntegrationRequest{
			CloudIntegrationType: typ, Region: "us-west-1", Bucket: lyveBucket,
			AccessKey: ak, SecretKey: sk, Endpoint: lyveEndpoint,
		})
		if err != nil {
			fmt.Printf("   create rejected: %v\n", err)
			continue
		}
		fmt.Printf("   integration %s created\n", integ.ID)

		if rerr := client.RestoreToCloud(ctx, bucketID, stagedKey, integ.ID, ""); rerr != nil {
			fmt.Printf("   RestoreToCloud error: %v — trying RestoreToCache first\n", rerr)
			if cerr := client.RestoreToCache(ctx, bucketID, stagedKey, ""); cerr != nil {
				fmt.Printf("   RestoreToCache error: %v\n", cerr)
			}
			if rerr2 := client.RestoreToCloud(ctx, bucketID, stagedKey, integ.ID, ""); rerr2 != nil {
				fmt.Printf("   RestoreToCloud retry error: %v\n", rerr2)
			}
		} else {
			fmt.Println("   RestoreToCloud accepted, polling Lyve for the object (5m)…")
		}

		// The probe bucket is public-read: poll without credentials.
		u := fmt.Sprintf("%s/%s/%s", lyveEndpoint, lyveBucket, stagedKey)
		deadline := time.Now().Add(5 * time.Minute)
		for time.Now().Before(deadline) && !restored {
			resp, gerr := http.Get(u) // #nosec G107 -- URL built from our own env
			if gerr == nil {
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
				if resp.StatusCode == 200 {
					fmt.Printf("   ✅ OBJECT LANDED ON LYVE: %s (via %s integration)\n", u, typ)
					restored = true
					break
				}
			}
			time.Sleep(10 * time.Second)
		}
		if derr := client.DeleteCloudIntegration(ctx, bucketID, integ.ID); derr != nil {
			fmt.Printf("   !! integration cleanup failed (%s): %v\n", integ.ID, derr)
		}
		if restored {
			break
		}
		fmt.Printf("   no object appeared within 5m via type=%s\n", typ)
	}
	if !restored {
		fmt.Println("   ❌ RESTORE LEG NOT CONFIRMED — endpoint may be dropped, or restore targets real AWS")
	}

	if syncSrcKey != "" {
		fmt.Println("\n== F2. INGEST LEG: cloudSync pulling FROM Lyve (endpoint injected)")
		err := client.CreateCloudSync(ctx, drivers.CreateCloudSyncRequest{
			Source: drivers.CloudSyncSource{
				Type: "AWS", Region: "us-west-1", Bucket: lyveBucket,
				AccessKey: ak, SecretKey: sk, Endpoint: lyveEndpoint,
			},
			Action:   "SYNC",
			BucketID: bucketID,
		})
		if err != nil {
			fmt.Printf("   cloudSync create rejected: %v\n", err)
		} else {
			fmt.Println("   cloudSync created, polling job status (5m)…")
			deadline := time.Now().Add(5 * time.Minute)
			for time.Now().Before(deadline) {
				jobs, jerr := client.GetCloudSyncStatus(ctx, bucketID)
				if jerr != nil {
					fmt.Printf("   status error: %v\n", jerr)
					break
				}
				if len(jobs) > 0 {
					last := jobs[len(jobs)-1]
					fmt.Printf("   job %s status=%s\n", last.ID, last.Status)
					if last.Status == "COMPLETED" {
						fmt.Println("   ✅ INGEST SYNC COMPLETED — wrapper verifies the object on Geyser")
						break
					}
					if last.Status == "FAILED" || last.Status == "ERROR" {
						break
					}
				}
				time.Sleep(15 * time.Second)
			}
		}
	}
	fmt.Println("\nFUNCTIONAL DONE")
}
