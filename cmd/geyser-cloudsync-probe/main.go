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
}
