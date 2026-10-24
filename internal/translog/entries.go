package translog

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"time"
)

// The entry kinds. Entries become PUBLIC (GET /translog/…, then
// trust.stored.cloud), so each kind is a typed struct with an allow-list of
// fields, and none of them — by name or by value — may name a tenant, a
// person, an e-mail, an IP address or an object
// (TestTranslog_EntriesNeverNameATenant). Entry is sealed: a new kind is a
// new struct in this file, with its allow-list and its checks.
const (
	// KindServerBuild: a server binary started on the box (TRUST.md item
	// 13; no fTPM quote yet) — one entry per distinct (git sha, binary
	// sha256), appended at boot.
	KindServerBuild = "server_build"
	// KindKeyCeremony: a key ceremony (F-14 master-key install, re-wrap,
	// escrow share rotation; F-19 recovery realm; this log's own key).
	KindKeyCeremony = "key_ceremony"
	// KindAdminRead: an operator read customer metadata (contract rule 7,
	// K4c-1): the scope read and an opaque commitment to the tenant read,
	// never the tenant, the operator or what was seen.
	KindAdminRead = "admin_read"
	// KindLogPolicy: a write of a translog_* feature-flag row — the switch
	// of what the log records and who may read it (Stage 1a Prompt 34a,
	// RP44-2): appended BEFORE the row is written, never naming the tenant.
	KindLogPolicy = "log_policy"
	// KindClientBuild: a client artefact release (K4c-3b, Stage 1a Prompt
	// 36; plan 43.6 "clients refuse an unlisted build", TRUST.md item 3) —
	// one entry per distinct (client, sha256), from the reviewed manifest
	// (internal/translog/clientbuilds), appended at boot like server_build.
	KindClientBuild = "client_build"

	// entryVersion is the body's "v": a change of any kind's fields is a
	// new version — except key_ceremony's v1 schema of #884 (a free-text
	// statement), which was replaced before any ceremony entry existed
	// (RP37-10: 0 on prod at the change), so v1 is the schema below.
	entryVersion = 1
)

// The actions a key_ceremony entry can record (docs/TRANSPARENCY.md
// § Ceremony, docs/ENCRYPTION.md). A closed list: a ceremony that fits none
// of them is a code change, never free text.
const (
	ActionKeyGenerated  = "key_generated"  // a key was generated (on the box, offline)
	ActionKeyInstalled  = "key_installed"  // sealed as the service's credential (TPM)
	ActionKeyRewrapped  = "key_rewrapped"  // tenant keys re-wrapped under it (master re-key)
	ActionKeyRotated    = "key_rotated"    // it replaced the previous key of its role
	ActionKeyRetired    = "key_retired"    // destroyed: its credential and its shares
	ActionEscrowSplit   = "escrow_split"   // split into trustee shares (shares, threshold)
	ActionEscrowTested  = "escrow_tested"  // threshold shares recombined to the same fingerprint
	ActionRealmEnrolled = "realm_enrolled" // a recovery realm enrolled with it (F-19)
	ActionLogRestored   = "log_restored"   // the database was restored behind a signed checkpoint
)

// The keys a key_ceremony entry can name, by role.
const (
	KeyMasterKey          = "master_key"           // ENCRYPTION_MASTER_KEY (F-14)
	KeyTranslogSigningKey = "translog_signing_key" // this log's checkpoint key
	KeyRecoveryRealmKey   = "recovery_realm_key"   // a recovery realm's key (F-19)
)

var (
	ceremonyActions = []string{ActionKeyGenerated, ActionKeyInstalled, ActionKeyRewrapped, ActionKeyRotated, ActionKeyRetired,
		ActionEscrowSplit, ActionEscrowTested, ActionRealmEnrolled, ActionLogRestored}
	keyRoles = []string{KeyMasterKey, KeyTranslogSigningKey, KeyRecoveryRealmKey}
)

// CeremonyActions lists the actions, for a tool's usage text.
func CeremonyActions() []string { return append([]string(nil), ceremonyActions...) }

// KeyRoles lists the key roles.
func KeyRoles() []string { return append([]string(nil), keyRoles...) }

// Entry is one log entry of a known kind.
type Entry interface {
	// Kind is the entry's kind (translog_entries.kind and the body's "kind").
	Kind() string
	// DedupKey, when not empty, makes the entry appear once: a second
	// append of the same key returns the first entry's index.
	DedupKey() string
	validate() error
}

// ServerBuild is a server binary that started.
type ServerBuild struct {
	GitSHA       string `json:"git_sha"`       // api.BuildSHA (12 hex on a deploy; "dev" unstamped)
	BinarySHA256 string `json:"binary_sha256"` // SHA-256 of os.Executable(), 64 hex
	BuildTime    string `json:"build_time"`    // api.BuildDate, RFC 3339 or "unknown"
	GoVersion    string `json:"go_version"`    // runtime.Version()
}

// Kind implements Entry.
func (ServerBuild) Kind() string { return KindServerBuild }

// DedupKey: one entry per distinct (git sha, binary).
func (b ServerBuild) DedupKey() string {
	return KindServerBuild + ":" + b.GitSHA + ":" + b.BinarySHA256
}

var (
	reGitSHA = regexp.MustCompile(`^([0-9a-f]{7,40}|dev|unknown)$`)
	reSHA256 = regexp.MustCompile(`^[0-9a-f]{64}$`)
	reGoVer  = regexp.MustCompile(`^(devel )?go[0-9][0-9A-Za-z.+-]{0,40}$`)
	reFinger = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

func (b ServerBuild) validate() error {
	if !reGitSHA.MatchString(b.GitSHA) {
		return errors.New("git_sha: 7–40 lowercase hex, dev or unknown")
	}
	if !reSHA256.MatchString(b.BinarySHA256) {
		return errors.New("binary_sha256: 64 lowercase hex")
	}
	if b.BuildTime != "unknown" {
		if _, err := time.Parse(time.RFC3339, b.BuildTime); err != nil {
			return errors.New("build_time: RFC 3339 or unknown")
		}
	}
	if !reGoVer.MatchString(b.GoVersion) {
		return errors.New("go_version: a Go version string")
	}
	return nil
}

// The clients a client_build entry can name (a CLOSED list: a new client is
// a code change here and a manifest row, never free text).
const (
	ClientSDKWasm = "sdk-wasm" // sdk/go/e2ee compiled to WebAssembly (sdk/wasm → e2ee.wasm)
	ClientSDKJS   = "sdk-js"   // the JS binding that loads it (sdk/js/e2ee/index.mjs)
)

var buildClients = []string{ClientSDKWasm, ClientSDKJS}

// BuildClients lists the clients.
func BuildClients() []string { return append([]string(nil), buildClients...) }

// ClientBuild is a client artefact release: the hash a loader (and
// translog-verify -client) checks an artefact against. Every field is a
// closed list or a fixed shape (RP37-10): no free text, no person, no
// tenant. The recipe is a script path in this repository at the source
// commit, so anyone can rebuild the artefact and compare
// (scripts/build-client.sh; reproducibility is gated in the Deploy job by
// scripts/repro-check-clients.sh).
type ClientBuild struct {
	Client       string `json:"client"`        // one of BuildClients()
	Version      string `json:"version"`       // the release's version, MAJOR.MINOR.PATCH
	SourceCommit string `json:"source_commit"` // the commit the artefact builds from, 40 hex
	SHA256       string `json:"sha256"`        // SHA-256 of the artefact as published, 64 hex
	Toolchain    string `json:"toolchain"`     // the Go toolchain the recipe pins (go.mod's), or "none" for a file published as-is
	Recipe       string `json:"recipe"`        // the build script's path at the source commit
}

// Kind implements Entry.
func (ClientBuild) Kind() string { return KindClientBuild }

// DedupKey: one entry per distinct (client, sha256), never twice.
func (b ClientBuild) DedupKey() string {
	return KindClientBuild + ":" + b.Client + ":" + b.SHA256
}

var (
	reVersion = regexp.MustCompile(`^[0-9]{1,4}\.[0-9]{1,4}\.[0-9]{1,4}$`)
	reCommit  = regexp.MustCompile(`^[0-9a-f]{40}$`)
	reRecipe  = regexp.MustCompile(`^scripts/[a-z0-9._-]{1,60}\.sh$`)
)

func (b ClientBuild) validate() error {
	if !contains(buildClients, b.Client) {
		return fmt.Errorf("client: one of %s", strings.Join(buildClients, ", "))
	}
	if !reVersion.MatchString(b.Version) {
		return errors.New("version: MAJOR.MINOR.PATCH")
	}
	if !reCommit.MatchString(b.SourceCommit) {
		return errors.New("source_commit: 40 lowercase hex")
	}
	if !reSHA256.MatchString(b.SHA256) {
		return errors.New("sha256: 64 lowercase hex")
	}
	if b.Toolchain != "none" && !reGoVer.MatchString(b.Toolchain) {
		return errors.New("toolchain: a Go version string, or none")
	}
	if !reRecipe.MatchString(b.Recipe) {
		return errors.New("recipe: scripts/<name>.sh")
	}
	return nil
}

// KeyCeremony is a key ceremony. Every field is enumerated, a number or a
// fixed shape (RP37-10): the log is public and permanent (the tables refuse
// DELETE), so there is no free text in which a name, an address or a tenant
// could be written. It names the key by role and fingerprint — never the
// key, a share or a participant. What a person would say goes in the
// referenced document, which we can correct; the entry cannot be.
type KeyCeremony struct {
	Action         string `json:"action"`          // one of CeremonyActions()
	KeyRole        string `json:"key_role"`        // one of KeyRoles()
	KeyFingerprint string `json:"key_fingerprint"` // "sha256:" + 64 hex
	Participants   int    `json:"participants"`    // how many people took part (1–20)
	Shares         int    `json:"shares"`          // trustee shares written (0 = none, else 2–20)
	Threshold      int    `json:"threshold"`       // shares that recombine the key (0 with no shares)
	// log_restored only (0 / false otherwise): the size of the newest tree
	// the key had signed (the high-water file), the size the restored
	// database holds, and whether the log continues under a new origin.
	SignedSize   int64 `json:"signed_size"`
	RestoredSize int64 `json:"restored_size"`
	NewOrigin    bool  `json:"new_origin"`
	// Reference is "" or a page of ours that says more: a pull request, a
	// commit, or a docs/ file at a commit of github.com/FairForge/vaultaire
	// (reReference) — never a URL with free words in it.
	Reference string `json:"reference"`
}

// Kind implements Entry.
func (KeyCeremony) Kind() string { return KindKeyCeremony }

// DedupKey: every ceremony is its own entry.
func (KeyCeremony) DedupKey() string { return "" }

// reReference: the only URLs a ceremony may point at. Fixed-length hex
// (a full 40-hex commit) so no short id fits; a docs path is upper-case
// file names only.
var reReference = regexp.MustCompile(`^https://github\.com/FairForge/vaultaire/(pull/[1-9][0-9]{0,5}|commit/[0-9a-f]{40}|blob/[0-9a-f]{40}/docs/[A-Z][A-Z0-9_]{0,40}\.md)$`)

func (c KeyCeremony) validate() error {
	if !contains(ceremonyActions, c.Action) {
		return fmt.Errorf("action: one of %s", strings.Join(ceremonyActions, ", "))
	}
	if !contains(keyRoles, c.KeyRole) {
		return fmt.Errorf("key_role: one of %s", strings.Join(keyRoles, ", "))
	}
	if !reFinger.MatchString(c.KeyFingerprint) {
		return errors.New("key_fingerprint: sha256: followed by 64 lowercase hex")
	}
	if c.Participants < 1 || c.Participants > 20 {
		return errors.New("participants: 1–20")
	}
	switch {
	case c.Shares == 0 && c.Threshold == 0:
		if c.Action == ActionEscrowSplit || c.Action == ActionEscrowTested {
			return fmt.Errorf("shares: %s names its shares (2–20) and threshold", c.Action)
		}
	case c.Shares < 2 || c.Shares > 20:
		return errors.New("shares: 0, or 2–20")
	case c.Threshold < 1 || c.Threshold > c.Shares:
		return errors.New("threshold: 1–shares")
	}
	if c.Action == ActionLogRestored {
		if c.KeyRole != KeyTranslogSigningKey {
			return errors.New("key_role: log_restored is the translog_signing_key's")
		}
		if c.SignedSize < 1 || c.RestoredSize < 0 || c.RestoredSize >= c.SignedSize {
			return errors.New("signed_size, restored_size: the database restored behind the newest signed tree (0 ≤ restored < signed)")
		}
	} else if c.SignedSize != 0 || c.RestoredSize != 0 || c.NewOrigin {
		return errors.New("signed_size, restored_size, new_origin: log_restored only")
	}
	if c.Reference != "" && !reReference.MatchString(c.Reference) {
		return errors.New("reference: empty, or https://github.com/FairForge/vaultaire/ pull/<n>, commit/<40 hex> or blob/<40 hex>/docs/<NAME>.md")
	}
	return nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// The scopes an admin_read entry can record (docs/TRANSPARENCY.md § Admin
// reads; the inventory of the admin routes is in docs/reviews/WP-P33.md and
// the classification maps beside the routes). A closed list: a new admin
// page that reads customer metadata is a new scope here, never free text.
// The six subject scopes (subjectScopes) read ONE identified tenant and
// carry a commitment to it; every other scope reads across tenants, or a
// record that names no tenant, and carries none.
const (
	ScopeTenantDetail   = "tenant_detail"       // one tenant's record: account, usage, keys, buckets, billing
	ScopeTenantPayments = "tenant_payments"     // one tenant's entitlement and payment ledger
	ScopeTenantAudit    = "tenant_audit"        // one tenant's (or one user's) audit trail, viewed
	ScopeTenantAuditCSV = "tenant_audit_export" // the same, exported as CSV (RP44-4: its own entry)
	ScopeSupportCase    = "support_case"        // one customer's support view and notes
	ScopeAbuseCase      = "abuse_case"          // one abuse report and the tenant it names
	ScopeAbuseReport    = "abuse_report"        // one abuse report that names no tenant (the reporter's e-mail, the URL)
	ScopeTenantList     = "tenant_list"         // the tenant / customer list (e-mails, names)
	ScopeAuditList      = "audit_list"          // the audit trail across tenants
	ScopeAuditExport    = "audit_export"        // its CSV export
	ScopeWaitlistList   = "waitlist_list"       // the waitlist (e-mails)
	ScopeWaitlistExport = "waitlist_export"     // its CSV export
	ScopeAbuseList      = "abuse_list"          // the abuse reports across tenants
	ScopeBillingList    = "billing_list"        // revenue, costs and margins across tenants with their identity
	ScopeUsageList      = "usage_list"          // per-tenant usage figures (dedup) with their identity
	ScopeFlagList       = "flag_list"           // the feature flags with the tenants they are set for
	ScopeJobList        = "job_list"            // the background jobs' results, which name tenants
	ScopeNoticeList     = "notice_list"         // the operator notifications, which name tenants
	ScopeRoutingRows    = "routing_rows"        // routing-truth's NULL rows resolved: tenant, bucket, key (RP43-6)
	ScopeChunkList      = "chunk_list"          // the chunk-orphan dry run's blob keys, under tenants' prefixes (RP43-6)
)

// ActorRoleAdmin is the only actor an admin_read names: a role, never the
// person (the operator stays in audit_logs).
const ActorRoleAdmin = "admin"

var (
	subjectScopes = []string{ScopeTenantDetail, ScopeTenantPayments, ScopeTenantAudit, ScopeTenantAuditCSV, ScopeSupportCase,
		ScopeAbuseCase}
	adminReadScopes = append(append([]string(nil), subjectScopes...), ScopeAbuseReport,
		ScopeTenantList, ScopeAuditList, ScopeAuditExport, ScopeWaitlistList, ScopeWaitlistExport, ScopeAbuseList,
		ScopeBillingList, ScopeUsageList, ScopeFlagList, ScopeJobList, ScopeNoticeList, ScopeRoutingRows, ScopeChunkList)
)

// AdminReadScopes lists the scopes.
func AdminReadScopes() []string { return append([]string(nil), adminReadScopes...) }

// ScopeHasSubject reports whether scope reads ONE identified tenant (its
// entry carries a commitment) — false for a list, an export, or a record
// that names no tenant.
func ScopeHasSubject(scope string) bool { return contains(subjectScopes, scope) }

// AdminRead is an operator's read of customer metadata. Every field is a
// closed list or a fixed shape (RP37-10): no tenant id, no e-mail, no
// object key, no IP, no operator. For a read of one tenant, Nonce is 16
// random bytes and Commitment = HMAC-SHA256(salt of that tenant,
// AdminReadLabel ‖ nonce) — the salt is translog_subjects', never public, so
// two entries cannot be linked to one tenant by anyone but the server (for
// that tenant), and by no one once the salt is erased with the account. The
// field is "commitment", not "subject": the public log's field names may not
// read as naming someone (TestTranslog_EntriesNeverNameATenant).
type AdminRead struct {
	Scope      string `json:"scope"`      // one of AdminReadScopes()
	ActorRole  string `json:"actor_role"` // ActorRoleAdmin
	Nonce      string `json:"nonce"`      // 32 lowercase hex; "" when the scope reads no one tenant
	Commitment string `json:"commitment"` // 64 lowercase hex; "" when the scope reads no one tenant
}

// AdminReadLabel is the commitment's domain label (the HMAC's message is
// the label's bytes then the nonce's 16 raw bytes).
const AdminReadLabel = "stored.cloud/translog/admin_read"

// Kind implements Entry.
func (AdminRead) Kind() string { return KindAdminRead }

// DedupKey: none of its own; the producer coalesces with CoalescedAdminRead.
func (AdminRead) DedupKey() string { return "" }

var (
	reNonce = regexp.MustCompile(`^[0-9a-f]{32}$`)
	reHMAC  = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

func (a AdminRead) validate() error {
	if !contains(adminReadScopes, a.Scope) {
		return fmt.Errorf("scope: one of %s", strings.Join(adminReadScopes, ", "))
	}
	if a.ActorRole != ActorRoleAdmin {
		return errors.New("actor_role: admin")
	}
	if ScopeHasSubject(a.Scope) {
		if !reNonce.MatchString(a.Nonce) || !reHMAC.MatchString(a.Commitment) {
			return fmt.Errorf("nonce, commitment: %s reads one tenant — 32 and 64 lowercase hex", a.Scope)
		}
	} else if a.Nonce != "" || a.Commitment != "" {
		return fmt.Errorf("nonce, commitment: %s reads across tenants — both empty", a.Scope)
	}
	return nil
}

// CoalescedAdminRead is a appended under a coalescing key (K4c-1: one entry
// per operator, scope and tenant per hour — a page refresh appends
// nothing). The key lives in translog_entries.dedup_key, never in the
// public body; the caller derives it so that it names no one either (keyed
// by the tenant's salt for a subject scope).
func CoalescedAdminRead(a AdminRead, key string) Entry {
	return coalescedAdminRead{AdminRead: a, key: key}
}

type coalescedAdminRead struct {
	AdminRead
	key string
}

func (c coalescedAdminRead) DedupKey() string {
	if c.key == "" {
		return ""
	}
	return KindAdminRead + ":" + c.key
}

// The translog_* flags a log_policy entry can name (internal/api's flag
// keys; a closed list — a new switch of the log is a code change here).
const (
	FlagTranslogPublic       = "translog_public"        // the read side on the trust host (global)
	FlagTranslogAdminReads   = "translog_admin_reads"   // admin reads of customer metadata appended (K4c-1)
	FlagTranslogCustomerView = "translog_customer_view" // a tenant's view of its own entries (K4c-1)
)

// What a log_policy entry records of the row's write, and of which row.
const (
	PolicyActionSet   = "set"   // the row was written: Enabled is its value
	PolicyActionClear = "clear" // the row was removed (the tenant follows the global row, the global row its default)
	PolicyScopeGlobal = "global"
	PolicyScopeTenant = "tenant"
)

var (
	policyFlags   = []string{FlagTranslogPublic, FlagTranslogAdminReads, FlagTranslogCustomerView}
	policyActions = []string{PolicyActionSet, PolicyActionClear}
	policyScopes  = []string{PolicyScopeGlobal, PolicyScopeTenant}
)

// PolicyFlags lists the flags a log_policy entry can name.
func PolicyFlags() []string { return append([]string(nil), policyFlags...) }

// LogPolicy is a write of a translog_* flag row (RP44-2; contract rule 7:
// switching off the record of reads is itself an access-widening act).
// Every field is a closed list or a fixed shape (RP37-10): no tenant id, no
// operator, no free text. A tenant row's write carries, like admin_read, a
// random 16-byte Nonce and Commitment = HMAC-SHA256(the tenant's salt,
// LogPolicyLabel ‖ nonce), so the tenant finds it in its own view and the
// public cannot link it; a global row's carries neither.
type LogPolicy struct {
	Flag       string `json:"flag"`       // one of PolicyFlags()
	Action     string `json:"action"`     // set | clear
	Enabled    bool   `json:"enabled"`    // the row's value written (false for a clear)
	Scope      string `json:"scope"`      // global | tenant
	Nonce      string `json:"nonce"`      // 32 lowercase hex for a tenant row; "" for the global row
	Commitment string `json:"commitment"` // 64 lowercase hex for a tenant row; "" for the global row
}

// LogPolicyLabel is the log_policy commitment's domain label.
const LogPolicyLabel = "stored.cloud/translog/log_policy"

// Kind implements Entry.
func (LogPolicy) Kind() string { return KindLogPolicy }

// DedupKey: none — every write of a switch is its own entry.
func (LogPolicy) DedupKey() string { return "" }

func (p LogPolicy) validate() error {
	switch {
	case !contains(policyFlags, p.Flag):
		return fmt.Errorf("flag: one of %s", strings.Join(policyFlags, ", "))
	case !contains(policyActions, p.Action):
		return errors.New("action: set or clear")
	case p.Action == PolicyActionClear && p.Enabled:
		return errors.New("enabled: a clear writes no value")
	case !contains(policyScopes, p.Scope):
		return errors.New("scope: global or tenant")
	case p.Scope == PolicyScopeTenant && (!reNonce.MatchString(p.Nonce) || !reHMAC.MatchString(p.Commitment)):
		return errors.New("nonce, commitment: a tenant row — 32 and 64 lowercase hex")
	case p.Scope == PolicyScopeGlobal && (p.Nonce != "" || p.Commitment != ""):
		return errors.New("nonce, commitment: the global row names no tenant — both empty")
	}
	return nil
}

// RetryableCeremony is c appended under a retry key (RP37-14, the ceremony
// route's Idempotency-Key): its DedupKey makes a second append of the same
// key return the first entry's index, in Append's own transaction — a
// client retrying after a commit it never saw appends nothing. The key is
// hashed with the actor by the caller; it lives in translog_entries.dedup_key,
// never in the public body (the body is c's, byte for byte).
func RetryableCeremony(c KeyCeremony, retryKey string) Entry {
	return retryableCeremony{KeyCeremony: c, key: retryKey}
}

type retryableCeremony struct {
	KeyCeremony
	key string
}

func (r retryableCeremony) DedupKey() string {
	if r.key == "" {
		return ""
	}
	return KindKeyCeremony + ":" + r.key
}

// ParseKeyCeremony reads a ceremony from JSON strictly: a field outside the
// allow-list (the removed "statement" included) is refused, then the entry
// is validated.
func ParseKeyCeremony(raw []byte) (KeyCeremony, error) {
	var c KeyCeremony
	if err := strictUnmarshal(raw, &c); err != nil {
		return KeyCeremony{}, fmt.Errorf("translog: key_ceremony: %w", err)
	}
	if err := c.validate(); err != nil {
		return KeyCeremony{}, fmt.Errorf("translog: key_ceremony entry refused: %w", err)
	}
	return c, nil
}

func strictUnmarshal(raw []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("trailing data")
	}
	return nil
}

// Kinds lists every entry kind.
func Kinds() []string {
	return []string{KindServerBuild, KindKeyCeremony, KindAdminRead, KindLogPolicy, KindClientBuild}
}

// AllowedFields is a kind's allow-list: the JSON fields of its "entry".
func AllowedFields(kind string) []string {
	switch kind {
	case KindServerBuild:
		return []string{"git_sha", "binary_sha256", "build_time", "go_version"}
	case KindKeyCeremony:
		return []string{"action", "key_role", "key_fingerprint", "participants", "shares", "threshold",
			"signed_size", "restored_size", "new_origin", "reference"}
	case KindAdminRead:
		return []string{"scope", "actor_role", "nonce", "commitment"}
	case KindLogPolicy:
		return []string{"flag", "action", "enabled", "scope", "nonce", "commitment"}
	case KindClientBuild:
		return []string{"client", "version", "source_commit", "sha256", "toolchain", "recipe"}
	}
	return nil
}

type envelope struct {
	Kind  string `json:"kind"`
	V     int    `json:"v"`
	At    string `json:"at"`
	Entry Entry  `json:"entry"`
}

// Encode is the body of entry e recorded at at — the exact bytes hashed:
// {"kind":…,"v":1,"at":"<RFC 3339 UTC, seconds>","entry":{…}}. An entry that
// fails its checks is refused.
func Encode(e Entry, at time.Time) ([]byte, error) {
	if e == nil || reflect.ValueOf(e).Kind() != reflect.Struct {
		return nil, errors.New("translog: not an entry")
	}
	if err := e.validate(); err != nil {
		return nil, fmt.Errorf("translog: %s entry refused: %w", e.Kind(), err)
	}
	body, err := json.Marshal(envelope{Kind: e.Kind(), V: entryVersion, At: at.UTC().Truncate(time.Second).Format(time.RFC3339), Entry: e})
	if err != nil {
		return nil, fmt.Errorf("translog: encode %s: %w", e.Kind(), err)
	}
	if len(body) > MaxEntrySize {
		return nil, ErrEntryTooLarge
	}
	return body, nil
}

// Decoded is a body read back.
type Decoded struct {
	Kind  string
	At    time.Time
	Entry Entry
}

// Decode parses a body Encode wrote (a verifier's view of an entry).
func Decode(body []byte) (Decoded, error) {
	var raw struct {
		Kind  string          `json:"kind"`
		V     int             `json:"v"`
		At    string          `json:"at"`
		Entry json.RawMessage `json:"entry"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return Decoded{}, fmt.Errorf("translog: decode: %w", err)
	}
	if raw.V != entryVersion {
		return Decoded{}, fmt.Errorf("translog: decode: version %d", raw.V)
	}
	at, err := time.Parse(time.RFC3339, raw.At)
	if err != nil {
		return Decoded{}, fmt.Errorf("translog: decode: at: %w", err)
	}
	d := Decoded{Kind: raw.Kind, At: at}
	switch raw.Kind {
	case KindServerBuild:
		var b ServerBuild
		err = strictUnmarshal(raw.Entry, &b)
		d.Entry = b
	case KindKeyCeremony:
		var c KeyCeremony
		err = strictUnmarshal(raw.Entry, &c)
		d.Entry = c
	case KindAdminRead:
		var a AdminRead
		err = strictUnmarshal(raw.Entry, &a)
		d.Entry = a
	case KindLogPolicy:
		var p LogPolicy
		err = strictUnmarshal(raw.Entry, &p)
		d.Entry = p
	case KindClientBuild:
		var b ClientBuild
		err = strictUnmarshal(raw.Entry, &b)
		d.Entry = b
	default:
		return Decoded{}, fmt.Errorf("translog: decode: unknown kind %q", raw.Kind)
	}
	if err != nil {
		return Decoded{}, fmt.Errorf("translog: decode %s: %w", raw.Kind, err)
	}
	return d, nil
}
