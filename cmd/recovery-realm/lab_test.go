package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The MIT juicebox-sdk demo client (rust/cli/demo at 2e0f544e, built with
// `cargo build --release -p juicebox_demo_cli`) against OUR realms over the
// Noise transport: it registers, recovers, burns its guesses until the
// record self-destructs, registers again, recovers and deletes. Env-gated:
// RECOVERY_LAB_SDK_DEMO=<path to the demo binary> (docs/reviews/K5-PROBE.md).
func TestRecoveryRealm_Lab_SDKDemoClientAgainstOurRealms(t *testing.T) {
	demo := os.Getenv("RECOVERY_LAB_SDK_DEMO")
	if demo == "" {
		t.Skip("RECOVERY_LAB_SDK_DEMO not set (path to juicebox-sdk's rust/cli/demo binary)")
	}
	r1, r2 := startRealm(t, 0x0a), startRealm(t, 0x0b)
	type realmCfg struct {
		ID        string `json:"id"`
		Address   string `json:"address"`
		PublicKey string `json:"public_key"`
	}
	cfg := map[string]any{
		"realms": []realmCfg{
			{ID: r1.idHex, Address: r1.http.URL + "/", PublicKey: hex.EncodeToString(r1.srv.staticPublic[:])},
			{ID: r2.idHex, Address: r2.http.URL + "/", PublicKey: hex.EncodeToString(r2.srv.staticPublic[:])},
		},
		"register_threshold": 2, "recover_threshold": 2, "pin_hashing_mode": "Standard2019",
	}
	tokens := map[string]string{}
	for _, r := range []*testRealm{r1, r2} {
		now := time.Now()
		tok := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"iss": "stored", "sub": "lab-opaque-subject",
			"aud": r.idHex, "scope": "user", "iat": now.Unix(), "nbf": now.Unix(), "exp": now.Add(10 * time.Minute).Unix()})
		tok.Header["kid"] = "stored:1"
		s, err := tok.SignedString(tenantKey)
		require.NoError(t, err)
		tokens[r.idHex] = s
	}
	cfgJSON, _ := json.Marshal(cfg)
	tokJSON, _ := json.Marshal(tokens)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, demo, "-c", string(cfgJSON), "-a", string(tokJSON)).CombinedOutput()
	t.Logf("SDK demo client:\n%s", out)
	require.NoError(t, err, "the demo panics on any unexpected answer")
	for _, line := range []string{
		"[Rust] Register succeeded",
		"[Rust] Recovered secret: teyla21",
		"[Rust] Starting recover with correct PIN (guess 3)", // after two wrong: InvalidPin {0}, self-destructed
		"[Rust] Recovered secret: presso42",
		"[Rust] delete succeeded",
	} {
		assert.Contains(t, string(out), line)
	}
	for _, r := range []*testRealm{r1, r2} {
		assert.Equal(t, float64(1), r.destroy(), "each realm destroyed the first registration")
		rec, _, err := r.store.Load(context.Background(), "stored", "lab-opaque-subject")
		require.NoError(t, err)
		assert.False(t, rec.HoldsSecretMaterial(), "deleted at the end")
	}
}
