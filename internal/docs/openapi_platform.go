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
					"Its bytes count in the platform's quota pool. `external_id` is the app's own id for it, unique per platform. Audited `platform.tenant_created`; emits `tenant.created`.",
				map[string]Response{
					"201": jsonResp("The child", ref("PlatformTenant")),
					"400": errResp("`invalid_json`, `label_required` (1-200 characters), `invalid_external_id` (at most 200)"),
					"403": errResp("`account_suspended`"),
					"404": notPlatform,
					"409": errResp("`external_id_taken` or `idempotency_key_reuse`"),
					"500": errResp("`internal_error`"),
				}),
				jsonBody("The child", objectPtr("", map[string]*Schema{
					"label":       str("A display name, 1-200 characters"),
					"external_id": str("The app's own id for the child (optional, unique per platform)"),
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
	}
}

// platformSchemas are the platform group's objects.
func platformSchemas() map[string]Schema {
	return map[string]Schema{
		"PlatformTenant": object("A child tenant of a platform (WP-PLAT-1)", map[string]*Schema{
			"object":                strEnum("", "tenant"),
			"id":                    str("The child's tenant id"),
			"label":                 str(""),
			"external_id":           str("The app's own id (absent when none)"),
			"residency":             str("Where its buckets default (`us`, `eu`, `uk`): a child's buckets default where its platform's do"),
			"created_at":            {Type: "string", Format: "date-time"},
			"deletion_scheduled_at": {Type: "string", Format: "date-time", Description: "Set once its erasure is scheduled"},
			"usage": objectPtr("", map[string]*Schema{
				"storage_used_bytes": integer("The bytes it stores, counted in the platform's pool"),
			}, "storage_used_bytes"),
			"request_id": str(""),
		}, "object", "id", "label", "residency", "created_at", "usage"),
	}
}
