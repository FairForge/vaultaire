package adminread

import "github.com/FairForge/vaultaire/internal/translog"

// The customer's view of its own entries says each one in words (Stage 1a
// Prompt 34b, "Access to your account"): ONE table, fixed sentences, no
// free text. A scope or a flag the table does not know is said as Unknown,
// never printed as it is.

// scopeWords is what an operator read, by admin_read scope. Only the
// subject scopes reach a tenant's view (Find skips the lists).
var scopeWords = map[string]string{
	translog.ScopeTenantDetail:   "Your account's record: profile, usage, keys and buckets",
	translog.ScopeTenantPayments: "Your plan and payments",
	translog.ScopeTenantAudit:    "Your account's audit trail",
	translog.ScopeTenantAuditCSV: "Your account's audit trail, exported as a file",
	translog.ScopeSupportCase:    "Your support view: buckets, keys and recent errors",
	translog.ScopeAbuseCase:      "An abuse report that names your account",
}

// policyWords says a write of the account's own row of a translog_* flag
// (log_policy, scope tenant), by flag, then action, then the value set.
var policyWords = map[string]map[string]string{
	translog.FlagTranslogAdminReads: {
		"set:true":  "Recording of operator reads turned on for your account",
		"set:false": "Recording of operator reads turned off for your account (it stays on while it is on for everyone)",
		"clear":     "Your account's own setting for recording operator reads removed: it follows the service-wide setting",
	},
	translog.FlagTranslogCustomerView: {
		"set:true":  "This page turned on for your account",
		"set:false": "This page turned off for your account (it stays on while it is on for everyone)",
		"clear":     "Your account's own setting for this page removed: it follows the service-wide setting",
	},
}

// Unknown is what the view says for an entry the table has no words for.
const Unknown = "An access the service cannot describe yet"

// Words is the sentence for one of the tenant's entries.
func Words(f Found) string {
	switch f.Kind {
	case translog.KindAdminRead:
		if w, ok := scopeWords[f.Scope]; ok {
			return w
		}
	case translog.KindLogPolicy:
		key := f.Action
		if f.Action == translog.PolicyActionSet && f.Enabled != nil {
			key = "set:false"
			if *f.Enabled {
				key = "set:true"
			}
		}
		if w, ok := policyWords[f.Flag][key]; ok {
			return w
		}
	}
	return Unknown
}
