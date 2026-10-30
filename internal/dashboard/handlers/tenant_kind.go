package handlers

import "net/http"

// The kind of a tenants row on the admin pages (RP82-7, Stage 1a Prompt 53
// Part 0d): a customer's own account (live — no parent, no sandbox_of), a
// platform's or an app's child (child — tenants.parent_tenant_id), or a
// test-mode sandbox (sandbox — tenants.sandbox_of). Every admin total and
// list counts live by default and takes ?kind=live|sandbox|child|all. The
// support search does not filter: a search by id finds any kind.

// tenantKind is one admin filter value.
type tenantKind string

const (
	tenantKindLive    tenantKind = "live"
	tenantKindSandbox tenantKind = "sandbox"
	tenantKindChild   tenantKind = "child"
	tenantKindAll     tenantKind = "all"
)

// parseTenantKind is the filter a request asks for; anything else is live.
func parseTenantKind(v string) tenantKind {
	switch k := tenantKind(v); k {
	case tenantKindSandbox, tenantKindChild, tenantKindAll:
		return k
	}
	return tenantKindLive
}

// tenantKindOf is the request's ?kind=.
func tenantKindOf(r *http.Request) tenantKind { return parseTenantKind(r.URL.Query().Get("kind")) }

// The kind predicate on tenants alias t, the kind a bound parameter (no
// SQL is built from a value): $1 in tenantKindWhere1, $2 in
// tenantKindWhere2.
const (
	tenantKindWhere1 = `($1 = 'all'
		OR ($1 = 'live' AND t.sandbox_of IS NULL AND t.parent_tenant_id IS NULL)
		OR ($1 = 'child' AND t.parent_tenant_id IS NOT NULL)
		OR ($1 = 'sandbox' AND t.sandbox_of IS NOT NULL))`
	tenantKindWhere2 = `($2 = 'all'
		OR ($2 = 'live' AND t.sandbox_of IS NULL AND t.parent_tenant_id IS NULL)
		OR ($2 = 'child' AND t.parent_tenant_id IS NOT NULL)
		OR ($2 = 'sandbox' AND t.sandbox_of IS NOT NULL))`
)
