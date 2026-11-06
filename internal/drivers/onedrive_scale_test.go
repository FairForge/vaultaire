package drivers

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/FairForge/vaultaire/internal/common"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// WP-FLEET-11 D-F11-1 / D-F11-2: the fleet file, one credential, token and
// transport set per tenant, a 300-drive boot inside 20 s, the env slots
// still read.

func writeFleetFile(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "fleet.json")
	require.NoError(t, os.WriteFile(p, []byte(body), 0o600))
	return p
}

func TestFleetFile_ParsesTenantsAndDrivesInIndexOrder(t *testing.T) {
	secret := filepath.Join(t.TempDir(), "s")
	require.NoError(t, os.WriteFile(secret, []byte("app-secret\n"), 0o600))
	body := fmt.Sprintf(`{"tenants":[
		{"tenant_id":"dir-b","client_id":"app-b","secret":{"file":%q},"drives":[{"index":3,"user":"c@b","drive_id":"b!c"},{"index":1,"user":"a@b"}]},
		{"tenant_id":"dir-a","client_id":"app-a","secret":{"file":%q},"drives":[{"index":2,"user":"b@a","drive_id":"b!b"}]}]}`, secret, secret)

	specs, err := parseFleetFile([]byte(body))

	require.NoError(t, err)
	require.Len(t, specs, 3)
	assert.Equal(t, []int{1, 2, 3}, []int{specs[0].index, specs[1].index, specs[2].index}, "placement by ascending index")
	assert.Equal(t, "dir-b", specs[0].tenantID)
	assert.Equal(t, "b!c", specs[2].driveID)
	assert.Equal(t, specs[0].authKey, specs[2].authKey, "one tenant, one credential")
	assert.NotEqual(t, specs[0].authKey, specs[1].authKey)
	cred, err := specs[0].newCred()
	require.NoError(t, err)
	assert.NotNil(t, cred)
}

func TestFleetFile_RefusesWhatItCannotUse(t *testing.T) {
	for name, body := range map[string]string{
		"both secret and certificate": `{"tenants":[{"tenant_id":"d","client_id":"c","secret":{"file":"/x"},"certificate":{"file":"/y"},"drives":[{"index":1,"user":"u"}]}]}`,
		"neither":                     `{"tenants":[{"tenant_id":"d","client_id":"c","drives":[{"index":1,"user":"u"}]}]}`,
		"an index twice":              `{"tenants":[{"tenant_id":"d","client_id":"c","secret":{"file":"/x"},"drives":[{"index":1,"user":"u"},{"index":1,"user":"v"}]}]}`,
		"index 0":                     `{"tenants":[{"tenant_id":"d","client_id":"c","secret":{"file":"/x"},"drives":[{"index":0,"user":"u"}]}]}`,
		"no drive":                    `{"tenants":[{"tenant_id":"d","client_id":"c","secret":{"file":"/x"},"drives":[]}]}`,
		"an inline secret field":      `{"tenants":[{"tenant_id":"d","client_id":"c","client_secret":"oops","secret":{"file":"/x"},"drives":[{"index":1,"user":"u"}]}]}`,
		"not JSON":                    `tenants:`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := parseFleetFile([]byte(body))
			require.Error(t, err)
			assert.ErrorIs(t, err, errODFleetConfig)
			assert.NotContains(t, err.Error(), "oops", "an error never quotes a secret")
		})
	}
}

func TestFleetFile_AndTheEnvSlotsTogetherAreRefused(t *testing.T) {
	p := writeFleetFile(t, `{"tenants":[{"tenant_id":"d","client_id":"c","secret":{"file":"/x"},"drives":[{"index":1,"user":"u"}]}]}`)
	env := map[string]string{"PERMAFROST_FLEET_FILE": p, "TENANT_1_ID": "d", "TENANT_1_CLIENT_ID": "c", "TENANT_1_SECRET": "s", "TENANT_1_USER": "u"}

	_, err := odFleetSpecs(func(k string) string { return env[k] }, zap.NewNop())

	require.ErrorIs(t, err, errODFleetConfig)
	assert.Contains(t, err.Error(), "both set")
}

func TestFleetFile_ACredentialIsReadFromTheCredentialsDirectory(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "fleet_dir_a"), []byte("s3cret"), 0o600))
	t.Setenv("CREDENTIALS_DIRECTORY", dir)

	b, err := readSecretRef(&odSecretRef{Credential: "fleet_dir_a"})
	require.NoError(t, err)
	assert.Equal(t, "s3cret", string(b))

	_, err = readSecretRef(&odSecretRef{Credential: "../etc/passwd"})
	require.Error(t, err, "a credential is a bare name")
}

func TestFleetEnvSlots_FifteenSlotsStillBoot(t *testing.T) {
	env := map[string]string{}
	for i := 1; i <= 15; i++ {
		p := fmt.Sprintf("TENANT_%d_", i)
		env[p+"ID"], env[p+"CLIENT_ID"], env[p+"SECRET"], env[p+"USER"] = fmt.Sprintf("dir-%d", i%3), "app", "s", fmt.Sprintf("u%d@x", i)
	}
	delete(env, "TENANT_7_USER") // an incomplete slot is skipped, as before

	specs, err := odFleetSpecs(func(k string) string { return env[k] }, zap.NewNop())

	require.NoError(t, err)
	require.Len(t, specs, 14)
	assert.Equal(t, 1, specs[0].index)
	assert.Equal(t, 15, specs[13].index)
	accts := buildAccounts(specs, odPaceConfig{}, 0, newODClients, zap.NewNop())
	require.Len(t, accts, 14)
	assert.Equal(t, "tenant-15", accts[13].name, "slot N is still tenant-N")
	graphs := map[*http.Client]bool{}
	for _, a := range accts {
		graphs[a.graphHTTP] = true
	}
	assert.Len(t, graphs, 3, "one transport set per directory and app")
}

// countingCred is a token source that counts its renewals.
type countingCred struct{ calls atomic.Int32 }

func (c *countingCred) GetToken(context.Context, policy.TokenRequestOptions) (azcore.AccessToken, error) {
	c.calls.Add(1)
	return azcore.AccessToken{Token: "tok", ExpiresOn: time.Now().Add(time.Hour)}, nil
}

// slowGraph answers the boot's calls after latency: the root folder, the
// marker reads (404) and the marker writes.
type slowGraph struct {
	latency time.Duration
	calls   atomic.Int64
}

func (g *slowGraph) RoundTrip(req *http.Request) (*http.Response, error) {
	g.calls.Add(1)
	time.Sleep(g.latency)
	if req.Body != nil {
		_, _ = io.Copy(io.Discard, req.Body)
	}
	switch {
	case req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/children"):
		return odResp(req, 201, `{"id":"ROOT"}`, nil), nil
	case req.Method == http.MethodPut:
		return odResp(req, 201, `{"id":"M","size":1}`, nil), nil
	}
	return odResp(req, 404, `{"error":{"code":"itemNotFound"}}`, nil), nil
}

func TestFleetAtScale_ThreeHundredDrivesBootInsideTwentySeconds(t *testing.T) {
	// Arrange: 300 drives in 3 tenants from a fleet file's specs; every
	// Graph call takes 50 ms (one after the other the boot would take
	// 300 × 3 × 50 ms = 45 s).
	g := &slowGraph{latency: 50 * time.Millisecond}
	creds := map[string]*countingCred{}
	var specs []odAccountSpec
	for i := 1; i <= 300; i++ {
		dir := fmt.Sprintf("dir-%d", i%3)
		c, ok := creds[dir]
		if !ok {
			c = &countingCred{}
			creds[dir] = c
		}
		specs = append(specs, odAccountSpec{index: i, tenantID: dir, clientID: "app", user: fmt.Sprintf("u%d@%s", i, dir),
			driveID: fmt.Sprintf("b!%d", i), authKey: dir,
			newCred: func() (azcore.TokenCredential, error) { return c, nil }})
	}
	clients := func() odClients {
		hc := &http.Client{Transport: g}
		return odClients{graph: hc, cdn: hc, upload: hc}
	}

	// Act
	start := time.Now()
	accts := buildAccounts(specs, odPaceConfig{}, 0, clients, zap.NewNop())
	kept := initFleetDrives(context.Background(), zap.NewNop(), accts)
	d := &OneDriveDriver{tenants: kept, logger: zap.NewNop()}
	fp, err := d.CheckFleetFingerprint(context.Background(), "")
	took := time.Since(start)

	// Assert
	require.NoError(t, err)
	assert.Len(t, kept, 300)
	assert.Equal(t, FleetRecorded, fp.State)
	assert.Less(t, took, 20*time.Second, "boot of 300 drives: %s", took)
	for dir, c := range creds {
		assert.Equal(t, int32(1), c.calls.Load(), "%s: one token for its 100 drives", dir)
	}
	assert.True(t, d.authoritative(), "a recorded fleet reads its home accounts only")
}

func TestFleetAtScale_TheTenantsTokenIsAskedOnceAndShared(t *testing.T) {
	c := &countingCred{}
	specs := []odAccountSpec{
		{index: 1, tenantID: "d", clientID: "a", user: "u1", authKey: "d", newCred: func() (azcore.TokenCredential, error) { return c, nil }},
		{index: 2, tenantID: "d", clientID: "a", user: "u2", authKey: "d", newCred: func() (azcore.TokenCredential, error) { return c, nil }},
	}
	accts := buildAccounts(specs, odPaceConfig{}, 0, newODClients, zap.NewNop())

	_, err1 := accts[0].token(context.Background())
	_, err2 := accts[1].token(context.Background())

	require.NoError(t, errors.Join(err1, err2))
	assert.Equal(t, int32(1), c.calls.Load())
	assert.Same(t, accts[0], accts[1].tokenOwner)
	assert.Same(t, accts[0].graphHTTP, accts[1].graphHTTP)
	assert.NotSame(t, accts[0].pace, accts[1].pace, "two users: two drives' buckets")
}

func TestFleetAtScale_EveryAccountIsProbedWithinTheSpread(t *testing.T) {
	d, _ := bigFleet(1000)
	d.fleetState = FleetMatch
	seen := map[int]bool{}
	for i := 0; i < odProbeSpread; i++ {
		for _, j := range d.probeBatch() {
			seen[j] = true
		}
	}
	assert.Len(t, seen, 1000, "every account within an hour of 15 s intervals")
	assert.LessOrEqual(t, len(d.probeBatch()), 16)
}

func TestFleetAtScale_TheFallbackWalksAreCounted(t *testing.T) {
	d, _ := bigFleet(20)
	d.fleetState = FleetUnknown
	before := testutil.ToFloat64(permafrostFallbackWalks.WithLabelValues("exists"))

	_, _ = d.Exists(common.WithTenantID(context.Background(), "x"), "bkt", "missing")

	assert.Equal(t, before+1, testutil.ToFloat64(permafrostFallbackWalks.WithLabelValues("exists")))
}
