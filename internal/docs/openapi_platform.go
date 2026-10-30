package docs

// generatePlatformPaths, internal/api/platform_tenants.go (WP-PLAT-1):
// requireJWT → the request log → the platform gate (404 for a tenant that
// is not a platform) → rate limiter → idempotency.
func generatePlatformPaths() map[string]*PathItem {
	mgmt := func(op *Operation) *Operation { return rateLimited(op) }
	mut := func(op *Operation) *Operation { return idempotent(rateLimited(op)) }
	notPlatform := errResp("`not_found`: the caller's tenant is not a platform (the routes do not exist for it)")
	id := pathParam("id", "The child tenant's id")

	return map[string]*PathItem{
		"/api/v1/platform/tenants": {
			Post: mut(withBody(jsonOp("Platform", "Create a child tenant", "PlatformCreateTenant",
				"Makes a CHILD tenant of the caller's platform: no user, no e-mail, no primary key pair (its keys are scoped keys the platform mints). "+
					"Its bytes count in the platform's quota pool. `external_id` is the app's own id for it, unique per platform. "+
					"`kind` is its edge: `managed` (the default) or `app` — an app may have managed children of its own, made here with `parent_id` = the app (depth 2, the deepest). "+
					"`storage_cap_bytes` / `egress_cap_bytes` cap it inside the pool (0 = the pool decides). Audited `platform.tenant_created`; emits `tenant.created`.",
				map[string]Response{
					"201": jsonResp("The child", ref("PlatformTenant")),
					"400": errResp("`invalid_json`, `label_required` (1-200 characters), `invalid_external_id` (at most 200), `invalid_kind`, `invalid_cap`, `child_of_child` (a parent that is not an app, or an app under an app)"),
					"403": errResp("`account_suspended`"),
					"404": notPlatform,
					"409": errResp("`external_id_taken` or `idempotency_key_reuse`"),
					"500": errResp("`internal_error`"),
				}),
				jsonBody("The child", objectPtr("", map[string]*Schema{
					"label":             str("A display name, 1-200 characters"),
					"external_id":       str("The app's own id for the child (optional, unique per platform)"),
					"kind":              strEnum("The edge (default `managed`)", "managed", "app"),
					"parent_id":         str("One of the platform's `app` children, to make this one under it (optional)"),
					"storage_cap_bytes": integer("A cap inside the pool, bytes (0 = the pool decides)"),
					"egress_cap_bytes":  integer("A monthly download cap inside the pool, bytes (0 = the pool decides)"),
				}, "label")))),
			Get: mgmt(withParams(jsonOp("Platform", "List the child tenants", "PlatformListTenants",
				"The caller's children by id, with each one's usage (the bytes it counts in the pool).",
				map[string]Response{
					"200": jsonResp("Children", listSchema("", ref("PlatformTenant"))),
					"404": notPlatform,
					"500": errResp("`internal_error`"),
				}),
				queryParam("limit", "1-100 (default 20)", &Schema{Type: "integer"}),
				queryParam("starting_after", "The last id of the previous page", &Schema{Type: "string"}))),
		},
		"/api/v1/platform/tenants/{id}": {
			Parameters: []Parameter{id},
			Get: mgmt(jsonOp("Platform", "Read a child tenant", "PlatformGetTenant",
				"One child of the caller's platform, with its usage.",
				map[string]Response{
					"200": jsonResp("The child", ref("PlatformTenant")),
					"404": errResp("`tenant_not_found` (not a child of this platform) or `not_found` (not a platform)"),
					"500": errResp("`internal_error`"),
				})),
			Patch: mut(withBody(jsonOp("Platform", "Set a child's caps", "PlatformPatchTenant",
				"A child's caps inside the pool: what it may store (min(cap, pool)) and download this UTC month (past its cap it is rate-limited while the pool has room). 0 = the pool decides. Audited `platform.tenant_caps_set`.",
				map[string]Response{
					"200": jsonResp("The child", ref("PlatformTenant")),
					"400": errResp("`invalid_json` or `invalid_cap`"),
					"404": errResp("`tenant_not_found` or `not_found`"),
					"500": errResp("`internal_error`"),
				}),
				jsonBody("The caps (either, both)", objectPtr("", map[string]*Schema{
					"storage_cap_bytes": integer("Bytes, 0 = the pool decides"),
					"egress_cap_bytes":  integer("Bytes a month, 0 = the pool decides"),
				})))),
			Delete: mut(jsonOp("Platform", "Schedule a child's erasure", "PlatformDeleteTenant",
				"Schedules the child's erasure after the ordinary 30-day grace: its objects, keys and rows go on the date, as any account's. "+
					"Scheduling twice keeps the first date. Audited `platform.tenant_deletion_scheduled`.",
				map[string]Response{
					"200": jsonResp("Scheduled", objectPtr("", map[string]*Schema{
						"object":                strEnum("", "tenant"),
						"id":                    str(""),
						"deletion_scheduled":    boolean("Always true"),
						"deletion_scheduled_at": {Type: "string", Format: "date-time"},
						"request_id":            str(""),
					}, "object", "id", "deletion_scheduled", "deletion_scheduled_at")),
					"404": errResp("`tenant_not_found` or `not_found`"),
					"500": errResp("`internal_error`"),
				})),
		},
		"/api/v1/platform/tenants/{id}/keys": {
			Parameters: []Parameter{id},
			Post: mut(withBody(jsonOp("Platform", "Mint a key for a child", "PlatformCreateKey",
				"A scoped key of the child (a child has no primary pair). Its scope never exceeds the calling credential's: what the request leaves unset is the caller's "+
					"(permissions, buckets, IP allowlist, expiry), and a wider request is 400 `scope_exceeds_parent`; a JWT is the account's own authority. "+
					"The secret is shown once. At most 50 live keys per child. Audited `platform.key_created`.",
				map[string]Response{
					"201": jsonResp("The key; `secret` only here", ref("PlatformKey")),
					"400": errResp("`invalid_json`, `invalid_permissions`, `invalid_ip_allowlist`, `unrestricted_ip_allowlist`, `invalid_expiry` or `scope_exceeds_parent`"),
					"404": errResp("`tenant_not_found` or `not_found`"),
					"409": errResp("`key_limit_exceeded` or `idempotency_key_reuse`"),
					"500": errResp("`internal_error`"),
				}),
				jsonBody("The key's scope", ref("APIKeyCreateRequest")))),
			Get: mgmt(jsonOp("Platform", "List a child's keys", "PlatformListKeys",
				"Every key of the child, revoked ones included, never a secret. Not paginated.",
				map[string]Response{
					"200": jsonResp("Keys", listSchema("", ref("PlatformKey"))),
					"404": errResp("`tenant_not_found` or `not_found`"),
					"500": errResp("`internal_error`"),
				})),
		},
		"/api/v1/platform/tenants/{id}/keys/{key}": {
			Parameters: []Parameter{id, pathParam("key", "The key's id")},
			Delete: mut(jsonOp("Platform", "Revoke a child's key", "PlatformDeleteKey",
				"Sets `revoked_at`; the key's STS tokens die with it. Audited `platform.key_revoked`.",
				map[string]Response{
					"200": jsonResp("Revoked", objectPtr("", map[string]*Schema{
						"object": strEnum("", "api_key"), "id": str(""), "deleted": boolean("Always true"), "request_id": str(""),
					}, "object", "id", "deleted")),
					"404": errResp("`key_not_found`, `tenant_not_found` or `not_found`"),
					"409": errResp("`key_revoked`"),
					"500": errResp("`internal_error`"),
				})),
		},
		"/api/v1/platform/tenants/{id}/sts": {
			Parameters: []Parameter{id},
			Post: mut(withBody(jsonOp("Platform", "Mint a temporary credential for a child", "PlatformCreateSTS",
				"An STS token of the child bounded by one of its keys (`parent_key_id`, required): the scope intersection and TTL of POST /api/v1/sts/token. Audited `platform.sts_created`.",
				map[string]Response{
					"201": jsonResp("The token", objectPtr("", map[string]*Schema{
						"object": strEnum("", "sts_token"), "access_key": str("ASIA…"), "secret_key": str(""),
						"tenant_id": str("The child"), "expiration": {Type: "string", Format: "date-time"}, "request_id": str(""),
					}, "object", "access_key", "secret_key", "expiration")),
					"400": errResp("`invalid_json`, `parent_key_required`, `invalid_permissions`, `parent_key_revoked` or `scope_error`"),
					"401": errResp("`parent_key_expired`"),
					"404": errResp("`parent_key_not_found`, `tenant_not_found` or `not_found`"),
					"500": errResp("`internal_error`"),
				}),
				jsonBody("The token's scope", objectPtr("", map[string]*Schema{
					"parent_key_id": str("The id of one of the child's keys"),
					"permissions":   {Type: "array", Items: str("")},
					"bucket_scope":  {Type: "array", Items: str("")},
					"ip_restrict":   {Type: "array", Items: str("")},
					"ttl":           integer("Seconds, 1-43200 (default 3600)"),
				}, "parent_key_id")))),
		},
	}
}

// platformSchemas are the platform group's objects.
func platformSchemas() map[string]Schema {
	return map[string]Schema{
		"PlatformTenant": object("A child tenant of a platform (WP-PLAT-1)", map[string]*Schema{
			"object":      strEnum("", "tenant"),
			"id":          str("The child's tenant id"),
			"label":       str(""),
			"external_id": str("The app's own id (absent when none)"),
			"residency":   str("Where its buckets default (`us`, `eu`, `uk`): a child's buckets default where its platform's do"),
			"kind":        strEnum("Its edge", "managed", "app"),
			"parent_id":   str("Its parent: the platform, or one of its apps"),
			"caps": objectPtr("Its caps inside the pool (0 = the pool decides)", map[string]*Schema{
				"storage_cap_bytes": integer(""),
				"egress_cap_bytes":  integer(""),
			}, "storage_cap_bytes", "egress_cap_bytes"),
			"created_at":            {Type: "string", Format: "date-time"},
			"deletion_scheduled_at": {Type: "string", Format: "date-time", Description: "Set once its erasure is scheduled"},
			"usage": objectPtr("", map[string]*Schema{
				"storage_used_bytes": integer("The bytes it stores, counted in the platform's pool"),
			}, "storage_used_bytes"),
			"request_id": str(""),
		}, "object", "id", "label", "residency", "created_at", "usage"),
		"PlatformKey": object("A child's scoped key (WP-PLAT-1)", map[string]*Schema{
			"object":       strEnum("", "api_key"),
			"id":           str(""),
			"name":         str(""),
			"key":          str("The access key id"),
			"secret":       str("Only in the create's answer"),
			"tenant_id":    str("The child"),
			"is_primary":   boolean("Always false: a child has no primary pair"),
			"permissions":  {Type: "array", Items: str("")},
			"bucket_scope": {Type: "array", Items: str("")},
			"ip_allowlist": {Type: "array", Items: str("")},
			"expires_at":   {Type: "string", Format: "date-time"},
			"revoked_at":   {Type: "string", Format: "date-time"},
			"created_at":   {Type: "string", Format: "date-time"},
		}, "object", "id", "key", "tenant_id", "permissions"),
	}
}
