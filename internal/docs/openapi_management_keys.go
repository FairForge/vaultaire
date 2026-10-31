package docs

// generateManagementKeyPaths, internal/api/management_keys.go (WP-KEY-1):
// the management key — an opaque bearer key of the ACCOUNT (sk_live_… /
// sk_test_…) a server presents instead of a person's JWT on
// /api/v1/manage and /api/v1/platform.
func generateManagementKeyPaths() map[string]*PathItem {
	mgmt := func(op *Operation) *Operation { return rateLimited(op) }
	mut := func(op *Operation) *Operation { return idempotent(rateLimited(op)) }
	personOnly := errResp("`person_required` (a management key never mints management keys), `role_forbidden` (an admin's or the owner's)")
	id := pathParam("id", "The management key's id")
	app := pathParam("id", "The app child's tenant id")

	return map[string]*PathItem{
		"/api/v1/manage/management-keys": {
			Post: mut(withBody(jsonOp("Management", "Mint a management key", "ManageCreateManagementKey",
				"A bearer key of the account for a server: `Authorization: Bearer sk_live_…` (`sk_test_…` for a sandbox) on /api/v1/manage and /api/v1/platform. "+
					"Its scopes are its authority: `manage:write` acts as a developer, `manage:read` as a viewer, `platform` as an admin on the platform routes (a platform only). "+
					"It never manages members, exports or erases the account, or mints management keys (403 `person_required`). "+
					"`s3_ceiling` bounds the S3 keys it mints (never its own buckets). The secret is shown once and stored as its sha256. "+
					"At most 25 live per account. Audited `management_key.created`, the audit trail naming it `mk:<id>`.",
				map[string]Response{
					"201": jsonResp("The key; `secret` only here", ref("ManagementKey")),
					"400": errResp("`invalid_json`, `invalid_scope` or `invalid_ceiling`"),
					"403": personOnly,
					"409": errResp("`key_limit_exceeded` or `idempotency_key_reuse`"),
					"500": errResp("`internal_error`"),
				}),
				jsonBody("The key", ref("ManagementKeyCreateRequest")))),
			Get: mgmt(jsonOp("Management", "List the management keys", "ManageListManagementKeys",
				"Every management key of the account, revoked ones included, never a secret or its hash. Not paginated.",
				map[string]Response{
					"200": jsonResp("Keys", listSchema("", ref("ManagementKey"))),
					"500": errResp("`internal_error`"),
				})),
		},
		"/api/v1/manage/management-keys/{id}": {
			Parameters: []Parameter{id},
			Delete: mut(jsonOp("Management", "Revoke a management key", "ManageRevokeManagementKey",
				"Sets `revoked_at`: the key is refused at its very next request (no cache). Audited `management_key.revoked`.",
				map[string]Response{
					"200": jsonResp("Revoked", objectPtr("", map[string]*Schema{
						"object": strEnum("", "management_key"), "id": str(""), "revoked": boolean("Always true"), "request_id": str(""),
					}, "object", "id", "revoked")),
					"403": personOnly,
					"404": errResp("`management_key_not_found`"),
					"500": errResp("`internal_error`"),
				})),
		},
		"/api/v1/platform/tenants/{id}/management-keys": {
			Parameters: []Parameter{app},
			Post: mut(withBody(jsonOp("Platform", "Mint an app's management key", "PlatformCreateManagementKey",
				"A management key of an APP child (kind `app`) for the app's backend: scope `apps` acts on /api/v1/platform AS the app, over its own managed children only "+
					"(a child it creates is under the app; depth 2 stays the deepest); `manage:read` / `manage:write` act on the app's own account. Never `platform`. "+
					"Its ceiling never exceeds the calling credential's. The secret is shown once.",
				map[string]Response{
					"201": jsonResp("The key; `secret` only here", ref("ManagementKey")),
					"400": errResp("`invalid_json`, `not_an_app`, `invalid_scope`, `invalid_ceiling` or `scope_exceeds_parent`"),
					"404": errResp("`tenant_not_found` or `not_found`"),
					"409": errResp("`key_limit_exceeded` or `idempotency_key_reuse`"),
					"500": errResp("`internal_error`"),
				}),
				jsonBody("The key", ref("ManagementKeyCreateRequest")))),
			Get: mgmt(jsonOp("Platform", "List an app's management keys", "PlatformListManagementKeys",
				"Every management key of the app, never a secret.",
				map[string]Response{
					"200": jsonResp("Keys", listSchema("", ref("ManagementKey"))),
					"400": errResp("`not_an_app`"),
					"404": errResp("`tenant_not_found` or `not_found`"),
					"500": errResp("`internal_error`"),
				})),
		},
		"/api/v1/platform/tenants/{id}/management-keys/{key}": {
			Parameters: []Parameter{app, pathParam("key", "The management key's id")},
			Delete: mut(jsonOp("Platform", "Revoke an app's management key", "PlatformRevokeManagementKey",
				"Sets `revoked_at`; refused at its next request.",
				map[string]Response{
					"200": jsonResp("Revoked", objectPtr("", map[string]*Schema{
						"object": strEnum("", "management_key"), "id": str(""), "revoked": boolean("Always true"), "request_id": str(""),
					}, "object", "id", "revoked")),
					"400": errResp("`not_an_app`"),
					"404": errResp("`management_key_not_found`, `tenant_not_found` or `not_found`"),
					"500": errResp("`internal_error`"),
				})),
		},
	}
}

func managementKeySchemas() map[string]Schema {
	ceiling := objectPtr("The most the key may mint (RP82-6: its own field, never a bucket scope of the key's); every member optional", map[string]*Schema{
		"permissions":        {Type: "array", Items: str("")},
		"bucket_scope":       {Type: "array", Items: str("")},
		"ip_allowlist":       {Type: "array", Items: str("")},
		"max_expiry_seconds": integer("The longest expiry of what it mints"),
	})
	return map[string]Schema{
		"ManagementKey": object("A management key (WP-KEY-1)", map[string]*Schema{
			"object":       strEnum("", "management_key"),
			"id":           str(""),
			"tenant_id":    str("The account it belongs to"),
			"name":         str(""),
			"prefix":       strEnum("Live or test", "sk_live_", "sk_test_"),
			"hint":         str("The first four characters after the prefix"),
			"scopes":       {Type: "array", Items: strEnum("", "manage:read", "manage:write", "platform", "apps")},
			"s3_ceiling":   ceiling,
			"created_by":   str("The person who minted it (absent when a key minted it, or its maker's account was erased)"),
			"created_at":   {Type: "string", Format: "date-time"},
			"last_used_at": {Type: "string", Format: "date-time", Description: "Recorded at most once a minute"},
			"revoked_at":   {Type: "string", Format: "date-time"},
			"secret":       str("Only in the mint's answer"),
		}, "object", "id", "prefix", "scopes", "created_at"),
		"ManagementKeyCreateRequest": object("A management key to mint", map[string]*Schema{
			"name":       str("A display name, at most 200 characters"),
			"scopes":     {Type: "array", Items: strEnum("", "manage:read", "manage:write", "platform", "apps")},
			"s3_ceiling": ceiling,
		}, "scopes"),
	}
}
