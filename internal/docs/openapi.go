// internal/docs/openapi.go
package docs

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// OpenAPISpec represents an OpenAPI 3.0 specification
type OpenAPISpec struct {
	OpenAPI    string                `json:"openapi"`
	Info       Info                  `json:"info"`
	Servers    []Server              `json:"servers,omitempty"`
	Paths      map[string]*PathItem  `json:"paths"`
	Components Components            `json:"components"`
	Security   []SecurityRequirement `json:"security,omitempty"`
	Tags       []Tag                 `json:"tags,omitempty"`
}

// Info contains API metadata
type Info struct {
	Title       string  `json:"title"`
	Description string  `json:"description,omitempty"`
	Version     string  `json:"version"`
	Contact     Contact `json:"contact,omitempty"`
	License     License `json:"license,omitempty"`
}

// Contact information
type Contact struct {
	Name  string `json:"name,omitempty"`
	URL   string `json:"url,omitempty"`
	Email string `json:"email,omitempty"`
}

// License information
type License struct {
	Name string `json:"name"`
	URL  string `json:"url,omitempty"`
}

// Server represents an API server
type Server struct {
	URL         string                    `json:"url"`
	Description string                    `json:"description,omitempty"`
	Variables   map[string]ServerVariable `json:"variables,omitempty"`
}

// ServerVariable for templating
type ServerVariable struct {
	Default     string   `json:"default"`
	Description string   `json:"description,omitempty"`
	Enum        []string `json:"enum,omitempty"`
}

// PathItem represents operations on a path
type PathItem struct {
	Get        *Operation  `json:"get,omitempty"`
	Patch      *Operation  `json:"patch,omitempty"`
	Put        *Operation  `json:"put,omitempty"`
	Post       *Operation  `json:"post,omitempty"`
	Delete     *Operation  `json:"delete,omitempty"`
	Head       *Operation  `json:"head,omitempty"`
	Options    *Operation  `json:"options,omitempty"`
	Parameters []Parameter `json:"parameters,omitempty"`
}

// Operation represents an API operation
type Operation struct {
	Tags        []string              `json:"tags,omitempty"`
	Summary     string                `json:"summary,omitempty"`
	Description string                `json:"description,omitempty"`
	OperationID string                `json:"operationId,omitempty"`
	Parameters  []Parameter           `json:"parameters,omitempty"`
	RequestBody *RequestBody          `json:"requestBody,omitempty"`
	Responses   map[string]Response   `json:"responses"`
	Security    []SecurityRequirement `json:"security,omitempty"`
}

// Parameter for operations. A Ref points at #/components/parameters and
// leaves every other field empty.
type Parameter struct {
	Ref         string      `json:"$ref,omitempty"`
	Name        string      `json:"name,omitempty"`
	In          string      `json:"in,omitempty"`
	Description string      `json:"description,omitempty"`
	Required    bool        `json:"required,omitempty"`
	Schema      *Schema     `json:"schema,omitempty"`
	Example     interface{} `json:"example,omitempty"`
}

// RequestBody for operations
type RequestBody struct {
	Description string               `json:"description,omitempty"`
	Content     map[string]MediaType `json:"content"`
	Required    bool                 `json:"required,omitempty"`
}

// Response from an operation. A Ref points at #/components/responses.
type Response struct {
	Ref         string               `json:"$ref,omitempty"`
	Description string               `json:"description,omitempty"`
	Headers     map[string]Header    `json:"headers,omitempty"`
	Content     map[string]MediaType `json:"content,omitempty"`
}

// Header definition. A Ref points at #/components/headers.
type Header struct {
	Ref         string  `json:"$ref,omitempty"`
	Description string  `json:"description,omitempty"`
	Schema      *Schema `json:"schema,omitempty"`
}

// MediaType with schema
type MediaType struct {
	Schema   *Schema            `json:"schema,omitempty"`
	Example  interface{}        `json:"example,omitempty"`
	Examples map[string]Example `json:"examples,omitempty"`
}

// Example for documentation
type Example struct {
	Summary     string      `json:"summary,omitempty"`
	Description string      `json:"description,omitempty"`
	Value       interface{} `json:"value,omitempty"`
}

// Components container
type Components struct {
	Schemas         map[string]Schema         `json:"schemas,omitempty"`
	SecuritySchemes map[string]SecurityScheme `json:"securitySchemes,omitempty"`
	Parameters      map[string]Parameter      `json:"parameters,omitempty"`
	RequestBodies   map[string]RequestBody    `json:"requestBodies,omitempty"`
	Responses       map[string]Response       `json:"responses,omitempty"`
	Headers         map[string]Header         `json:"headers,omitempty"`
}

// Schema definition
type Schema struct {
	Type                 string             `json:"type,omitempty"`
	Format               string             `json:"format,omitempty"`
	Description          string             `json:"description,omitempty"`
	Properties           map[string]*Schema `json:"properties,omitempty"`
	AdditionalProperties *Schema            `json:"additionalProperties,omitempty"`
	Required             []string           `json:"required,omitempty"`
	Items                *Schema            `json:"items,omitempty"`
	Example              interface{}        `json:"example,omitempty"`
	Ref                  string             `json:"$ref,omitempty"`
	Enum                 []interface{}      `json:"enum,omitempty"`
	Nullable             bool               `json:"nullable,omitempty"`
	Minimum              *int               `json:"minimum,omitempty"`
	Maximum              *int               `json:"maximum,omitempty"`
	Default              interface{}        `json:"default,omitempty"`
	XML                  *XMLObject         `json:"xml,omitempty"`
}

// XMLObject for XML marshaling hints
type XMLObject struct {
	Name      string `json:"name,omitempty"`
	Namespace string `json:"namespace,omitempty"`
	Wrapped   bool   `json:"wrapped,omitempty"`
}

// SecurityScheme definition
type SecurityScheme struct {
	Type         string `json:"type"`
	Description  string `json:"description,omitempty"`
	Name         string `json:"name,omitempty"`
	In           string `json:"in,omitempty"`
	Scheme       string `json:"scheme,omitempty"`
	BearerFormat string `json:"bearerFormat,omitempty"`
}

// SecurityRequirement mapping
type SecurityRequirement map[string][]string

// Tag for grouping operations
type Tag struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// GenerateOpenAPISpec creates the complete OpenAPI specification
func GenerateOpenAPISpec() *OpenAPISpec {
	return &OpenAPISpec{
		OpenAPI: "3.0.3",
		Info: Info{
			Title: "Stored Storage API",
			Description: "Two APIs on one host.\n\n" +
				"**S3 API** — the root paths (`/`, `/{bucket}`, `/{bucket}/{key}`) speak the S3 wire protocol: " +
				"AWS Signature Version 4, region `us-east-1`, path-style addressing, XML bodies. Use them with any S3 client " +
				"and the access key / secret from registration or a scoped API key.\n\n" +
				"**JSON API** — everything under `/api/v1` is JSON and authenticated with `Authorization: Bearer <JWT>`; " +
				"the JWT comes from `POST /auth/login` (24 h). Operations tagged Admin additionally require the JWT's user to have role `admin`. " +
				"Operations with no security listed (registration, login, password reset, waitlist) are public.\n\n" +
				"JSON API conventions: a single resource is returned as an object with an `object` type field and a `request_id`; " +
				"a collection is a `{\"object\":\"list\",\"data\":[...],\"has_more\":bool,\"next_cursor\":...}` envelope; " +
				"errors are `{\"error\":{\"type\",\"code\",\"message\",\"param\",\"request_id\"}}`. The Management, Webhooks and Events groups " +
				"are rate limited per tenant (100 requests/min, burst 10; `X-RateLimit-*` headers on every response, 429 when exhausted) " +
				"and honour an `Idempotency-Key` header on mutations (the first response is replayed for 24 h).",
			Version: "1.1.0",
			Contact: Contact{
				Name:  "Stored Support",
				Email: "support@stored.ge",
			},
			License: License{
				Name: "MIT",
				URL:  "https://opensource.org/licenses/MIT",
			},
		},
		Servers: []Server{
			{
				URL:         "https://stored.ge",
				Description: "Production",
			},
			{
				URL:         "http://localhost:8000",
				Description: "Local development",
			},
		},
		Tags: []Tag{
			{Name: "Buckets", Description: "S3 bucket operations (SigV4)"},
			{Name: "Objects", Description: "S3 object operations (SigV4)"},
			{Name: "Auth", Description: "Registration, login and password reset. Public; per-IP rate limited (login and reset 5/min, register 10/min)"},
			{Name: "Waitlist", Description: "Pre-launch waitlist signup from the landing page. Public; 10 signups per IP per hour"},
			{Name: "Site", Description: "Cookieless statistics beacon for the public site: named clicks, counted as daily totals with no identifier. Public; 240 pings per IP per hour"},
			{Name: "User", Description: "The caller's own account: profile, API keys, quota, usage, presigned URLs. Bearer JWT; no rate limiter on this group"},
			{Name: "Management", Description: "Buckets, keys, usage and GDPR account operations under /api/v1/manage. Bearer JWT, rate limited per tenant, Idempotency-Key honoured on mutations"},
			{Name: "Webhooks", Description: "Tenant webhook endpoints and their deliveries. Bearer JWT, rate limited per tenant, Idempotency-Key honoured on mutations"},
			{Name: "Events", Description: "The tenant's event log (what webhooks are fed from). Bearer JWT, rate limited per tenant"},
			{Name: "STS", Description: "Temporary S3 credentials bounded by the caller's authority. Bearer JWT"},
			{Name: "Admin", Description: "Operator endpoints under /api/v1/admin: Bearer JWT whose user has role admin (403 otherwise)"},
			{Name: "Health", Description: "Health checks"},
		},
		Paths: generatePaths(),
		Components: Components{
			Schemas:         generateSchemas(),
			SecuritySchemes: generateSecuritySchemes(),
			Parameters:      generateParameters(),
			Responses:       generateResponses(),
			Headers:         generateHeaders(),
		},
	}
}

// --- builders -------------------------------------------------------------
//
// The JSON API is built from a few small constructors so that each path
// reads as its contract (body, responses, auth) and not as nested braces.

const (
	schemaRef    = "#/components/schemas/"
	responseRef  = "#/components/responses/"
	parameterRef = "#/components/parameters/"
	headerRef    = "#/components/headers/"
)

func ref(name string) *Schema { return &Schema{Ref: schemaRef + name} }

func bearer() []SecurityRequirement { return []SecurityRequirement{{"BearerAuth": {}}} }

func sigv4() []SecurityRequirement { return []SecurityRequirement{{"S3Signature": {}}} }

func str(desc string) *Schema { return &Schema{Type: "string", Description: desc} }

func strEnum(desc string, values ...string) *Schema {
	s := str(desc)
	for _, v := range values {
		s.Enum = append(s.Enum, v)
	}
	return s
}

func integer(desc string) *Schema {
	return &Schema{Type: "integer", Format: "int64", Description: desc}
}

func intRange(desc string, minimum, maximum int) *Schema {
	s := &Schema{Type: "integer", Description: desc}
	s.Minimum, s.Maximum = &minimum, &maximum
	return s
}

func number(desc string) *Schema { return &Schema{Type: "number", Format: "double", Description: desc} }

func boolean(desc string) *Schema { return &Schema{Type: "boolean", Description: desc} }

func dateTime(desc string) *Schema {
	return &Schema{Type: "string", Format: "date-time", Description: desc}
}

func nullable(s *Schema) *Schema { s.Nullable = true; return s }

func strArray(desc string) *Schema {
	return &Schema{Type: "array", Description: desc, Items: &Schema{Type: "string"}}
}

func arrayOf(item *Schema) *Schema { return &Schema{Type: "array", Items: item} }

func freeObject(desc string) *Schema { return &Schema{Type: "object", Description: desc} }

func object(desc string, props map[string]*Schema, required ...string) Schema {
	return Schema{Type: "object", Description: desc, Properties: props, Required: required}
}

func objectPtr(desc string, props map[string]*Schema, required ...string) *Schema {
	s := object(desc, props, required...)
	return &s
}

// listSchema is the JSON API collection envelope written by
// writeListResponse (internal/api/management.go).
func listSchema(desc string, item *Schema) *Schema {
	return objectPtr(desc, map[string]*Schema{
		"object":      strEnum("Always `list`", "list"),
		"data":        arrayOf(item),
		"has_more":    boolean("True when another page follows"),
		"next_cursor": str("Pass as the next page's cursor (`starting_after` or `cursor`, per endpoint); absent on the last page"),
		"total_count": integer("Total rows matching, or the size of this page where the endpoint does not count (documented per endpoint)"),
		"request_id":  str("Request id echoed from X-Request-Id"),
	}, "object", "data", "has_more", "total_count", "request_id")
}

func jsonBody(desc string, schema *Schema) *RequestBody {
	return &RequestBody{
		Description: desc,
		Required:    true,
		Content:     map[string]MediaType{"application/json": {Schema: schema}},
	}
}

func jsonResp(desc string, schema *Schema) Response {
	if schema == nil {
		return Response{Description: desc}
	}
	return Response{Description: desc, Content: map[string]MediaType{"application/json": {Schema: schema}}}
}

// errResp is a JSON API error (JSONError envelope). desc names the codes
// the handler writes at that status.
func errResp(desc string) Response { return jsonResp(desc, ref("JSONError")) }

// textResp is an http.Error response: text/plain, the message as the body.
func textResp(desc string) Response {
	return Response{Description: desc, Content: map[string]MediaType{"text/plain": {Schema: &Schema{Type: "string"}}}}
}

func refResp(name string) Response { return Response{Ref: responseRef + name} }

func xmlErr(desc string) Response {
	return Response{Description: desc, Content: map[string]MediaType{"application/xml": {Schema: ref("Error")}}}
}

func pathParam(name, desc string) Parameter {
	return Parameter{Name: name, In: "path", Description: desc, Required: true, Schema: &Schema{Type: "string"}}
}

func queryParam(name, desc string, schema *Schema) Parameter {
	return Parameter{Name: name, In: "query", Description: desc, Schema: schema}
}

func refParam(name string) Parameter { return Parameter{Ref: parameterRef + name} }

// jsonOp is a Bearer-authenticated JSON operation. Every one answers 401
// the same way (requireJWT), so the shared response is attached here.
func jsonOp(tag, summary, opID, desc string, responses map[string]Response) *Operation {
	if _, ok := responses["401"]; !ok {
		responses["401"] = refResp("Unauthorized")
	}
	return &Operation{
		Tags:        []string{tag},
		Summary:     summary,
		OperationID: opID,
		Description: desc,
		Responses:   responses,
		Security:    bearer(),
	}
}

// publicOp is an unauthenticated operation (no security requirement).
func publicOp(tag, summary, opID, desc string, responses map[string]Response) *Operation {
	return &Operation{
		Tags:        []string{tag},
		Summary:     summary,
		OperationID: opID,
		Description: desc,
		Responses:   responses,
	}
}

func withParams(op *Operation, params ...Parameter) *Operation {
	op.Parameters = append(op.Parameters, params...)
	return op
}

func withBody(op *Operation, body *RequestBody) *Operation {
	op.RequestBody = body
	return op
}

// rateLimited marks an operation served behind ManagementRateLimiter: the
// X-RateLimit-* headers ride on every 2xx and 429 is possible.
func rateLimited(op *Operation) *Operation {
	for code, r := range op.Responses {
		if len(code) > 0 && code[0] == '2' && r.Ref == "" {
			if r.Headers == nil {
				r.Headers = map[string]Header{}
			}
			for _, h := range []string{"X-RateLimit-Limit", "X-RateLimit-Remaining", "X-RateLimit-Reset"} {
				r.Headers[h] = Header{Ref: headerRef + h}
			}
			op.Responses[code] = r
		}
	}
	op.Responses["429"] = refResp("RateLimited")
	return op
}

// idempotent marks a mutation behind the Idempotency-Key middleware.
func idempotent(op *Operation) *Operation {
	op.Parameters = append([]Parameter{refParam("IdempotencyKey")}, op.Parameters...)
	for code, r := range op.Responses {
		if len(code) > 0 && code[0] == '2' && r.Ref == "" {
			if r.Headers == nil {
				r.Headers = map[string]Header{}
			}
			r.Headers["Idempotency-Replayed"] = Header{Ref: headerRef + "Idempotency-Replayed"}
			op.Responses[code] = r
		}
	}
	if _, ok := op.Responses["409"]; !ok {
		op.Responses["409"] = errResp("`idempotency_key_reuse` — the Idempotency-Key was already used with a different method or path")
	}
	return op
}

// adminOnly marks an /api/v1/admin operation: requireAdmin looks the JWT's
// user up per request and answers 403 unless users.role = 'admin'.
func adminOnly(op *Operation) *Operation {
	op.Description = "Requires the Bearer JWT's user to have role `admin` (looked up per request; 403 otherwise). " + op.Description
	op.Responses["403"] = refResp("Forbidden")
	return op
}

// --- paths ------------------------------------------------------------------

func generatePaths() map[string]*PathItem {
	paths := map[string]*PathItem{}
	for _, group := range []map[string]*PathItem{
		generateS3Paths(),
		generateAuthPaths(),
		generateUserPaths(),
		generateManagementPaths(),
		generateWebhookPaths(),
		generateSTSPaths(),
		generateAdminPaths(),
	} {
		for p, item := range group {
			paths[p] = item
		}
	}
	return paths
}

func generateS3Paths() map[string]*PathItem {
	return map[string]*PathItem{
		"/": {
			Get: &Operation{
				Tags:        []string{"Buckets"},
				Summary:     "List all buckets",
				Description: "Returns a list of all buckets owned by the authenticated user",
				OperationID: "ListBuckets",
				Security:    sigv4(),
				Responses: map[string]Response{
					"200": {
						Description: "Successful response",
						Content: map[string]MediaType{
							"application/xml": {Schema: ref("ListBucketsResponse")},
						},
					},
					"403": xmlErr("Access denied"),
				},
			},
		},
		"/{bucket}": {
			Parameters: []Parameter{pathParam("bucket", "Bucket name")},
			Get: &Operation{
				Tags:        []string{"Objects"},
				Summary:     "List objects in bucket",
				Description: "Returns a list of objects in the specified bucket",
				OperationID: "ListObjects",
				Security:    sigv4(),
				Parameters: []Parameter{
					queryParam("prefix", "Limits results to objects beginning with prefix", &Schema{Type: "string"}),
					queryParam("delimiter", "Delimiter for grouping objects", &Schema{Type: "string"}),
					queryParam("max-keys", "Maximum number of objects to return", &Schema{Type: "integer", Format: "int32"}),
				},
				Responses: map[string]Response{
					"200": {
						Description: "Successful response",
						Content: map[string]MediaType{
							"application/xml": {Schema: ref("ListObjectsResponse")},
						},
					},
					"403": xmlErr("Access denied"),
					"404": xmlErr("Bucket not found"),
				},
			},
			Put: &Operation{
				Tags:        []string{"Buckets"},
				Summary:     "Create bucket",
				Description: "Creates a new bucket",
				OperationID: "CreateBucket",
				Security:    sigv4(),
				Responses: map[string]Response{
					"200": {Description: "Bucket created successfully"},
					"403": xmlErr("Access denied"),
					"409": xmlErr("Bucket already exists"),
				},
			},
			Delete: &Operation{
				Tags:        []string{"Buckets"},
				Summary:     "Delete bucket",
				Description: "Deletes an empty bucket",
				OperationID: "DeleteBucket",
				Security:    sigv4(),
				Responses: map[string]Response{
					"204": {Description: "Bucket deleted successfully"},
					"403": xmlErr("Access denied"),
					"404": xmlErr("Bucket not found"),
					"409": xmlErr("Bucket not empty"),
				},
			},
		},
		"/{bucket}/{key}": {
			Get: &Operation{
				Tags:        []string{"Objects"},
				Summary:     "Get object",
				Description: "Retrieves an object from a bucket",
				OperationID: "GetObject",
				Security:    sigv4(),
				Parameters: []Parameter{
					pathParam("bucket", "Bucket name"),
					pathParam("key", "Object key"),
				},
				Responses: map[string]Response{
					"200": {
						Description: "Object retrieved successfully",
						Headers: map[string]Header{
							"Content-Type":   {Schema: &Schema{Type: "string"}},
							"Content-Length": {Schema: &Schema{Type: "integer"}},
							"ETag":           {Schema: &Schema{Type: "string"}},
						},
						Content: map[string]MediaType{
							"application/octet-stream": {Schema: &Schema{Type: "string", Format: "binary"}},
						},
					},
					"403": xmlErr("Access denied"),
					"404": xmlErr("Object not found"),
				},
			},
			Put: &Operation{
				Tags:        []string{"Objects"},
				Summary:     "Upload object",
				Description: "Uploads an object to a bucket",
				OperationID: "PutObject",
				Security:    sigv4(),
				Parameters: []Parameter{
					pathParam("bucket", "Bucket name"),
					pathParam("key", "Object key"),
				},
				RequestBody: &RequestBody{
					Description: "Object data",
					Required:    true,
					Content: map[string]MediaType{
						"application/octet-stream": {Schema: &Schema{Type: "string", Format: "binary"}},
					},
				},
				Responses: map[string]Response{
					"200": {
						Description: "Object uploaded successfully",
						Headers:     map[string]Header{"ETag": {Schema: &Schema{Type: "string"}}},
					},
					"403": xmlErr("Access denied"),
				},
			},
			Delete: &Operation{
				Tags:        []string{"Objects"},
				Summary:     "Delete object",
				Description: "Deletes an object from a bucket",
				OperationID: "DeleteObject",
				Security:    sigv4(),
				Parameters: []Parameter{
					pathParam("bucket", "Bucket name"),
					pathParam("key", "Object key"),
				},
				Responses: map[string]Response{
					"204": {Description: "Object deleted successfully"},
					"403": xmlErr("Access denied"),
					"404": xmlErr("Object not found"),
				},
			},
		},
	}
}

// generateAuthPaths — internal/api/server.go handleRegister / handleLogin /
// handlePasswordReset / handlePasswordResetComplete, and waitlist.go.
// These handlers answer errors with http.Error (text/plain).
func generateAuthPaths() map[string]*PathItem {
	return map[string]*PathItem{
		"/auth/register": {
			Post: withBody(publicOp("Auth", "Register an account", "Register",
				"Creates the user, its tenant, the primary S3 key pair and the quota row in one transaction, and returns the S3 credentials. "+
					"Closed when the `signups` feature flag is off (403). Rate limited 10/min per IP.",
				map[string]Response{
					"200": jsonResp("Account created; the S3 credentials are shown once", ref("RegisterResponse")),
					"400": textResp("Malformed JSON, `invalid email address`, `user already exists`, or the password is shorter than the minimum"),
					"403": textResp("Signups are currently closed (the `signups` flag is off)"),
					"429": refResp("AuthRateLimited"),
				}),
				jsonBody("Registration", ref("RegisterRequest"))),
		},
		"/auth/login": {
			Post: withBody(publicOp("Auth", "Log in and mint a JWT", "Login",
				"Validates the password and returns the Bearer JWT (HS256, 24 h) used by every /api/v1 endpoint. Rate limited 5/min per IP.",
				map[string]Response{
					"200": jsonResp("Token issued", ref("LoginResponse")),
					"400": textResp("Malformed JSON"),
					"401": textResp("`Invalid credentials`"),
					"404": textResp("`User not found` (the password validated but the user row could not be loaded)"),
					"429": refResp("AuthRateLimited"),
					"500": textResp("`Failed to generate token`"),
				}),
				jsonBody("Credentials", ref("LoginRequest"))),
		},
		"/auth/password-reset": {
			Post: withBody(publicOp("Auth", "Request a password reset email", "RequestPasswordReset",
				"Always answers 200 with the same message whether or not the email is registered (no account enumeration). Rate limited 5/min per IP.",
				map[string]Response{
					"200": jsonResp("Accepted", objectPtr("", map[string]*Schema{
						"message": str("`If that email is registered, a reset link has been sent.`"),
					}, "message")),
					"400": textResp("Malformed JSON"),
					"429": refResp("AuthRateLimited"),
				}),
				jsonBody("Account email", objectPtr("", map[string]*Schema{
					"email": str("Account email address"),
				}, "email"))),
		},
		"/auth/password-reset/complete": {
			Post: withBody(publicOp("Auth", "Complete a password reset", "CompletePasswordReset",
				"Consumes the emailed token and sets the new password. Every dashboard session of the user is revoked. Rate limited 5/min per IP.",
				map[string]Response{
					"200": jsonResp("Password changed", objectPtr("", map[string]*Schema{
						"message": str("`Password reset successful`"),
					}, "message")),
					"400": textResp("Malformed JSON, or `Invalid or expired token` (also covers a rejected new password)"),
					"429": refResp("AuthRateLimited"),
				}),
				jsonBody("Token and new password", objectPtr("", map[string]*Schema{
					"token":        str("The token from the reset email"),
					"new_password": str("The new password"),
				}, "token", "new_password"))),
		},
		"/api/waitlist": {
			Post: withBody(publicOp("Waitlist", "Join the pre-launch waitlist", "WaitlistSignup",
				"Records an email (and, optionally, the house the visitor built on the landing page plus attribution). "+
					"Accepts a form post (`application/x-www-form-urlencoded`) or JSON. Re-signing up with the same email is a no-op success, "+
					"except that a newly built house replaces the old one. Junk in the optional fields is clamped or dropped, never rejected. "+
					"Rate limited 10 signups per IP per hour. Without a database (local dev) the signup is logged and 200 returned.",
				map[string]Response{
					"200": jsonResp("Recorded", objectPtr("", map[string]*Schema{
						"status": strEnum("", "ok"),
					}, "status")),
					"400": jsonResp("`invalid email` (unparseable or longer than 320 characters)", ref("SimpleError")),
					"429": jsonResp("`too many requests`", ref("SimpleError")),
					"500": jsonResp("`could not save`", ref("SimpleError")),
				}),
				&RequestBody{
					Description: "Signup. `email` is the only required field.",
					Required:    true,
					Content: map[string]MediaType{
						"application/json":                  {Schema: ref("WaitlistSignup")},
						"application/x-www-form-urlencoded": {Schema: ref("WaitlistSignup")},
					},
				}),
		},
		"/api/ping": {
			Post: withBody(publicOp("Site", "Count one named click on the public site", "SitePing",
				"The landing page's `navigator.sendBeacon` target. Counts the event into the day's cookieless site "+
					"statistics (page, referring host, campaign labels, country from Cloudflare, device class — totals only, no identifier). "+
					"Event names come from a closed list (`builder.edit`, `builder.attic`, `builder.share`, `builder.save`, `builder.pack`, "+
					"`build.view`, `pricing.view`, `calc.move`, `age.scrub`, `copy`, `waitlist.submit`, `cta.register`, `uc.*`, `res.*`); "+
					"anything else, and any non-browser user agent, is ignored with the same 204 so the page never sees an error.",
				map[string]Response{
					"204": {Description: "Counted, or ignored"},
					"400": jsonResp("`invalid body` (not JSON, or over 2 KB)", ref("SimpleError")),
					"429": jsonResp("`too many requests`", ref("SimpleError")),
				}),
				jsonBody("One event", ref("SitePing"))),
		},
	}
}

// generateUserPaths — internal/api/user_api.go, user_quota.go, usage.go,
// presigned.go. requireJWT only: no rate limiter, no idempotency, and the
// key handlers answer failures with http.Error (text/plain).
func generateUserPaths() map[string]*PathItem {
	keyID := pathParam("keyId", "API key id (the `id` from list/create, not the access key)")
	keyErr500 := textResp("The auth service failed, including `API key not found` when the id is not one of the caller's keys (Review R11-17: not mapped to 404 on this group)")

	return map[string]*PathItem{
		"/api/v1/user": {
			Get: jsonOp("User", "Get the calling account", "GetUser",
				"The account as persisted: profile columns from `users`, MFA state, and the tenant quota (used/limit/tier).",
				map[string]Response{
					"200": jsonResp("The account", ref("UserInfo")),
					"401": errResp("Bearer token missing/invalid (text/plain from the middleware), or `missing_credentials` when the token carries no user or tenant"),
					"404": errResp("`user_not_found` — the JWT's user row no longer exists"),
					"500": errResp("`db_error`"),
				}),
			Delete: withBody(jsonOp("User", "Schedule account deletion", "ScheduleAccountDeletionViaUser",
				"Same handler and contract as `DELETE /api/v1/manage/account`: marks the user `pending_deletion` with a 30-day grace period; "+
					"a second call returns the already-scheduled date. Cancel with `POST /api/v1/manage/account/cancel-deletion`. Not rate limited on this path.",
				map[string]Response{
					"200": jsonResp("Deletion scheduled", ref("AccountDeletionScheduled")),
					"400": errResp("`invalid_json`, or `missing_parameter` when `reason` is empty"),
					"401": errResp("Bearer token missing/invalid, or `missing_credentials`"),
					"500": errResp("`deletion_failed` (also when no database is configured)"),
				}),
				jsonBody("Why the account is being closed", ref("AccountDeletionRequest"))),
		},
		"/api/v1/user/apikeys": {
			Get: jsonOp("User", "List the caller's API keys", "ListUserAPIKeys",
				"Every key row of the user, including revoked ones (`revoked_at` set). `secret` is never returned after creation. Plain JSON array, not the list envelope.",
				map[string]Response{
					"200": jsonResp("Keys", arrayOf(ref("UserAPIKey"))),
					"500": textResp("The auth service failed"),
				}),
			Post: withBody(jsonOp("User", "Create an API key", "CreateUserAPIKey",
				"Creates a scoped key. `permissions` restricts the key to the named S3 operations (validated); `expiry_days` sets `expires_at`. "+
					"`BypassGovernanceRetention` is a privilege, not an operation: it lets the key have `x-amz-bypass-governance-retention` honoured. "+
					"The secret is returned once. Subject to the plan's key cap (409).",
				map[string]Response{
					"201": jsonResp("Key created; `secret` is shown only here", ref("UserAPIKeyCreated")),
					"400": textResp("`Invalid request` (malformed JSON) or `unknown permission: ...`"),
					"409": textResp("`API key limit reached for this plan; revoke a key or upgrade`"),
					"500": textResp("The auth service failed"),
				}),
				jsonBody("Key definition", ref("UserAPIKeyCreateRequest"))),
		},
		"/api/v1/user/apikeys/{keyId}/rotate": {
			Parameters: []Parameter{keyID},
			Post: jsonOp("User", "Rotate an API key", "RotateUserAPIKey",
				"Issues a new access key and secret for the key id; the old pair stops working. No request body.",
				map[string]Response{
					"200": jsonResp("New credentials; `secret` is shown only here", objectPtr("", map[string]*Schema{
						"id":     str("Key id (unchanged)"),
						"name":   str("Key name"),
						"key":    str("New access key id"),
						"secret": str("New secret access key"),
					}, "id", "name", "key", "secret")),
					"500": keyErr500,
				}),
		},
		"/api/v1/user/apikeys/{keyId}": {
			Parameters: []Parameter{keyID},
			Delete: jsonOp("User", "Revoke an API key", "RevokeUserAPIKey",
				"Sets `revoked_at`; S3 auth stops accepting the key immediately. The audit row is written by the auth service.",
				map[string]Response{
					"204": {Description: "Revoked"},
					"500": keyErr500,
				}),
		},
		"/api/v1/user/apikeys/{keyId}/expire": {
			Parameters: []Parameter{keyID},
			Post: withBody(jsonOp("User", "Set an API key's expiry", "SetUserAPIKeyExpiration",
				"Sets `expires_at` to now + `days`. The handler does not reject non-positive values (Review R11-22): `0` or a negative number expires the key at or before now.",
				map[string]Response{
					"200": jsonResp("Expiry set", objectPtr("", map[string]*Schema{
						"message":    str("`Expiration set successfully`"),
						"expires_at": dateTime("The new expiry (RFC 3339)"),
					}, "message", "expires_at")),
					"400": textResp("`Invalid request` (malformed JSON)"),
					"500": keyErr500,
				}),
				jsonBody("Days from now", objectPtr("", map[string]*Schema{
					"days": {Type: "integer", Description: "Days from now until the key expires"},
				}, "days"))),
		},
		"/api/v1/user/apikeys/audit": {
			Get: withParams(jsonOp("User", "List the caller's key audit events", "ListUserAPIKeyAudit",
				"The caller's own rows from `audit_logs` with `event_type = key` (create, rotate, revoke, expiry), newest first, keyset-paginated. "+
					"`total_count` is the size of the page, not the total.",
				map[string]Response{
					"200": jsonResp("Audit rows", listSchema("", ref("AuditRow"))),
					"400": errResp("`invalid_cursor` — the cursor is not one this endpoint issued"),
					"500": errResp("`db_error`"),
				}),
				queryParam("action", "Exact `action` to filter on, e.g. `key.revoked`", &Schema{Type: "string"}),
				refParam("Cursor"),
				queryParam("limit", "Page size; default 100, clamped to 100", intRange("", 1, 100))),
		},
		"/api/v1/user/quota": {
			Get: jsonOp("User", "Get the tenant quota", "GetUserQuota",
				"Storage used and limit for the JWT's tenant, the plan tier, and whether an upgrade is possible (`tier != enterprise`).",
				map[string]Response{
					"200": jsonResp("Quota", ref("QuotaInfo")),
					"401": textResp("`unauthorized` — Bearer token missing/invalid, or no tenant in the token"),
					"500": textResp("`failed to get quota`"),
				}),
		},
		"/api/v1/user/quota/history": {
			Get: jsonOp("User", "Get 30 days of usage history", "GetUserQuotaHistory",
				"One row per day of `quota_usage_events` in the last 30 days, newest first. Plain JSON array (may be `null` when there were no events).",
				map[string]Response{
					"200": jsonResp("Daily usage", nullable(arrayOf(ref("UsageHistoryEntry")))),
					"401": textResp("`unauthorized` — Bearer token missing/invalid, or no tenant in the token"),
					"500": textResp("`failed to get history`"),
				}),
		},
		"/api/v1/user/usage": {
			Get: jsonOp("User", "Get usage statistics", "GetUserUsageStats",
				"Storage used/limit and the percentage for the JWT's tenant.",
				map[string]Response{
					"200": jsonResp("Usage", ref("UsageStats")),
					"401": textResp("`unauthorized` — Bearer token missing/invalid, or no tenant in the token"),
					"500": textResp("`failed to get usage`"),
				}),
		},
		"/api/v1/user/usage/alerts": {
			Get: jsonOp("User", "Get usage alerts", "GetUserUsageAlerts",
				"At most one alert: `INFO` at 70 %, `WARNING` at 80 %, `CRITICAL` at 90 % of the storage limit; an empty array below 70 %.",
				map[string]Response{
					"200": jsonResp("Alerts", arrayOf(ref("UsageAlert"))),
					"401": textResp("`unauthorized` — Bearer token missing/invalid, or no tenant in the token"),
					"500": textResp("`failed to get usage`"),
				}),
		},
		"/api/v1/user/presigned": {
			Get: withParams(jsonOp("User", "Mint a presigned S3 URL", "GetPresignedURL",
				"Signs a GET or PUT URL for one object with the tenant's primary S3 credentials. Errors are JSON `{\"error\": ...}` written with a text/plain content type.",
				map[string]Response{
					"200": jsonResp("Presigned URL", objectPtr("", map[string]*Schema{
						"url":        str("The presigned URL"),
						"expires_at": dateTime("When the URL stops working (RFC 3339)"),
						"method":     strEnum("HTTP method the URL is signed for", "GET", "PUT"),
					}, "url", "expires_at", "method")),
					"400": jsonResp("`bucket and key are required`, `method must be GET or PUT`, or `expires must be between 1 and 604800 seconds`", ref("SimpleError")),
					"401": jsonResp("`unauthorized` — Bearer token missing/invalid, or no tenant in the token", ref("SimpleError")),
					"404": jsonResp("`tenant credentials not found`", ref("SimpleError")),
					"500": jsonResp("`database not available`", ref("SimpleError")),
				}),
				Parameter{Name: "bucket", In: "query", Required: true, Description: "Bucket name", Schema: &Schema{Type: "string"}},
				Parameter{Name: "key", In: "query", Required: true, Description: "Object key", Schema: &Schema{Type: "string"}},
				queryParam("method", "Method to sign for; default GET", strEnum("", "GET", "PUT")),
				queryParam("expires", "Lifetime in seconds; default 3600, maximum 604800 (7 days)", intRange("", 1, 604800))),
		},
	}
}

// generateManagementPaths — internal/api/management_routes.go. Chain:
// requireJWT → ManagementRateLimiter → idempotency middleware.
func generateManagementPaths() map[string]*PathItem {
	name := pathParam("name", "Bucket name")
	mgmt := func(op *Operation) *Operation { return rateLimited(op) }
	mut := func(op *Operation) *Operation { return idempotent(rateLimited(op)) }

	return map[string]*PathItem{
		"/api/v1/manage/buckets": {
			Get: mgmt(withParams(jsonOp("Management", "List buckets", "ManageListBuckets",
				"The tenant's buckets ordered by name. `total_count` is the tenant's bucket count.",
				map[string]Response{
					"200": jsonResp("Buckets", listSchema("", ref("ManagedBucket"))),
					"500": errResp("`db_error`"),
				}),
				refParam("Limit"),
				queryParam("starting_after", "Return buckets whose name sorts after this one (the previous page's `next_cursor`)", &Schema{Type: "string"}))),
			Post: mut(withBody(jsonOp("Management", "Create a bucket", "ManageCreateBucket",
				"Registers a bucket for the tenant. `region` picks the iDrive region the bucket is homed in; omitted = the deployment default. "+
					"A region is selectable only when its driver is enabled on this deployment (`region_unavailable` otherwise). "+
					"Free-tier tenants are capped at one bucket (403); every tenant at 1000 (409).",
				map[string]Response{
					"201": jsonResp("Bucket created", ref("ManagedBucket")),
					"400": errResp("`invalid_json`, `missing_parameter` (name), `invalid_bucket_name` (3–63 chars, lowercase alphanumeric and hyphens), `invalid_region` (not a known region id), `region_unavailable` (known region with no driver here)"),
					"403": errResp("`free_tier_bucket_limit` — the free tier allows one bucket; upgrade at /dashboard/billing"),
					"409": errResp("`bucket_exists` (the tenant already owns it), `bucket_limit_exceeded` (1000 per account), or `idempotency_key_reuse`"),
					"500": errResp("`internal_error`"),
				}),
				jsonBody("Bucket definition", ref("ManagedBucketCreateRequest")))),
		},
		"/api/v1/manage/buckets/{name}": {
			Parameters: []Parameter{name},
			Get: mgmt(jsonOp("Management", "Get a bucket", "ManageGetBucket",
				"The bucket row including its metadata map.",
				map[string]Response{
					"200": jsonResp("Bucket", ref("ManagedBucket")),
					"404": errResp("`bucket_not_found`"),
					"500": errResp("`no_database`"),
				})),
			Patch: mut(withBody(jsonOp("Management", "Merge bucket metadata", "ManagePatchBucket",
				"Shallow-merges `metadata` into the stored map (a key set to null is removed). The whole map must be present in the body.",
				map[string]Response{
					"200": jsonResp("Bucket with the merged metadata", ref("ManagedBucket")),
					"400": errResp("`invalid_json`, `missing_parameter` (metadata), or `invalid_metadata`"),
					"404": errResp("`bucket_not_found`"),
					"500": errResp("`db_error` or `no_database`"),
				}),
				jsonBody("Metadata to merge", objectPtr("", map[string]*Schema{
					"metadata": {Type: "object", Description: "Keys to set; string values", AdditionalProperties: &Schema{Type: "string", Nullable: true}},
				}, "metadata")))),
			Delete: mut(jsonOp("Management", "Delete a bucket", "ManageDeleteBucket",
				"Registry-driven like S3 DeleteBucket: the bucket must be empty (no objects, versions or in-progress multipart uploads). Emits `bucket.deleted`.",
				map[string]Response{
					"200": jsonResp("Deleted", ref("BucketDeleted")),
					"404": errResp("`bucket_not_found` (also for a name that cannot be a bucket)"),
					"409": errResp("`bucket_not_empty` — the message lists what is still in it; or `idempotency_key_reuse`"),
					"500": errResp("`internal_error`"),
				})),
		},
		"/api/v1/manage/buckets/{name}/objects": {
			Parameters: []Parameter{name},
			Get: mgmt(withParams(jsonOp("Management", "List objects in a bucket", "ManageListObjects",
				"Objects from the HEAD cache ordered by key. An unknown bucket yields an empty list, not 404. `total_count` is the size of the page.",
				map[string]Response{
					"200": jsonResp("Objects", listSchema("", ref("ManagedObject"))),
					"500": errResp("`db_error`"),
				}),
				refParam("Limit"),
				queryParam("prefix", "Only keys beginning with this prefix", &Schema{Type: "string"}),
				queryParam("starting_after", "Return keys that sort after this one (the previous page's `next_cursor`)", &Schema{Type: "string"}))),
		},
		"/api/v1/manage/buckets/{name}/tier": {
			Parameters: []Parameter{name},
			Put: mut(withBody(jsonOp("Management", "Set the bucket's tier preference", "ManageSetBucketTier",
				"Stores `tier_preference`, the placement hint for new objects in the bucket.",
				map[string]Response{
					"200": jsonResp("Tier set", objectPtr("", map[string]*Schema{
						"object":          strEnum("", "bucket"),
						"name":            str("Bucket name"),
						"tier_preference": strEnum("", "auto", "performance", "standard", "archive", "resilient"),
						"request_id":      str(""),
					}, "object", "name", "tier_preference", "request_id")),
					"400": errResp("`invalid_json` or `invalid_tier`"),
					"404": errResp("`bucket_not_found`"),
					"500": errResp("`db_error` or `no_database`"),
				}),
				jsonBody("Tier preference", objectPtr("", map[string]*Schema{
					"tier": strEnum("One of the placement tiers", "auto", "performance", "standard", "archive", "resilient"),
				}, "tier")))),
		},
		"/api/v1/manage/buckets/{name}/residency": {
			Parameters: []Parameter{name},
			Put: mut(withBody(jsonOp("Management", "Set the bucket's data residency", "ManageSetBucketResidency",
				"Stores `data_residency` (`us`, `eu`) or clears it with `null`.",
				map[string]Response{
					"200": jsonResp("Residency set", objectPtr("", map[string]*Schema{
						"object":         strEnum("", "bucket"),
						"name":           str("Bucket name"),
						"data_residency": nullable(strEnum("", "us", "eu")),
						"request_id":     str(""),
					}, "object", "name", "data_residency", "request_id")),
					"400": errResp("`invalid_json` or `invalid_residency` (must be `us`, `eu` or null)"),
					"404": errResp("`bucket_not_found`"),
					"500": errResp("`db_error` or `no_database`"),
				}),
				jsonBody("Residency", objectPtr("", map[string]*Schema{
					"residency": nullable(strEnum("`us`, `eu`, or null to clear", "us", "eu")),
				})))),
		},
		"/api/v1/manage/keys": {
			Get: mgmt(jsonOp("Management", "List API keys", "ManageListKeys",
				"The caller's keys (including revoked ones) as `api_key` objects without the secret. Not paginated: `has_more` is always false.",
				map[string]Response{
					"200": jsonResp("Keys", listSchema("", ref("APIKey"))),
					"500": errResp("`internal_error`"),
				})),
			Post: mut(withBody(jsonOp("Management", "Create an API key", "ManageCreateKey",
				"Creates a scoped key: `permissions` (S3 operation names, validated), `bucket_scope`, `ip_allowlist` (CIDRs) and `expires_at`. "+
					"`BypassGovernanceRetention` is a privilege, not an operation: it lets the key have `x-amz-bypass-governance-retention` honoured. "+
					"The secret is returned once. Caps: 50 live keys per account, and the plan's own limit (both 409). Emits `key.created`.",
				map[string]Response{
					"201": jsonResp("Key created; `secret` is shown only here", ref("APIKeyCreated")),
					"400": errResp("`invalid_json` or `invalid_permissions` (`unknown permission: ...`)"),
					"409": errResp("`key_limit_exceeded` (50 per account, or the plan's limit) or `idempotency_key_reuse`"),
					"500": errResp("`internal_error`"),
				}),
				jsonBody("Key definition", ref("APIKeyCreateRequest")))),
		},
		"/api/v1/manage/keys/{id}": {
			Parameters: []Parameter{pathParam("id", "API key id")},
			Delete: mut(jsonOp("Management", "Revoke an API key", "ManageDeleteKey",
				"Sets `revoked_at` and emits `key.revoked`. An id that is not one of the caller's keys fails as `internal_error` (500), not 404.",
				map[string]Response{
					"200": jsonResp("Revoked", objectPtr("", map[string]*Schema{
						"object":     strEnum("", "api_key"),
						"id":         str("Key id"),
						"deleted":    boolean("Always true"),
						"request_id": str(""),
					}, "object", "id", "deleted", "request_id")),
					"500": errResp("`internal_error` — including an unknown or foreign key id"),
				})),
		},
		"/api/v1/manage/usage": {
			Get: mgmt(jsonOp("Management", "Get tenant usage", "ManageGetUsage",
				"Storage used and limit for the JWT's tenant plus the plan tier.",
				map[string]Response{
					"200": jsonResp("Usage", ref("Usage")),
					"500": errResp("`internal_error`"),
				})),
		},
		"/api/v1/manage/account/export": {
			Post: mut(jsonOp("Management", "Request a data export (GDPR)", "ManageExportData",
				"Records the request and answers **202** with the export id; a background job renders the export within about a minute into a private system bucket of the account "+
					"(`_exports`, invisible to ListBuckets; the object counts against the standard-floor quota but is written even when the account is over quota). "+
					"Poll GET /account/export/{id} for the status and, once `completed`, the download URL. One export at a time per user: **409** `export_in_progress` while one is pending. "+
					"The export is one JSON document: user profile, security (MFA on/off, MFA events), tenant, quota (totals, floors, house), buckets (every setting), objects "+
					"(key, size, etag, content type, the storage class the customer sees, timestamps, user metadata, tags), object versions, object locks, multipart uploads in flight, "+
					"API keys (ids, names, permissions, scope — never a secret), 90 days of bandwidth, events, webhooks (url, events — never the signing secret), OAuth links (provider only), "+
					"dashboard sessions (created, last seen, user agent, ip), sign-up attribution, the export records and the account's audit trail. Exports are kept for 7 days. No request body.",
				map[string]Response{
					"202": jsonResp("The export was requested", ref("DataExportRequested")),
					"409": errResp("`export_in_progress`"),
					"500": errResp("`export_failed` · `export_unavailable`"),
				})),
		},
		"/api/v1/manage/account/export/{id}": {
			Parameters: []Parameter{pathParam("id", "Export id")},
			Get: mgmt(jsonOp("Management", "Get an export's status and download URL", "ManageGetExport",
				"The status of one of the caller's exports (`pending` → `completed` | `failed`). When `completed`, `download_url` is a presigned S3 GET of the export object, "+
					"signed with the account's primary key pair and valid for one hour (`download_url_expires_at`) — call again for a fresh one; a rotated primary key invalidates it. "+
					"The object itself expires 7 days after completion (`expires_at`): **410** `export_expired` after that (the record stays). "+
					"**404** for an id that is not the caller's — existence is never confirmed.",
				map[string]Response{
					"200": jsonResp("Export status", ref("DataExportStatus")),
					"404": errResp("`export_not_found` (also when the export belongs to another user)"),
					"410": errResp("`export_expired`"),
				})),
		},
		"/api/v1/manage/account": {
			Delete: mut(withBody(jsonOp("Management", "Schedule account deletion (GDPR)", "ManageDeleteAccount",
				"Marks the user `pending_deletion` with `deletion_scheduled_at` = now + 30 days. Login keeps working during the grace period; "+
					"a second call returns the existing date. Reverse it with POST /account/cancel-deletion. On that date the deletion job cancels the Stripe subscription and erases every object (on its recorded backend), key, session and account record; backups age out within 7 days. Nothing changes during the grace period — export and cancellation keep working. `reason` is capped at 500 characters.",
				map[string]Response{
					"200": jsonResp("Deletion scheduled", ref("AccountDeletionScheduled")),
					"400": errResp("`invalid_json`, or `missing_parameter` when `reason` is empty"),
					"500": errResp("`deletion_failed` (also when no database is configured)"),
				}),
				jsonBody("Why the account is being closed", ref("AccountDeletionRequest")))),
		},
		"/api/v1/manage/account/cancel-deletion": {
			Post: mut(jsonOp("Management", "Cancel a scheduled account deletion", "ManageCancelDeletion",
				"Clears the scheduled deletion and sets the user back to `active`. Fails (500 `cancel_failed`) when nothing is pending. No request body.",
				map[string]Response{
					"200": jsonResp("Cancelled", ref("AccountDeletionCancelled")),
					"500": errResp("`cancel_failed` — including when no deletion was pending"),
				})),
		},
	}
}

// generateWebhookPaths — internal/api/webhooks_routes.go and events.go.
// Webhooks: requireJWT → rate limiter → idempotency; events: no idempotency.
func generateWebhookPaths() map[string]*PathItem {
	id := pathParam("id", "Webhook id")
	mut := func(op *Operation) *Operation { return idempotent(rateLimited(op)) }

	eventTypes := strEnum("Event types the endpoint receives: exact names, `prefix.*`, or `*` for everything",
		"*", "object.*", "bucket.*", "key.*", "sts.*", "webhook.*", "bandwidth.*",
		"object.created", "object.deleted", "object.downloaded", "bucket.created", "bucket.deleted",
		"key.created", "key.revoked", "sts.token_created", "webhook.test", "bandwidth.alert")

	return map[string]*PathItem{
		"/api/v1/webhooks": {
			Post: mut(withBody(jsonOp("Webhooks", "Create a webhook endpoint", "CreateWebhook",
				"Registers a URL the server POSTs matching events to, signed with `X-Webhook-Signature: sha256=<hmac>` over the body using the returned secret. "+
					"Target policy (Review R11-04): `http` or `https` only, no credentials in the URL, not `localhost`, and not a literal loopback, private, "+
					"link-local or metadata address; hostnames are re-checked against their resolved addresses at dial time. Refusals are 400 `invalid_url`.",
				map[string]Response{
					"201": jsonResp("Endpoint created; `secret` is shown only here", ref("Webhook")),
					"400": errResp("`invalid_json`, `missing_url`, `invalid_url` (target policy), `missing_events`, `invalid_events`"),
					"500": errResp("`internal_error`"),
				}),
				jsonBody("Endpoint definition", objectPtr("", map[string]*Schema{
					"url":     str("Delivery URL (https or http; public host only)"),
					"events":  {Type: "array", Items: eventTypes, Description: "Non-empty list of event filters"},
					"enabled": boolean("Default true"),
				}, "url", "events")))),
			Get: rateLimited(withParams(jsonOp("Webhooks", "List webhook endpoints", "ListWebhooks",
				"The tenant's endpoints ordered by id, without secrets. `total_count` is the tenant's endpoint count.",
				map[string]Response{
					"200": jsonResp("Endpoints", listSchema("", ref("Webhook"))),
					"500": errResp("`internal_error`"),
				}),
				refParam("Limit"),
				queryParam("starting_after", "Return endpoints whose id sorts after this one (the previous page's `next_cursor`)", &Schema{Type: "string"}))),
		},
		"/api/v1/webhooks/{id}": {
			Parameters: []Parameter{id},
			Patch: mut(withBody(jsonOp("Webhooks", "Update a webhook endpoint", "UpdateWebhook",
				"Changes any of `url` (same target policy as create), `events`, `enabled`; omitted fields keep their value.",
				map[string]Response{
					"200": jsonResp("Updated endpoint (no secret)", ref("Webhook")),
					"400": errResp("`invalid_json`, `invalid_url` (empty or refused by the target policy), `invalid_events`"),
					"404": errResp("`webhook_not_found`"),
					"500": errResp("`internal_error`"),
				}),
				jsonBody("Fields to change", objectPtr("", map[string]*Schema{
					"url":     str("New delivery URL"),
					"events":  {Type: "array", Items: eventTypes, Description: "Replacement filter list (must not be empty when present)"},
					"enabled": boolean(""),
				})))),
			Delete: mut(jsonOp("Webhooks", "Delete a webhook endpoint", "DeleteWebhook",
				"Removes the endpoint and its delivery history.",
				map[string]Response{
					"200": jsonResp("Deleted", objectPtr("", map[string]*Schema{
						"object":     strEnum("", "webhook"),
						"id":         str("Webhook id"),
						"deleted":    boolean("Always true"),
						"request_id": str(""),
					}, "object", "id", "deleted", "request_id")),
					"404": errResp("`webhook_not_found`"),
					"500": errResp("`internal_error`"),
				})),
		},
		"/api/v1/webhooks/{id}/deliveries": {
			Parameters: []Parameter{id},
			Get: rateLimited(withParams(jsonOp("Webhooks", "List deliveries of a webhook", "ListWebhookDeliveries",
				"Delivery attempts newest first. `next_cursor` is the `created_at` of the last row; pass it back as `cursor`. `total_count` is the endpoint's delivery count.",
				map[string]Response{
					"200": jsonResp("Deliveries", listSchema("", ref("WebhookDelivery"))),
					"404": errResp("`webhook_not_found`"),
					"500": errResp("`internal_error`"),
				}),
				refParam("Limit"),
				queryParam("cursor", "Return deliveries created before this RFC 3339 timestamp (the previous page's `next_cursor`)", &Schema{Type: "string", Format: "date-time"}))),
		},
		"/api/v1/webhooks/{id}/test": {
			Parameters: []Parameter{id},
			Post: mut(jsonOp("Webhooks", "Send a test event", "TestWebhook",
				"Inserts a `webhook.test` event for the tenant and dispatches it asynchronously to every enabled endpoint whose filter matches (not only this one). "+
					"Check the outcome under /deliveries. No request body.",
				map[string]Response{
					"200": jsonResp("Test event queued", objectPtr("", map[string]*Schema{
						"object":     strEnum("", "webhook_test"),
						"event_id":   str("Id of the inserted event"),
						"request_id": str(""),
					}, "object", "event_id", "request_id")),
					"404": errResp("`webhook_not_found`"),
					"500": errResp("`internal_error`"),
				})),
		},
		"/api/v1/events": {
			Get: rateLimited(withParams(jsonOp("Events", "List the tenant's events", "ListEvents",
				"Events newest first, optionally filtered by exact `type`. `next_cursor` is the `created_at` of the last row; pass it back as `cursor`. `total_count` is the matching event count.",
				map[string]Response{
					"200": jsonResp("Events", listSchema("", ref("Event"))),
					"500": errResp("`internal_error`"),
				}),
				refParam("Limit"),
				queryParam("cursor", "Return events created before this RFC 3339 timestamp (the previous page's `next_cursor`)", &Schema{Type: "string", Format: "date-time"}),
				queryParam("type", "Exact event type, e.g. `object.created`", &Schema{Type: "string"}))),
		},
	}
}

// generateSTSPaths — internal/api/sts_routes.go (requireJWT only).
func generateSTSPaths() map[string]*PathItem {
	return map[string]*PathItem{
		"/api/v1/sts/token": {
			Post: withBody(jsonOp("STS", "Mint temporary S3 credentials", "CreateSTSToken",
				"Issues an `ASIA`-prefixed access key / secret pair usable with SigV4 on the S3 API until `expiration`. "+
					"The token's scope is the intersection of the requested scope with a parent scope: by default the account's own full access; "+
					"with `parent_key_id` (Review R11-03) the named key's permissions, bucket scope, IP allowlist and expiry bound it instead. "+
					"A token never inherits the GOVERNANCE bypass: it carries `BypassGovernanceRetention` only when the request names it and the parent may bypass (`*` on a token does not include it). "+
					"`ttl` defaults to 3600 s and is clamped to 43200 s. Emits `sts.token_created` and writes an audit row.",
				map[string]Response{
					"201": jsonResp("Credentials; `secret_key` is shown only here", ref("STSToken")),
					"400": errResp("`invalid_json`, `invalid_permissions` (`unknown permission: ...`), `parent_key_revoked`, or `scope_error` (e.g. `no permissions overlap between parent key and request`)"),
					"401": errResp("Bearer token missing/invalid, or `missing_user`"),
					"404": errResp("`parent_key_not_found` — `parent_key_id` is not one of the caller's keys"),
					"500": errResp("`internal_error`"),
				}),
				jsonBody("Requested scope", ref("STSRequest"))),
		},
	}
}

// generateAdminPaths — the /api/v1/admin group in internal/api/server.go:
// requireJWT + requireAdmin. Breach handlers live in internal/compliance
// (http.Error text on failure); flags in admin_flags.go; audit in
// admin_audit.go; the triggers in dedup_gc.go, smart_demotion.go and
// quota_accounting.go.
func generateAdminPaths() map[string]*PathItem {
	admin := func(op *Operation) *Operation { return adminOnly(op) }
	flagKey := pathParam("key", "Flag key as registered in code (e.g. `signups`, `smart_demotion`, `quota_checkout`)")
	flagResp := jsonResp("The flag's resolved state after the change", objectPtr("", map[string]*Schema{
		"flag": nullable(ref("Flag")),
	}, "flag"))

	return map[string]*PathItem{
		"/api/v1/admin/breach": {
			Post: admin(withBody(jsonOp("Admin", "Report a data breach", "AdminReportBreach",
				"Records a breach in the operator-global `breach_records` ledger (GDPR Art. 33/34) and assesses its severity and notification deadline.",
				map[string]Response{
					"201": jsonResp("The breach record", ref("BreachRecord")),
					"400": textResp("`Invalid request body`"),
					"500": textResp("The breach service failed (its error text)"),
				}),
				jsonBody("Breach report", ref("BreachRequest")))),
		},
		"/api/v1/admin/breaches": {
			Get: admin(withParams(jsonOp("Admin", "List data breaches", "AdminListBreaches",
				"Every breach record, optionally filtered by `severity` and `status`.",
				map[string]Response{
					"200": jsonResp("Breaches", objectPtr("", map[string]*Schema{
						"breaches": arrayOf(ref("BreachRecord")),
						"total":    {Type: "integer", Description: "Number of records returned"},
					}, "breaches", "total")),
					"500": textResp("The breach service failed (its error text)"),
				}),
				queryParam("severity", "Exact severity to filter on", &Schema{Type: "string"}),
				queryParam("status", "Exact status to filter on", &Schema{Type: "string"}))),
		},
		"/api/v1/admin/breach/{id}": {
			Parameters: []Parameter{pathParam("id", "Breach id (UUID)")},
			Patch: admin(withBody(jsonOp("Admin", "Update a data breach", "AdminUpdateBreach",
				"Applies a free-form set of field updates to the record.",
				map[string]Response{
					"204": {Description: "Updated"},
					"400": textResp("`Invalid breach ID` (not a UUID) or `Invalid request body`"),
					"500": textResp("The breach service failed (its error text)"),
				}),
				jsonBody("Fields to update", freeObject("Field name → new value")))),
		},
		"/api/v1/admin/jobs": {
			Get: admin(jsonOp("Admin", "List the background jobs", "AdminListJobs",
				"Every background job this server runs, joined with its `job_runs` row: the schedule, whether a run is in progress, the last start, finish and success, "+
					"the last outcome (`ok`, `error`, `interrupted` — the process stopped during the run — or `running`), the rows it affected and when it is next expected. "+
					"Daily jobs: `inventory` 00:30 UTC, `dedup_gc` 02:30, `retention` 03:30, `account_deletion` 04:30, `routing_truth` 05:30, `smart_demotion` 06:30 (a catch-up check a few minutes after every start, "+
					"then one per hour; a run happens when `job_runs` has no success since the last scheduled time). Interval jobs: `multipart_reaper`, `cdn_rollup`, `idempotency_cleanup`, "+
					"`sts_cleanup`, `session_cleanup`, `bandwidth_alerts` (hourly) and `access_log_delivery` (every 5 minutes). On `ok`, `last_error` is the run's note: items that failed "+
					"without failing the run. A `job_runs` row no job of this process claims is listed with `registered: false`.",
				map[string]Response{
					"200": jsonResp("The jobs", objectPtr("", map[string]*Schema{
						"object": strEnum("", "list"),
						"jobs":   arrayOf(ref("JobRun")),
					}, "object", "jobs")),
					"500": textResp("`could not read job_runs`"),
					"503": textResp("`jobs not available` (no database)"),
				})),
		},
		"/api/v1/admin/jobs/{job}/run": {
			Parameters: []Parameter{pathParam("job", "Job name, as listed by `GET /api/v1/admin/jobs`")},
			Post: admin(withParams(jsonOp("Admin", "Start one run of a background job", "AdminRunJob",
				"Starts one run now and answers **202** as soon as the run holds the job's advisory lock; the run continues on the server after the response "+
					"(a closed connection does not cancel it, a shutdown does). The result is in `GET /api/v1/admin/jobs` (`last_outcome`, `rows_affected`, `last_error`) and in the log. "+
					"**409** `already_running` while another run of the job — the scheduler's, another trigger's, or another process's — holds the lock. "+
					"`dry_run=true` is supported by `smart_demotion` only: it is synchronous, answers 200 with the report, moves nothing and is not recorded as a run. "+
					"Writes an `admin.<job>` audit row. No request body.",
				map[string]Response{
					"200": jsonResp("`dry_run=true` on `smart_demotion`: what the run would do", ref("SmartDemotionResult")),
					"202": jsonResp("The run was started", ref("JobStarted")),
					"400": textResp("`dry_run is only supported by smart_demotion`"),
					"404": textResp("No such job on this server"),
					"409": jsonResp("A run is already in progress", ref("JobAlreadyRunning")),
					"500": textResp("`could not start <job>: ...` (the lock could not be taken)"),
				}),
				queryParam("dry_run", "`smart_demotion` only: report, move nothing", &Schema{Type: "boolean", Default: false}))),
		},
		"/api/v1/admin/dedup-gc": {
			Post: admin(jsonOp("Admin", "Start one dedup GC run", "AdminDedupGC",
				"The same call as `POST /api/v1/admin/jobs/dedup_gc/run`: reconciles chunk reference counts and sweeps unreferenced chunks past their 7-day grace. "+
					"Answers 202 once the run has started; it also runs on its own every day at 02:30 UTC. Writes an `admin.dedup_gc` audit row. No request body.",
				map[string]Response{
					"202": jsonResp("The run was started", ref("JobStarted")),
					"409": jsonResp("A run is already in progress", ref("JobAlreadyRunning")),
					"500": textResp("`could not start dedup_gc: ...`"),
					"503": textResp("The job is not available on this server (no database or engine)"),
				})),
		},
		"/api/v1/admin/smart-demotion": {
			Post: admin(withParams(jsonOp("Admin", "Start one Smart-tier demotion run", "AdminSmartDemotion",
				"The same call as `POST /api/v1/admin/jobs/smart_demotion/run`: the hot→cold demotion job (it also runs on its own every day at 06:30 UTC). "+
					"Answers 202 once the run has started. `dry_run=true` is synchronous and reports what would move without moving it — the flag-dark verification path "+
					"before enabling `smart_demotion` for a tenant. Writes an `admin.smart_demotion` audit row. No request body.",
				map[string]Response{
					"200": jsonResp("`dry_run=true`: what the run would do", ref("SmartDemotionResult")),
					"202": jsonResp("The run was started", ref("JobStarted")),
					"409": jsonResp("A run (or a dry run) is already in progress", ref("JobAlreadyRunning")),
					"500": textResp("`smart demotion failed: ...` (dry run) or `could not start smart_demotion: ...`"),
					"503": textResp("The job is not available on this server (the hot or the cold backend is not registered)"),
				}),
				queryParam("dry_run", "Report only; move nothing", &Schema{Type: "boolean", Default: false}))),
		},
		"/api/v1/admin/quota-reconcile": {
			Post: admin(jsonOp("Admin", "Reconcile quota usage from the HEAD cache", "AdminQuotaReconcile",
				"Rewrites every tenant's `storage_used_bytes` from the `object_head_cache` sum. Run only while writes are quiesced: an in-flight PUT's "+
					"reservation is not yet in the cache and would be erased until the next reconcile. Writes an `admin.quota_reconcile` audit row. No request body.",
				map[string]Response{
					"200": jsonResp("Tenants updated", objectPtr("", map[string]*Schema{
						"object":          strEnum("", "quota_reconcile"),
						"tenants_updated": integer("Number of tenant rows rewritten"),
					}, "object", "tenants_updated")),
					"500": textResp("`reconcile failed: ...`"),
					"503": textResp("`quota reconciliation not available`"),
				})),
		},
		"/api/v1/admin/chunk-move": {
			Post: admin(withParams(jsonOp("Admin", "Move chunk blobs written before WP-R8-7 to their one address", "AdminChunkMove",
				"A chunk blob has one address: its index row's backend, the `_global` container and the reserved tenant `_global` in the driver context "+
					"(`t-_global/_global/<storage_key>` on a fixed-bucket backend). Blobs written before WP-R8-7 are under the uploading tenant's prefix "+
					"(`t-<tenant>/_global/…`). This call works on ONE backend: for every index row there it copies a verified old copy to the one address, "+
					"reads it back, verifies size and hash, and only then deletes the old copies; blobs under a tenant's chunk container with no index row "+
					"(orphans) are deleted. Dry run by default: only `dry_run=false` changes anything. Idempotent and resumable — a run stopped anywhere is "+
					"finished by the next one. Synchronous, on a context detached from the request (a proxy timeout does not stop it; the result is also in "+
					"the `chunk move` log line). Done when `at_address == rows` and `orphans == 0`. Writes an `admin.chunk_move` audit row. No request body.",
				map[string]Response{
					"200": jsonResp("What the run did (or, for a dry run, would do)", ref("ChunkMoveResult")),
					"400": textResp("`backend is required`, `dry_run must be true or false`, an unregistered backend or a `tenant` that is not a tenant id"),
					"409": jsonResp("A chunk move is already running", ref("JobAlreadyRunning")),
					"500": textResp("`chunk move failed (see the log)` — what was done is kept"),
					"503": textResp("`chunk move not available` (no database or engine)"),
				}),
				queryParam("backend", "Required. A registered backend name (e.g. `idrive`)", &Schema{Type: "string"}),
				queryParam("dry_run", "Report only (the default). `false` moves and deletes", &Schema{Type: "boolean", Default: true}),
				queryParam("tenant", "Repeatable. An extra tenant id to look under — an erased tenant whose `account.erased` row shows `chunk_blobs_left` > 0", &Schema{Type: "string"}))),
		},
		"/api/v1/admin/routing-truth": {
			Get: admin(jsonOp("Admin", "The routing truth: head rows vs. the backends that hold their bytes", "AdminRoutingTruth",
				"`object_head_cache.backend_name` is where an object's bytes are supposed to be — GET, DELETE, the demotion ledger, the deletion runner and the erasure sweep act on it. "+
					"This answers with three things (WP-R7-5): the boot check (every backend name on record must be a registered driver; two registered names must never share a store), "+
					"the rows whose backend no driver is registered under — per table, live from the database, `backend` empty for rows with none — and the last run of the daily "+
					"`routing_truth` job (05:30 UTC), which samples rows per backend and asks the recorded backend only whether the bytes are there: `present`, `missing` (bytes gone, row intact), "+
					"`changed` (the row changed while it looked), `errors` (the backend could not be asked; an open breaker is one error, never N misses), plus the chunks of sampled chunked objects "+
					"at their one address (`legacy` = found under the uploader's prefix; run the chunk move). Nothing here writes. Run the job now with `POST /api/v1/admin/jobs/routing_truth/run`. "+
					"What to do with the findings is the plan in `docs/reviews/WP-R7-5.md`.",
				map[string]Response{
					"200": jsonResp("The routing truth", ref("RoutingTruth")),
					"500": textResp("`could not read the routing tables` / `could not read job_runs`"),
					"503": textResp("`routing truth not available (no database)`"),
				})),
		},
		"/api/v1/admin/routing-truth/resolve-null": {
			Post: admin(withParams(jsonOp("Admin", "Resolve head rows that record no backend", "AdminRoutingTruthResolveNull",
				"Rows with no `backend_name` (multipart completes before Review R3-03) are served only by the engine's fan-out. This is the one place a fan-out is justified: "+
					"for up to `limit` such rows every registered driver is asked once, with the row's tenant in the context, and the row is `resolved` when exactly ONE holds the bytes, "+
					"`nowhere` when none does (the object is gone — delete it through S3 DELETE, which releases quota, or the tenant's erasure), `ambiguous` when more than one does, "+
					"`error` when a backend could not be asked. **Dry run by default**: only `dry_run=false` writes, and then only the resolved rows, under a guard (`backend_name IS NULL` and the etag unchanged). "+
					"Nothing is ever deleted. Synchronous; call again until `remaining` is 0. Writes an `admin.routing_truth_resolve_null` audit row. No request body.",
				map[string]Response{
					"200": jsonResp("What was (or would be) resolved", ref("RoutingTruthResolveNull")),
					"400": textResp("`dry_run must be true or false`, `limit must be an integer in 1..500`"),
					"500": textResp("`resolve failed: ...`"),
					"503": textResp("`routing truth not available (no database)`"),
				}),
				queryParam("dry_run", "Report only (the default). `false` writes the resolved rows", &Schema{Type: "boolean", Default: true}),
				queryParam("limit", "Rows looked at in this call (each costs one HEAD per registered backend)", intRange("", 1, 500)))),
		},
		"/api/v1/admin/retention": {
			Post: admin(jsonOp("Admin", "Start one retention run", "AdminRetention",
				"The same call as `POST /api/v1/admin/jobs/retention/run`: prunes the log tables past their retention periods (s3_access_log 30 d, events / quota_usage_events / stripe_events 90 d, "+
					"webhook_deliveries 30 d, cdn_access_log 2 d after rollup, waitlist sign-up IP/user agent cleared after 90 d; audit_logs never) in batches under "+
					"the job's advisory lock — the nightly 03:30 UTC run and this trigger cannot overlap. Answers 202 once the run has started. Writes an `admin.retention` audit row. No request body.",
				map[string]Response{
					"202": jsonResp("The run was started", ref("JobStarted")),
					"409": jsonResp("A run is already in progress", ref("JobAlreadyRunning")),
					"500": textResp("`could not start retention: ...`"),
					"503": textResp("The job is not available on this server (no database)"),
				})),
		},
		"/api/v1/admin/account-deletion": {
			Post: admin(jsonOp("Admin", "Start one account-deletion run", "AdminAccountDeletion",
				"The same call as `POST /api/v1/admin/jobs/account_deletion/run`: erases every account whose 30-day grace period has ended (WP-R10-3) — per tenant, cancel the Stripe subscription, "+
					"delete every object on its recorded backend (chunked manifests released, multipart uploads aborted), then remove every tenant/user row in one transaction, revoke sessions and "+
					"write an `account.erased` audit row. A tenant that Stripe or a backend refuses is deferred: the run's outcome is `error` with the per-tenant reasons in `last_error`, "+
					"and the hourly check retries it. One advisory lock — the daily 04:30 UTC run and this trigger cannot overlap. Answers 202 once the run has started. "+
					"Writes an `admin.account_deletion` audit row. No request body.",
				map[string]Response{
					"202": jsonResp("The run was started", ref("JobStarted")),
					"409": jsonResp("A run is already in progress", ref("JobAlreadyRunning")),
					"500": textResp("`could not start account_deletion: ...`"),
					"503": textResp("The job is not available on this server (no database or engine)"),
				})),
		},
		"/api/v1/admin/flags": {
			Get: admin(jsonOp("Admin", "List feature flags", "AdminListFlags",
				"Every flag registered in code with its default, the global row (if any), the effective global state and every per-tenant override.",
				map[string]Response{
					"200": jsonResp("Flags", objectPtr("", map[string]*Schema{
						"flags": arrayOf(ref("Flag")),
					}, "flags")),
				})),
		},
		"/api/v1/admin/flags/{key}": {
			Parameters: []Parameter{flagKey},
			Put: admin(withBody(jsonOp("Admin", "Set a feature flag", "AdminSetFlag",
				"Upserts the global row (`tenant_id` omitted or empty) or a per-tenant override. `updated_by` is the JWT's email, never the body. "+
					"Only keys registered in code are settable (400 otherwise). Writes a `flag.set` audit row.",
				map[string]Response{
					"200": flagResp,
					"400": textResp("`unknown flag: <key>` or `invalid body: expected {\"enabled\": bool, \"tenant_id\"?: string}`"),
					"500": textResp("`failed to set flag` (e.g. no database)"),
				}),
				jsonBody("New state", objectPtr("", map[string]*Schema{
					"enabled":   boolean("Required"),
					"tenant_id": str("Tenant to override for; omit for the global row"),
				}, "enabled")))),
			Delete: admin(withParams(jsonOp("Admin", "Unset a feature flag", "AdminUnsetFlag",
				"Deletes the global row (`tenant_id` omitted) or one tenant's override, reverting to the next level down (override → global → code default).",
				map[string]Response{
					"200": flagResp,
					"400": textResp("`unknown flag: <key>`"),
					"500": textResp("`failed to unset flag`"),
				}),
				queryParam("tenant_id", "Tenant whose override to remove; omit for the global row", &Schema{Type: "string"}))),
		},
		"/api/v1/admin/audit": {
			Get: admin(withParams(jsonOp("Admin", "List the operator audit trail", "AdminListAudit",
				"Rows from `audit_logs` (Review R11-09) newest first with a keyset cursor: key lifecycle, password/MFA changes, registration, flags, "+
					"account deletion, exports, STS, webhooks and admin actions, each with actor (`performed_by`), subject, client IP and user agent. "+
					"Filters combine with AND. `total_count` is the size of the page.",
				map[string]Response{
					"200": jsonResp("Audit rows", listSchema("", ref("AuditRow"))),
					"400": errResp("`invalid_cursor` — the cursor is not one this endpoint issued"),
					"500": errResp("`db_error`"),
				}),
				queryParam("tenant_id", "Rows for this tenant", &Schema{Type: "string"}),
				queryParam("user_id", "Rows about this subject user", &Schema{Type: "string"}),
				queryParam("action", "Exact action, e.g. `key.revoked`, `flag.set`, `admin.dedup_gc`", &Schema{Type: "string"}),
				queryParam("event_type", "Action family: `key`, `flag`, `account`, `admin`, `auth`, ...", &Schema{Type: "string"}),
				refParam("Cursor"),
				queryParam("limit", "Page size; default 50, clamped to 100", intRange("", 1, 100)))),
		},
	}
}

// --- components ------------------------------------------------------------

func generateSchemas() map[string]Schema {
	schemas := generateS3Schemas()
	for name, s := range generateJSONSchemas() {
		schemas[name] = s
	}
	return schemas
}

func generateS3Schemas() map[string]Schema {
	return map[string]Schema{
		"Bucket": {
			Type: "object",
			Properties: map[string]*Schema{
				"Name": {
					Type:        "string",
					Description: "Bucket name",
				},
				"CreationDate": {
					Type:        "string",
					Format:      "date-time",
					Description: "Bucket creation timestamp",
				},
			},
			Required: []string{"Name", "CreationDate"},
		},
		"ListBucketsResponse": {
			Type: "object",
			XML:  &XMLObject{Name: "ListAllMyBucketsResult"},
			Properties: map[string]*Schema{
				"Buckets": {
					Type: "array",
					XML:  &XMLObject{Name: "Buckets", Wrapped: true},
					Items: &Schema{
						Ref: "#/components/schemas/Bucket",
					},
				},
				"Owner": {
					Type: "object",
					Properties: map[string]*Schema{
						"ID": {
							Type: "string",
						},
						"DisplayName": {
							Type: "string",
						},
					},
				},
			},
		},
		"Object": {
			Type: "object",
			Properties: map[string]*Schema{
				"Key": {
					Type:        "string",
					Description: "Object key",
				},
				"LastModified": {
					Type:        "string",
					Format:      "date-time",
					Description: "Last modification timestamp",
				},
				"ETag": {
					Type:        "string",
					Description: "Entity tag",
				},
				"Size": {
					Type:        "integer",
					Format:      "int64",
					Description: "Object size in bytes",
				},
				"StorageClass": {
					Type:        "string",
					Description: "Storage class",
					Enum:        []interface{}{"STANDARD", "REDUCED_REDUNDANCY", "GLACIER"},
				},
			},
			Required: []string{"Key", "LastModified", "ETag", "Size"},
		},
		"ListObjectsResponse": {
			Type: "object",
			XML:  &XMLObject{Name: "ListBucketResult"},
			Properties: map[string]*Schema{
				"Name": {
					Type:        "string",
					Description: "Bucket name",
				},
				"Prefix": {
					Type:        "string",
					Description: "Object prefix",
				},
				"MaxKeys": {
					Type:        "integer",
					Description: "Maximum keys returned",
				},
				"IsTruncated": {
					Type:        "boolean",
					Description: "Whether the results were truncated",
				},
				"Contents": {
					Type: "array",
					XML:  &XMLObject{Name: "Contents"},
					Items: &Schema{
						Ref: "#/components/schemas/Object",
					},
				},
			},
		},
		"Error": {
			Type:        "object",
			Description: "S3 error body (XML)",
			XML:         &XMLObject{Name: "Error"},
			Properties: map[string]*Schema{
				"Code": {
					Type:        "string",
					Description: "Error code",
					Example:     "NoSuchBucket",
				},
				"Message": {
					Type:        "string",
					Description: "Error message",
					Example:     "The specified bucket does not exist",
				},
				"Resource": {
					Type:        "string",
					Description: "Resource associated with error",
				},
				"RequestId": {
					Type:        "string",
					Description: "Request ID for debugging",
				},
			},
			Required: []string{"Code", "Message"},
		},
	}
}

// generateJSONSchemas documents the JSON API bodies as the handlers write
// them (internal/api). Field names are the wire names.
func generateJSONSchemas() map[string]Schema {
	permissions := &Schema{
		Type:        "array",
		Description: "S3 operation names the key may perform (`GetObject`, `PutObject`, `ListBuckets`, ...) or `*` for all; empty = full access. `BypassGovernanceRetention` adds the privilege to bypass GOVERNANCE Object Lock retention with `x-amz-bypass-governance-retention` (full-access keys have it)",
		Items:       &Schema{Type: "string"},
	}
	apiKeyProps := func(withSecret, withRequestID bool) map[string]*Schema {
		p := map[string]*Schema{
			"object":       strEnum("", "api_key"),
			"id":           str("Key id (use in the key endpoints' path)"),
			"name":         str("Key name"),
			"key":          str("Access key id (the S3 `AccessKeyId`)"),
			"permissions":  permissions,
			"bucket_scope": nullable(strArray("Buckets the key is limited to; empty/null = all")),
			"ip_allowlist": nullable(strArray("CIDRs the key may be used from; empty/null = anywhere")),
			"expires_at":   nullable(dateTime("Expiry, or null")),
			"created_at":   dateTime(""),
		}
		if withSecret {
			p["secret"] = str("Secret access key — returned only by this call")
		}
		if withRequestID {
			p["request_id"] = str("")
		}
		return p
	}

	return map[string]Schema{
		"JSONError": object("JSON API error envelope (writeManagementError)", map[string]*Schema{
			"error": objectPtr("", map[string]*Schema{
				"type": strEnum("Error class; determines the status code",
					"invalid_request_error", "authentication_error", "permission_error", "not_found_error",
					"conflict_error", "rate_limit_error", "api_error"),
				"code":       str("Machine-readable code, e.g. `bucket_not_found`"),
				"message":    str("Human-readable message"),
				"param":      str("The request field or header the error is about, when there is one"),
				"request_id": str("Request id echoed from X-Request-Id"),
			}, "type", "message", "request_id"),
		}, "error"),
		"SimpleError": object("Minimal error body written by the waitlist and presign handlers", map[string]*Schema{
			"error": str("Error message"),
		}, "error"),
		"ListEnvelope": *listSchema("The collection envelope every paginated JSON endpoint returns; `data` items are documented per endpoint", freeObject("")),

		// Auth
		"RegisterRequest": object("", map[string]*Schema{
			"email":    str("Login email; must parse as an address"),
			"password": str("Password; rejected below the minimum length"),
			"company":  str("Company / tenant display name"),
		}, "email", "password"),
		"RegisterResponse": object("S3 credentials of the new account's primary key (shown once)", map[string]*Schema{
			"accessKeyId":     str("S3 access key id"),
			"secretAccessKey": str("S3 secret access key"),
			"endpoint":        str("S3 endpoint to use with the credentials"),
		}, "accessKeyId", "secretAccessKey", "endpoint"),
		"LoginRequest": object("", map[string]*Schema{
			"email":    str(""),
			"password": str(""),
		}, "email", "password"),
		"LoginResponse": object("", map[string]*Schema{
			"token":     str("The Bearer JWT (HS256, valid 24 h)"),
			"tenant_id": str("The user's tenant id"),
		}, "token", "tenant_id"),
		"WaitlistSignup": object("", map[string]*Schema{
			"email":        str("Email address (required, at most 320 characters)"),
			"std_tb":       str("Standard-tier TB planned on the landing page (number or numeric string)"),
			"vault_tb":     str("Vault-tier TB planned (number or numeric string)"),
			"room":         str("Share-link room of the house built on the landing page"),
			"referrer":     str("document.referrer host; the Referer header is the fallback"),
			"utm_source":   str(""),
			"utm_medium":   str(""),
			"utm_campaign": str(""),
		}, "email"),

		"SitePing": object("", map[string]*Schema{
			"event":        str("Event name from the closed list (required)"),
			"referrer":     str("document.referrer host, if any"),
			"utm_source":   str(""),
			"utm_medium":   str(""),
			"utm_campaign": str(""),
		}, "event"),

		// User
		"UserInfo": object("The account as persisted (GET /api/v1/user)", map[string]*Schema{
			"id":                    str("User id"),
			"email":                 str(""),
			"tenant_id":             str(""),
			"company":               str(""),
			"role":                  str("`admin` for operators; empty or `user` otherwise"),
			"status":                str("`active` or `pending_deletion`"),
			"deletion_scheduled_at": dateTime("Present while a deletion is scheduled"),
			"quota":                 ref("QuotaInfo"),
			"mfa_enabled":           boolean("Whether TOTP MFA is enabled"),
			"created_at":            dateTime(""),
		}, "id", "email", "tenant_id", "quota", "mfa_enabled", "created_at"),
		"QuotaInfo": object("", map[string]*Schema{
			"tenant_id":           str(""),
			"storage_used_bytes":  integer(""),
			"storage_limit_bytes": integer(""),
			"usage_percentage":    number("used / limit × 100 (0 when the limit is 0)"),
			"tier":                str("Plan tier, e.g. `free`, `standard`, `vault`, `enterprise`"),
			"can_upgrade":         boolean("`tier != enterprise`"),
		}, "tenant_id", "storage_used_bytes", "storage_limit_bytes", "usage_percentage", "tier", "can_upgrade"),
		"UsageStats": object("", map[string]*Schema{
			"tenant_id":                       str(""),
			"storage_used":                    integer("Bytes"),
			"storage_limit":                   integer("Bytes"),
			"usage_percent":                   number("used / limit × 100"),
			"bandwidth_used":                  integer("Bytes; omitted when zero"),
			"egress_allowance":                integer("This month's egress allowance in bytes: 0.5 × the downstairs (Standard) quota + 1 × the attic (Vault) quota, or the override an operator set"),
			"egress_used":                     integer("Bytes downloaded this UTC calendar month (S3 GetObject and public-bucket CDN bodies, responses in flight included). Uploads never count"),
			"egress_throttled":                boolean("True when the allowance is used up and downloads are being rate-limited"),
			"egress_rate_limit_bytes_per_sec": integer("The total download rate that applies to the account past the allowance"),
			"egress_resets_at":                dateTime("When the allowance resets: the start of the next UTC month"),
			"last_updated":                    dateTime("When this response was computed"),
		}, "tenant_id", "storage_used", "storage_limit", "usage_percent", "last_updated"),
		"UsageAlert": object("", map[string]*Schema{
			"level":     strEnum("", "INFO", "WARNING", "CRITICAL"),
			"message":   str(""),
			"threshold": number("The percentage threshold crossed (70, 80 or 90)"),
			"current":   number("Current usage percentage"),
			"timestamp": dateTime(""),
		}, "level", "message", "threshold", "current", "timestamp"),
		"UsageHistoryEntry": object("One day of quota_usage_events", map[string]*Schema{
			"date":       str("Day (YYYY-MM-DD)"),
			"peak_usage": integer("Largest single byte delta that day"),
			"uploaded":   integer("Bytes added by PUTs"),
			"deleted":    integer("Bytes removed by DELETEs"),
		}, "date", "peak_usage", "uploaded", "deleted"),
		"UserAPIKey": object("An API key row as the auth service serialises it (secret blanked)", map[string]*Schema{
			"id":           str("Key id"),
			"user_id":      str(""),
			"tenant_id":    str(""),
			"name":         str(""),
			"key":          str("Access key id"),
			"secret":       str("Always omitted/empty after creation"),
			"permissions":  permissions,
			"bucket_scope": nullable(strArray("")),
			"ip_allowlist": nullable(strArray("")),
			"expires_at":   dateTime("Omitted when unset"),
			"last_used":    dateTime("Omitted when never used"),
			"created_at":   dateTime(""),
			"revoked_at":   dateTime("Present once the key is revoked"),
			"metadata":     nullable(&Schema{Type: "object", AdditionalProperties: &Schema{Type: "string"}}),
			"usage_count":  integer("Requests authenticated with the key"),
			"last_ip":      str("Omitted when never used"),
		}, "id", "name", "key", "permissions", "created_at"),
		"UserAPIKeyCreateRequest": object("", map[string]*Schema{
			"name":        str("Key name"),
			"permissions": permissions,
			"expiry_days": {Type: "integer", Description: "Days until the key expires; omitted or ≤ 0 = never"},
		}),
		"UserAPIKeyCreated": object("Created key with its secret (POST /api/v1/user/apikeys)", map[string]*Schema{
			"id":          str("Key id"),
			"name":        str(""),
			"key":         str("Access key id"),
			"secret":      str("Secret access key — returned only by this call"),
			"permissions": permissions,
			"expires_at":  nullable(dateTime("")),
			"created_at":  dateTime(""),
		}, "id", "name", "key", "secret", "permissions", "created_at"),

		// Management
		"ManagedBucketCreateRequest": object("", map[string]*Schema{
			"name":   str("3–63 characters, lowercase alphanumeric and hyphens"),
			"region": str("Region id the bucket is homed in (e.g. `us-central-1`, `eu-west-1`); omit for the deployment default. Only regions with an enabled driver are accepted"),
		}, "name"),
		"ManagedBucket": object("A bucket as the management API returns it", map[string]*Schema{
			"object":     strEnum("", "bucket"),
			"name":       str(""),
			"region":     str("Region the bucket is homed in; present on create"),
			"metadata":   nullable(&Schema{Type: "object", Description: "User metadata map; present on get/patch (null in list items)", AdditionalProperties: &Schema{Type: "string"}}),
			"created_at": dateTime(""),
			"request_id": str("Present on single-object responses"),
		}, "object", "name", "created_at"),
		"BucketDeleted": object("", map[string]*Schema{
			"object":     strEnum("", "bucket"),
			"name":       str(""),
			"deleted":    boolean("Always true"),
			"request_id": str(""),
		}, "object", "name", "deleted", "request_id"),
		"ManagedObject": object("An object as listed from the HEAD cache", map[string]*Schema{
			"object":        strEnum("", "object"),
			"key":           str(""),
			"size":          integer("Bytes"),
			"etag":          str(""),
			"content_type":  str(""),
			"last_modified": dateTime(""),
		}, "object", "key", "size", "etag", "content_type", "last_modified"),
		"APIKey":        object("An API key as the management API lists it (no secret)", apiKeyProps(false, false), "object", "id", "name", "key", "created_at"),
		"APIKeyCreated": object("Created key with its secret (POST /api/v1/manage/keys)", apiKeyProps(true, true), "object", "id", "name", "key", "secret", "created_at", "request_id"),
		"APIKeyCreateRequest": object("", map[string]*Schema{
			"name":         str("Key name"),
			"permissions":  permissions,
			"bucket_scope": strArray("Buckets the key is limited to"),
			"ip_allowlist": strArray("CIDRs the key may be used from"),
			"expires_at":   dateTime("Absolute expiry"),
		}),
		"Usage": object("", map[string]*Schema{
			"object":        strEnum("", "usage"),
			"tenant_id":     str(""),
			"storage_used":  integer("Bytes"),
			"storage_limit": integer("Bytes"),
			"usage_percent": number("used / limit × 100"),
			"tier":          str("Plan tier"),
			"request_id":    str(""),
		}, "object", "tenant_id", "storage_used", "storage_limit", "usage_percent", "tier", "request_id"),
		"DataExportRequested": object("A requested GDPR export (POST /api/v1/manage/account/export)", map[string]*Schema{
			"object":     strEnum("", "data_export"),
			"id":         str("Export id — poll GET /account/export/{id}"),
			"status":     strEnum("", "pending"),
			"request_id": str(""),
		}, "object", "id", "status", "request_id"),
		"DataExportStatus": object("", map[string]*Schema{
			"object":                  strEnum("", "data_export"),
			"id":                      str(""),
			"status":                  strEnum("", "pending", "completed", "failed"),
			"file_size_bytes":         integer("Size of the export object (0 until completed)"),
			"created_at":              dateTime(""),
			"completed_at":            dateTime("When the object was written (completed only)"),
			"expires_at":              dateTime("When the object is removed — 7 days after completion (completed only)"),
			"etag":                    str("MD5 of the export object (completed only)"),
			"download_url":            str("Presigned GET of the export object, valid one hour (completed only; never logged — treat as a credential)"),
			"download_url_expires_at": dateTime("(completed only)"),
			"error":                   str("Why the export failed (failed only)"),
			"request_id":              str(""),
		}, "object", "id", "status", "file_size_bytes", "created_at", "request_id"),
		"AccountDeletionRequest": object("", map[string]*Schema{
			"reason": str("Why the account is being closed (required, stored on the user row)"),
		}, "reason"),
		"AccountDeletionScheduled": object("", map[string]*Schema{
			"object":       strEnum("", "account_deletion"),
			"scheduled_at": dateTime("When the grace period ends (now + 30 days, or the previously scheduled date)"),
			"message":      str("What happens next and how to cancel"),
			"request_id":   str(""),
		}, "object", "scheduled_at", "message", "request_id"),
		"AccountDeletionCancelled": object("", map[string]*Schema{
			"object":     strEnum("", "account_deletion"),
			"cancelled":  boolean("Always true"),
			"message":    str(""),
			"request_id": str(""),
		}, "object", "cancelled", "message", "request_id"),

		// Webhooks / events
		"Webhook": object("A webhook endpoint", map[string]*Schema{
			"object":     strEnum("", "webhook"),
			"id":         str("Webhook id (UUID)"),
			"url":        str("Delivery URL"),
			"events":     strArray("Event filters (exact types, `prefix.*`, or `*`)"),
			"secret":     str("`whsec_...` HMAC-SHA256 key for `X-Webhook-Signature` — returned only on create"),
			"enabled":    boolean(""),
			"created_at": dateTime(""),
			"updated_at": dateTime("Present on list and update"),
			"request_id": str("Present on single-object responses"),
		}, "object", "id", "url", "events", "enabled", "created_at"),
		"WebhookDelivery": object("One delivery attempt", map[string]*Schema{
			"object":        strEnum("", "webhook_delivery"),
			"id":            str(""),
			"event_id":      str("The event delivered"),
			"status":        strEnum("", "delivered", "failed"),
			"response_code": {Type: "integer", Description: "HTTP status the target answered (0 when the request failed)"},
			"latency_ms":    {Type: "integer"},
			"retry_count":   {Type: "integer"},
			"created_at":    dateTime(""),
		}, "object", "id", "event_id", "status", "response_code", "latency_ms", "retry_count", "created_at"),
		"Event": object("A tenant event; webhooks receive `{id, type, tenant_id, data, created_at}`", map[string]*Schema{
			"object":     strEnum("", "event"),
			"id":         str("Event id (UUID)"),
			"type":       strEnum("", "object.created", "object.deleted", "object.downloaded", "bucket.created", "bucket.deleted", "key.created", "key.revoked", "sts.token_created", "webhook.test", "bandwidth.alert"),
			"data":       freeObject("Type-specific payload"),
			"created_at": dateTime(""),
		}, "object", "id", "type", "data", "created_at"),

		// STS
		"STSRequest": object("", map[string]*Schema{
			"permissions":   permissions,
			"bucket_scope":  strArray("Buckets the token is limited to; intersected with the parent's scope"),
			"ip_restrict":   strArray("CIDRs the token may be used from; narrowed by the parent's allowlist"),
			"ttl":           intRange("Lifetime in seconds; default 3600, clamped to 43200 (12 h)", 1, 43200),
			"parent_key_id": str("One of the caller's own API key ids whose scope bounds the token; omit for the account's full authority"),
		}),
		"STSToken": object("Temporary S3 credentials", map[string]*Schema{
			"object":     strEnum("", "sts_token"),
			"access_key": str("`ASIA`-prefixed access key id"),
			"secret_key": str("Secret access key — returned only by this call"),
			"expiration": dateTime("When the credentials stop working (RFC 3339)"),
			"request_id": str(""),
		}, "object", "access_key", "secret_key", "expiration", "request_id"),

		// Admin
		"Flag": object("A feature flag's resolved state (internal/flags)", map[string]*Schema{
			"key":            str("Flag key"),
			"registered":     boolean("Whether the key is registered in code"),
			"default":        boolean("The in-code default"),
			"has_global_row": boolean("Whether a global (`*`) row exists"),
			"global":         boolean("The global row's value; omitted when false"),
			"enabled":        boolean("Effective global state (global row, else default)"),
			"overrides":      arrayOf(ref("FlagOverride")),
		}, "key", "registered", "default", "has_global_row", "enabled"),
		"FlagOverride": object("A per-tenant flag row", map[string]*Schema{
			"tenant_id":  str(""),
			"enabled":    boolean(""),
			"updated_by": str("Email of the admin who set it"),
			"updated_at": dateTime(""),
		}, "tenant_id", "enabled"),
		"AuditRow": object("One audit_logs row (internal/audit)", map[string]*Schema{
			"id":           str(""),
			"timestamp":    dateTime(""),
			"user_id":      str("The subject the action is about"),
			"performed_by": str("The actor"),
			"tenant_id":    str(""),
			"event_type":   str("Action family: `key`, `flag`, `account`, `admin`, `auth`, ..."),
			"action":       str("e.g. `key.revoked`, `flag.set`, `account.deletion_scheduled`"),
			"resource":     str("e.g. `key:<id>`, `flag:<key>`, `user:<id>`"),
			"result":       strEnum("", "success", "failure"),
			"severity":     str("`info`, `warning` or `error`"),
			"ip":           str("Client IP (internal/clientip)"),
			"user_agent":   str(""),
			"error":        str("Error message on failure"),
			"metadata":     freeObject("Action-specific details"),
		}, "id", "timestamp", "event_type", "action", "result", "severity", "metadata"),
		"BreachRequest": object("", map[string]*Schema{
			"breach_type":           str(""),
			"description":           str(""),
			"root_cause":            str(""),
			"detected_at":           dateTime("Defaults to now"),
			"affected_user_count":   {Type: "integer"},
			"affected_record_count": {Type: "integer"},
			"data_categories":       strArray(""),
			"affected_user_ids":     strArray("User UUIDs"),
		}),
		"BreachRecord": object("A breach_records row (Go field names, no JSON tags)", map[string]*Schema{
			"ID":                  str("UUID"),
			"BreachType":          str(""),
			"Severity":            str(""),
			"Status":              str(""),
			"DetectedAt":          dateTime(""),
			"ReportedAt":          nullable(dateTime("")),
			"AffectedUserCount":   {Type: "integer"},
			"AffectedRecordCount": {Type: "integer"},
			"DataCategories":      nullable(strArray("")),
			"Description":         str(""),
			"RootCause":           str(""),
			"Consequences":        str(""),
			"Mitigation":          str(""),
			"NotifiedAuthority":   boolean(""),
			"NotifiedSubjects":    boolean(""),
			"AuthorityNotifiedAt": nullable(dateTime("")),
			"SubjectsNotifiedAt":  nullable(dateTime("")),
			"DeadlineAt":          dateTime("72-hour authority notification deadline"),
			"Metadata":            nullable(freeObject("")),
			"CreatedAt":           dateTime(""),
			"UpdatedAt":           dateTime(""),
		}, "ID", "BreachType", "Severity", "Status", "DetectedAt", "DeadlineAt", "CreatedAt", "UpdatedAt"),
		"JobRun": object("One background job and its `job_runs` row", map[string]*Schema{
			"job":              str("Job name"),
			"schedule":         str("`daily HH:MM UTC` or `every <interval>`; empty for an unregistered row"),
			"registered":       boolean("False for a `job_runs` row that no job of this server claims"),
			"running":          boolean("A run is recorded as in progress"),
			"last_started_at":  nullable(dateTime("")),
			"last_finished_at": nullable(dateTime("")),
			"last_success_at":  nullable(dateTime("The last run whose outcome was `ok`; what `vaultaire_job_last_success_timestamp_seconds{job_name}` exports")),
			"last_outcome":     strEnum("Empty when the job has never run", "", "ok", "error", "interrupted", "running"),
			"last_error":       str("The failure text of an `error` run; on `ok`, the run's note (items that failed without failing the run)"),
			"rows_affected":    integer("The job's own unit: rows pruned, objects moved, chunks swept, reports written, accounts erased"),
			"next_run_at":      dateTime("When the job is next expected to run; omitted when unknown"),
			"result":           freeObject("The structured result of the last run that wrote one (`routing_truth`: the per-backend counts); omitted for the other jobs"),
		}, "job", "schedule", "registered", "running", "last_started_at", "last_finished_at", "last_success_at", "last_outcome", "last_error", "rows_affected"),
		"RoutingTruth": object("The routing truth (WP-R7-5)", map[string]*Schema{
			"job":                 str("The job's name in `job_runs`: `routing_truth`"),
			"registered_backends": strArray("Every registered driver name"),
			"boot_check": nullable(objectPtr("What the boot check found (null before it ran)", map[string]*Schema{
				"at":                   dateTime(""),
				"registered_backends":  strArray(""),
				"unknown_backend_rows": arrayOf(ref("UnknownBackendRows")),
				"shared_stores":        arrayOf(ref("SharedStore")),
			})),
			"unknown_backend_rows": arrayOf(ref("UnknownBackendRows")),
			"shared_stores":        arrayOf(ref("SharedStore")),
			"last_outcome":         strEnum("Empty when the job has never run", "", "ok", "error", "interrupted", "running"),
			"last_started_at":      nullable(dateTime("")),
			"last_finished_at":     nullable(dateTime("")),
			"last_success_at":      nullable(dateTime("")),
			"last_note":            str("The last run's one-line summary (or its error)"),
			"last_run": nullable(objectPtr("The last run's counts (`job_runs.result`); null before the first run", map[string]*Schema{
				"started_at":  dateTime(""),
				"finished_at": dateTime(""),
				"backends": freeObject("Per registered backend: `rows` (whole-object head rows naming it), `sampled`, `present`, `missing` (bytes gone, row intact), `changed` (row changed mid-check), " +
					"`errors` (could not be asked), `missing_ratio` (missing over present + missing), `skipped` (why the backend was not fully sampled: an open breaker, consecutive errors)"),
				"unknown": freeObject("Head rows on a backend no driver is registered under, by name (`\"\"` = rows with none)"),
				"chunks": objectPtr("Chunks of the sampled chunked objects, at their one address", map[string]*Schema{
					"objects": integer(""), "chunks": integer(""), "present": integer(""),
					"legacy":  integer("Present only under the uploader's prefix (written before WP-R8-7): run the chunk move"),
					"missing": integer(""), "errors": integer(""), "unknown_backend": integer("The index row names a backend no driver has"),
				}),
				"checks":  integer("Calls made (whole rows + chunks) — `job_runs.rows_affected`"),
				"summary": str("The one-line note"),
			})),
		}, "job", "registered_backends", "unknown_backend_rows", "shared_stores", "last_outcome", "last_note"),
		"UnknownBackendRows": object("Rows of one table whose backend no driver is registered under", map[string]*Schema{
			"table":   strEnum("", "object_head_cache", "smart_demotions", "object_versions"),
			"backend": str("The recorded name; empty for rows with none (NULL)"),
			"rows":    integer(""),
		}, "table", "backend", "rows"),
		"SharedStore": object("One store that two or more registered backends write into", map[string]*Schema{
			"Store":    str("The store (endpoint + bucket, a directory, a fleet) or `<same driver value>` when one driver is registered twice"),
			"Backends": strArray("The registered names, sorted"),
		}, "Store", "Backends"),
		"RoutingTruthResolveNull": object("", map[string]*Schema{
			"dry_run":    boolean(""),
			"rows":       integer("NULL rows looked at in this call"),
			"resolved":   integer("Exactly one backend holds the bytes (written unless a dry run)"),
			"nowhere":    integer("No backend holds the bytes: the object is gone"),
			"ambiguous":  integer("More than one backend holds a blob at that key: left alone"),
			"errors":     integer("A backend could not be asked: nothing written for the row"),
			"remaining":  integer("NULL rows left after this call"),
			"by_backend": freeObject("Resolved rows per backend"),
			"details": arrayOf(objectPtr("", map[string]*Schema{
				"tenant_id": str(""), "bucket": str(""), "key": str(""),
				"holders": strArray("The backends that hold a blob at the key"),
				"verdict": strEnum("", "resolved", "nowhere", "ambiguous", "error"),
				"written": boolean("The row was updated (never in a dry run)"),
				"error":   str("Omitted unless the verdict is `error`"),
			})),
		}, "dry_run", "rows", "resolved", "nowhere", "ambiguous", "errors", "remaining", "by_backend", "details"),
		"JobStarted": object("A job run was started and continues on the server", map[string]*Schema{
			"job":        str("Job name"),
			"status":     strEnum("", "started"),
			"status_url": str("Where the result will be: `/api/v1/admin/jobs`"),
		}, "job", "status", "status_url"),
		"JobAlreadyRunning": object("Another run of the job holds its lock", map[string]*Schema{
			"error": strEnum("", "already_running"),
			"job":   str("Job name"),
		}, "error", "job"),
		"ChunkMoveResult": object("", map[string]*Schema{
			"object":          strEnum("", "chunk_move"),
			"backend":         str("The backend the run worked on"),
			"dry_run":         boolean("True: nothing was changed; moved, bytes_moved, legacy_deleted and orphans_deleted are what a run would do"),
			"rows":            integer("Index rows on the backend that were examined"),
			"at_address":      integer("Chunks whose blob is at the one address, verified, with no old copy left"),
			"moved":           integer("Chunks copied to the one address and verified"),
			"bytes_moved":     integer("Stored bytes copied"),
			"legacy_deleted":  integer("Old copies deleted"),
			"missing":         integer("Chunks with no copy that verifies anywhere — the objects that reference them cannot be read"),
			"failed":          integer("Chunks (or listings) that could not be decided or done; the next run retries"),
			"orphans":         integer("Blobs under a tenant's chunk container with no index row"),
			"orphans_deleted": integer("Orphans deleted"),
			"unrecognized":    integer("Names under a tenant's chunk container that are not a chunk key, or belong to a row on another backend; left alone"),
			"tenants_listed":  integer("Tenant prefixes listed"),
			"note":            str("Why nothing was done (a backend whose keys do not depend on the tenant)"),
			"errors":          arrayOf(str("One failure, at most 20")),
		}, "object", "backend", "dry_run", "rows", "at_address", "moved", "bytes_moved", "legacy_deleted", "missing", "failed", "orphans", "orphans_deleted", "unrecognized", "tenants_listed"),
		"SmartDemotionResult": object("", map[string]*Schema{
			"dry_run":              boolean(""),
			"tenants_scanned":      {Type: "integer"},
			"candidates":           {Type: "integer"},
			"demoted":              {Type: "integer"},
			"skipped":              {Type: "integer"},
			"bytes_demoted":        integer(""),
			"bytes_demoted_before": integer("Bytes the ledger shows as demoted in the 24 hours before this run; they count against the daily byte budget. Omitted when zero"),
			"hot_reclaimed":        {Type: "integer", Description: "Hot copies reclaimed after their grace period"},
			"promoted":             {Type: "integer", Description: "Objects promoted back on read"},
			"errors":               strArray("Per-object errors; omitted when none"),
			"tenants":              arrayOf(ref("TenantDemotionStats")),
		}, "dry_run", "tenants_scanned", "candidates", "demoted", "skipped", "bytes_demoted", "hot_reclaimed", "promoted"),
		"TenantDemotionStats": object("Per-tenant breakdown of a demotion run", map[string]*Schema{
			"tenant_id":        str(""),
			"quota_bytes":      integer(""),
			"pin_hot_bytes":    integer("The paid pin-hot add-on, added to the budget"),
			"hot_budget_bytes": integer(""),
			"hot_bytes_before": integer(""),
			"hot_bytes_after":  integer(""),
			"idle_candidates":  {Type: "integer"},
			"lru_candidates":   {Type: "integer"},
			"demoted":          {Type: "integer"},
		}, "tenant_id", "quota_bytes", "pin_hot_bytes", "hot_budget_bytes", "hot_bytes_before", "hot_bytes_after", "idle_candidates", "lru_candidates", "demoted"),
	}
}

func generateSecuritySchemes() map[string]SecurityScheme {
	return map[string]SecurityScheme{
		"BearerAuth": {
			Type:         "http",
			Scheme:       "bearer",
			BearerFormat: "JWT",
			Description: "`Authorization: Bearer <JWT>` — the token from `POST /auth/login` (HS256, 24 h). " +
				"Its claims carry the user, email and tenant every JSON endpoint acts on. Missing or invalid: 401 text/plain.",
		},
		"S3Signature": {
			Type:        "apiKey",
			Description: "AWS Signature Version 4 (`Authorization: AWS4-HMAC-SHA256 ...`) with the account's S3 credentials, a scoped API key, or an STS token; region `us-east-1`",
			Name:        "Authorization",
			In:          "header",
		},
	}
}

func generateParameters() map[string]Parameter {
	return map[string]Parameter{
		"IdempotencyKey": {
			Name: "Idempotency-Key",
			In:   "header",
			Description: "Optional, at most 256 characters. The first 2xx response for a (tenant, key) is stored for 24 h and replayed verbatim " +
				"(with `Idempotency-Replayed: true`) on every later request carrying the same key; a reuse with a different method or path is refused with 409 `idempotency_key_reuse`. " +
				"Longer keys are refused with 400 `idempotency_key_too_long`.",
			Schema: &Schema{Type: "string"},
		},
		"Limit": {
			Name:        "limit",
			In:          "query",
			Description: "Page size; default 20, values outside 1–100 fall back to the default",
			Schema:      intRange("", 1, 100),
		},
		"Cursor": {
			Name:        "cursor",
			In:          "query",
			Description: "Opaque keyset cursor from the previous page's `next_cursor`",
			Schema:      &Schema{Type: "string"},
		},
	}
}

func generateResponses() map[string]Response {
	rl := map[string]Header{
		"X-RateLimit-Limit":     {Ref: headerRef + "X-RateLimit-Limit"},
		"X-RateLimit-Remaining": {Ref: headerRef + "X-RateLimit-Remaining"},
		"X-RateLimit-Reset":     {Ref: headerRef + "X-RateLimit-Reset"},
		"Retry-After":           {Ref: headerRef + "Retry-After"},
	}
	return map[string]Response{
		"Unauthorized": textResp("Bearer token missing (`Unauthorized - missing token`) or rejected (`Invalid token: ...`)"),
		"Forbidden":    textResp("`admin access required` — the JWT's user does not have role `admin`"),
		"RateLimited": {
			Description: "`rate_limit_exceeded` — the tenant's budget (100/min, burst 10) is spent; `Retry-After` says when to try again",
			Headers:     rl,
			Content:     map[string]MediaType{"application/json": {Schema: ref("JSONError")}},
		},
		"AuthRateLimited": {
			Description: "Too many attempts from this IP (`Too many login attempts. Please try again in a minute.`)",
			Headers:     map[string]Header{"Retry-After": {Ref: headerRef + "Retry-After"}},
			Content:     map[string]MediaType{"text/plain": {Schema: &Schema{Type: "string"}}},
		},
	}
}

func generateHeaders() map[string]Header {
	return map[string]Header{
		"X-RateLimit-Limit":     {Description: "Requests per minute allowed for the tenant (100)", Schema: &Schema{Type: "integer"}},
		"X-RateLimit-Remaining": {Description: "Tokens left in the tenant's bucket (burst 10)", Schema: &Schema{Type: "integer"}},
		"X-RateLimit-Reset":     {Description: "Unix time when the bucket is full again (when admitted) or when the next request will be accepted (when refused), rounded up", Schema: &Schema{Type: "integer"}},
		"Retry-After":           {Description: "Seconds to wait before retrying", Schema: &Schema{Type: "integer"}},
		"Idempotency-Replayed":  {Description: "`true` when this response was replayed from the idempotency cache rather than executed", Schema: &Schema{Type: "string"}},
	}
}

// OpenAPIJSONHandler returns an HTTP handler that serves the OpenAPI spec as JSON
func OpenAPIJSONHandler() http.HandlerFunc {
	spec := GenerateOpenAPISpec()
	specJSON, _ := json.MarshalIndent(spec, "", "  ")

	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(specJSON)
	}
}

// SwaggerUIHandler returns an HTTP handler that serves Swagger UI
func SwaggerUIHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = fmt.Fprint(w, swaggerUIHTML)
	}
}

// swaggerUIHTML is the HTML for Swagger UI
const swaggerUIHTML = `<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <title>API Reference — Stored</title>
    <link rel="stylesheet" href="https://unpkg.com/swagger-ui-dist@5.10.3/swagger-ui.css">
    <style>
        html { box-sizing: border-box; overflow: -moz-scrollbars-vertical; overflow-y: scroll; }
        *, *:before, *:after { box-sizing: inherit; }
        body { margin: 0; background: #fafafa; }
    </style>
</head>
<body>
    <div id="swagger-ui"></div>
    <script src="https://unpkg.com/swagger-ui-dist@5.10.3/swagger-ui-bundle.js"></script>
    <script src="https://unpkg.com/swagger-ui-dist@5.10.3/swagger-ui-standalone-preset.js"></script>
    <script>
    window.onload = function() {
        window.ui = SwaggerUIBundle({
            url: "/openapi.json",
            dom_id: '#swagger-ui',
            deepLinking: true,
            presets: [
                SwaggerUIBundle.presets.apis,
                SwaggerUIStandalonePreset
            ],
            plugins: [
                SwaggerUIBundle.plugins.DownloadUrl
            ],
            layout: "StandaloneLayout"
        });
    };
    </script>
</body>
</html>`
