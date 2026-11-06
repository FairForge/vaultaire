package drivers

// The permafrost driver at scale (WP-FLEET-11, docs/reviews/WP-FLEET-NEXT-
// PROMPT.md Part 5). Measured: the driver read TENANT_1..15 and built a
// credential and three HTTP transports PER SLOT; a miss walked every account
// (Get, Exists, Delete); HealthCheck probed every account inside the probe's
// 15 s. At 900 drives a miss is 900 Graph calls and the probe cannot finish.
//
//   - D-F11-1 PERMAFROST_FLEET_FILE replaces the env slots past 15: tenants
//     (directory id, client id, a secret OR a certificate — a systemd
//     credential name or a file) with their drives beneath ({index, user,
//     drive_id}). Account `index` is `tenant-<index>` and takes the same
//     place in the fleet as env slot <index> did, so moving from the slots
//     to a file changes neither placement nor the metric labels. Indexes are
//     unique; placement is by ascending index (append-only: a new drive
//     takes a new, higher index). The env slots are still read when no file
//     is set; both set = refused.
//   - D-F11-2 one credential, one token cache and one transport set per
//     TENANT, shared by its drives (env slots of one directory and app share
//     them too). The tenant's first account owns the token; the others ask it.
//   - D-F11-3 placement-authoritative reads: once the fleet fingerprint is
//     settled (match, recorded, repaired, accepted), Get / Exists / Delete
//     ask the home account only — a miss is a miss (the WP-R6-1 rule). While
//     it is mismatch or unknown the fallback walk stays, capped at
//     odFallbackWalkMax accounts, and each walk past the home is counted
//     (vaultaire_permafrost_fallback_walks_total{op}).
//   - D-F11-5 probe sampling: a HealthCheck probes at most
//     max(min(n, odProbeConcurrency), ceil(n/240)) accounts in rotation —
//     every account within the hour at 15 s intervals, a small fleet still
//     every time — plus the accounts whose last probe failed (so a recovery
//     is seen at once), and asks each tenant's token once. It reports every
//     account whose LAST probe failed.
//   - Boot: the drives are initialised and the fingerprint markers read
//     odBootConcurrency at a time (they were one after the other).
//
// Not done here, and why (recorded in the PR):
//   - D-F11-4 (List / ListDir from the drives placement names): placement is
//     a hash of the FULL path, so a folder's files — a parity folder's
//     p0..p3 — sit on different drives; no drive can be named for a prefix
//     without WP-FLEET-5's placement over (tenant, drive). List and ListDir
//     stay the union, as before.
//   - The fingerprint read by $batch: a $batch answers a /content GET with a
//     redirect, so it saves the metadata calls only; the concurrent read
//     bounds the boot instead (WP-FLEET-9 owns $batch).

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
)

const (
	// odFallbackWalkMax caps the accounts a fallback walk asks (the home
	// included) while the fleet's order is not settled.
	odFallbackWalkMax = 8
	// odBootConcurrency is the accounts initialised, and markers read, at once.
	odBootConcurrency = 16
	// odProbeSpread is the probe intervals within which every account is
	// probed (an hour at 15 s).
	odProbeSpread = 240
)

var permafrostFallbackWalks = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "vaultaire_permafrost_fallback_walks_total",
	Help: "Permafrost reads, existence checks and deletes that missed on the home account and walked other accounts (at most 8) because the fleet fingerprint was not settled (mismatch or unknown); 0 while it matches — a miss is then a miss (WP-FLEET-11 D-F11-3).",
}, []string{"op"})

func init() {
	for _, op := range []string{"get", "exists", "delete"} {
		permafrostFallbackWalks.WithLabelValues(op)
	}
}

// authoritative: the fleet's order is settled, so the home account is the
// only one that can hold an object.
func (d *OneDriveDriver) authoritative() bool {
	switch d.FleetState() {
	case FleetMatch, FleetRecorded, FleetRepaired, FleetAccepted:
		return true
	}
	return false
}

// noteWalk counts one fallback walk of op.
func noteWalk(op string) { permafrostFallbackWalks.WithLabelValues(op).Inc() }

// ── D-F11-1: the fleet file ─────────────────────────────────────────────────

// odFleetFile is PERMAFROST_FLEET_FILE.
type odFleetFile struct {
	Tenants []odFileTenant `json:"tenants"`
}

// odFileTenant is one directory and its app registration.
type odFileTenant struct {
	TenantID    string        `json:"tenant_id"`
	ClientID    string        `json:"client_id"`
	Secret      *odSecretRef  `json:"secret,omitempty"`
	Certificate *odSecretRef  `json:"certificate,omitempty"`
	Drives      []odFileDrive `json:"drives"`
}

// odSecretRef names where a secret lives: a systemd credential (under
// $CREDENTIALS_DIRECTORY) or a file. Never the secret itself.
type odSecretRef struct {
	Credential string `json:"credential,omitempty"`
	File       string `json:"file,omitempty"`
}

// odFileDrive is one fleet account.
type odFileDrive struct {
	Index   int    `json:"index"`
	User    string `json:"user"`
	DriveID string `json:"drive_id"`
}

// odAccountSpec is one account to build, from the file or an env slot.
type odAccountSpec struct {
	index    int
	tenantID string
	clientID string
	secret   string // env slots only (kept on the account as before)
	user     string
	driveID  string
	// authKey groups the accounts that share a credential and transports;
	// newCred builds that credential once.
	authKey string
	newCred func() (azcore.TokenCredential, error)
}

// errODFleetConfig is a fleet configuration that cannot be used.
var errODFleetConfig = errors.New("onedrive fleet configuration")

// readSecretRef reads a referenced secret.
func readSecretRef(ref *odSecretRef) ([]byte, error) {
	switch {
	case ref == nil:
		return nil, errors.New("no reference")
	case ref.Credential != "" && ref.File != "":
		return nil, errors.New("both a credential and a file")
	case ref.Credential != "":
		dir := os.Getenv("CREDENTIALS_DIRECTORY")
		if dir == "" || strings.ContainsAny(ref.Credential, `/\`) {
			return nil, fmt.Errorf("credential %q: no $CREDENTIALS_DIRECTORY, or not a bare name", ref.Credential)
		}
		b, err := os.ReadFile(filepath.Join(dir, ref.Credential)) // #nosec G304 -- a systemd credential name, validated above
		if err != nil {
			return nil, fmt.Errorf("credential %q: %w", ref.Credential, err)
		}
		return b, nil
	case ref.File != "":
		b, err := os.ReadFile(filepath.Clean(ref.File)) // #nosec G304 -- an operator-named secret file
		if err != nil {
			return nil, fmt.Errorf("file %s: %w", ref.File, err)
		}
		return b, nil
	}
	return nil, errors.New("empty reference")
}

// parseFleetFile validates a fleet file and returns its accounts in
// placement order (ascending index). Errors never quote a secret.
func parseFleetFile(b []byte) ([]odAccountSpec, error) {
	var f odFleetFile
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("%w: PERMAFROST_FLEET_FILE: %w", errODFleetConfig, err)
	}
	var specs []odAccountSpec
	seen := map[int]bool{}
	for ti, tn := range f.Tenants {
		where := fmt.Sprintf("tenants[%d]", ti)
		if tn.TenantID == "" || tn.ClientID == "" {
			return nil, fmt.Errorf("%w: %s: tenant_id and client_id are required", errODFleetConfig, where)
		}
		if (tn.Secret == nil) == (tn.Certificate == nil) {
			return nil, fmt.Errorf("%w: %s: exactly one of secret or certificate", errODFleetConfig, where)
		}
		if len(tn.Drives) == 0 {
			return nil, fmt.Errorf("%w: %s: no drives", errODFleetConfig, where)
		}
		tn := tn
		newCred := sync.OnceValues(func() (azcore.TokenCredential, error) { return fileTenantCred(tn) })
		for di, dr := range tn.Drives {
			if dr.Index < 1 || dr.User == "" {
				return nil, fmt.Errorf("%w: %s.drives[%d]: index ≥ 1 and user are required", errODFleetConfig, where, di)
			}
			if seen[dr.Index] {
				return nil, fmt.Errorf("%w: index %d appears twice", errODFleetConfig, dr.Index)
			}
			seen[dr.Index] = true
			specs = append(specs, odAccountSpec{
				index: dr.Index, tenantID: tn.TenantID, clientID: tn.ClientID,
				user: dr.User, driveID: strings.TrimSpace(dr.DriveID),
				authKey: tn.TenantID + "\x00" + tn.ClientID, newCred: newCred,
			})
		}
	}
	if len(specs) == 0 {
		return nil, fmt.Errorf("%w: PERMAFROST_FLEET_FILE names no drive", errODFleetConfig)
	}
	sort.Slice(specs, func(i, j int) bool { return specs[i].index < specs[j].index })
	return specs, nil
}

// fileTenantCred builds a file tenant's credential from its secret or its
// certificate (PEM: the certificate and its key).
func fileTenantCred(tn odFileTenant) (azcore.TokenCredential, error) {
	if tn.Secret != nil {
		b, err := readSecretRef(tn.Secret)
		if err != nil {
			return nil, fmt.Errorf("tenant %s secret: %w", tn.TenantID, err)
		}
		return azidentity.NewClientSecretCredential(tn.TenantID, tn.ClientID, strings.TrimSpace(string(b)), nil)
	}
	b, err := readSecretRef(tn.Certificate)
	if err != nil {
		return nil, fmt.Errorf("tenant %s certificate: %w", tn.TenantID, err)
	}
	certs, key, err := azidentity.ParseCertificates(b, nil)
	if err != nil {
		return nil, fmt.Errorf("tenant %s certificate: %w", tn.TenantID, err)
	}
	return azidentity.NewClientCertificateCredential(tn.TenantID, tn.ClientID, certs, key, nil)
}

// envAccountSpecs reads the TENANT_1..15 slots (the configuration before the
// fleet file). An incomplete slot is a Warn and skipped, as before.
func envAccountSpecs(getenv func(string) string, logger *zap.Logger) []odAccountSpec {
	var specs []odAccountSpec
	for i := 1; i <= 15; i++ {
		p := fmt.Sprintf("TENANT_%d_", i)
		tid := getenv(p + "ID")
		if tid == "" {
			continue
		}
		clientID, secret, user := getenv(p+"CLIENT_ID"), getenv(p+"SECRET"), getenv(p+"USER")
		if clientID == "" || secret == "" || user == "" {
			logger.Warn("incomplete OneDrive tenant config, skipping", zap.Int("tenant", i))
			continue
		}
		specs = append(specs, odAccountSpec{
			index: i, tenantID: tid, clientID: clientID, secret: secret, user: user,
			driveID: strings.TrimSpace(getenv(p + "DRIVE_ID")),
			authKey: tid + "\x00" + clientID + "\x00" + secret,
			newCred: func() (azcore.TokenCredential, error) {
				return azidentity.NewClientSecretCredential(tid, clientID, secret, nil)
			},
		})
	}
	return specs
}

// odFleetSpecs is the fleet's accounts: the file when PERMAFROST_FLEET_FILE
// is set, else the env slots; both = refused.
func odFleetSpecs(getenv func(string) string, logger *zap.Logger) ([]odAccountSpec, error) {
	file := strings.TrimSpace(getenv("PERMAFROST_FLEET_FILE"))
	env := envAccountSpecs(getenv, zap.NewNop())
	if file == "" {
		return envAccountSpecs(getenv, logger), nil
	}
	if len(env) > 0 || getenv("TENANT_1_ID") != "" {
		return nil, fmt.Errorf("%w: PERMAFROST_FLEET_FILE and TENANT_N_* are both set — the file replaces the slots; remove one", errODFleetConfig)
	}
	b, err := os.ReadFile(filepath.Clean(file)) // #nosec G304 -- the operator's fleet file
	if err != nil {
		return nil, fmt.Errorf("%w: PERMAFROST_FLEET_FILE: %w", errODFleetConfig, err)
	}
	return parseFleetFile(b)
}

// odClients is one tenant's transport set.
type odClients struct{ graph, cdn, upload *http.Client }

// newODClients builds a transport set (one per tenant, D-F11-2).
func newODClients() odClients {
	dial := odCachedDialContext()
	return odClients{
		graph:  &http.Client{Transport: odGraphTransport(dial, odTLSSessionCache)},
		cdn:    &http.Client{Transport: odCDNTransport(dial, odTLSSessionCache)},
		upload: &http.Client{Transport: odUploadTransport(dial, odTLSSessionCache)},
	}
}

// buildAccounts turns specs into accounts: one credential, token owner and
// transport set per authKey; pacing per identity (WP-FLEET-3).
func buildAccounts(specs []odAccountSpec, paceCfg odPaceConfig, hedgeAfter time.Duration,
	clients func() odClients, logger *zap.Logger) []*odTenant {
	type shared struct {
		cred    azcore.TokenCredential
		clients odClients
		owner   *odTenant
	}
	auths := map[string]*shared{}
	paces := map[string]*odPace{}
	var out []*odTenant
	for _, sp := range specs {
		sh, ok := auths[sp.authKey]
		if !ok {
			cred, err := sp.newCred()
			if err != nil {
				logger.Warn("failed to create OneDrive credential", zap.Int("tenant", sp.index), zap.Error(err))
				auths[sp.authKey] = nil
				continue
			}
			sh = &shared{cred: cred, clients: clients()}
			auths[sp.authKey] = sh
		}
		if sh == nil {
			continue // the tenant's credential failed: its every drive is skipped
		}
		t := &odTenant{
			name: fmt.Sprintf("tenant-%d", sp.index), tenantID: sp.tenantID, clientID: sp.clientID,
			secret: sp.secret, userUPN: sp.user, pinnedDriveID: sp.driveID, cred: sh.cred,
			graphHTTP: sh.clients.graph, cdnHTTP: sh.clients.cdn, uploadHTTP: sh.clients.upload,
			rangeHedgeAfter: hedgeAfter,
		}
		if sh.owner == nil {
			sh.owner = t
		} else {
			t.tokenOwner = sh.owner
		}
		if p, ok := paces[t.identity()]; ok {
			t.pace = p
		} else {
			t.pace = newODPace(t.name, sp.tenantID, paceCfg)
			paces[t.identity()] = t.pace
		}
		out = append(out, t)
	}
	return out
}

// ── D-F11-5: probe sampling ─────────────────────────────────────────────────

// probeBatch is the accounts one HealthCheck probes: the next k in rotation
// and every account whose last probe failed (at most odProbeConcurrency of
// those), k = max(min(n, odProbeConcurrency), ceil(n/odProbeSpread)).
func (d *OneDriveDriver) probeBatch() []int {
	n := len(d.tenants)
	k := max(min(n, odProbeConcurrency), (n+odProbeSpread-1)/odProbeSpread)
	d.probeMu.Lock()
	start := d.probeNext
	d.probeNext = (start + k) % max(n, 1)
	d.probeMu.Unlock()
	picked := make(map[int]bool, k)
	out := make([]int, 0, k)
	for i := 0; i < k; i++ {
		j := (start + i) % n
		picked[j] = true
		out = append(out, j)
	}
	again := 0
	for j, t := range d.tenants {
		if again >= odProbeConcurrency {
			break
		}
		if !picked[j] && t.probeState.Load() == 2 {
			out = append(out, j)
			again++
		}
	}
	return out
}
