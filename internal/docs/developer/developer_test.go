package developer

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestNewAPIReference(t *testing.T) {
	ref := NewAPIReference("storage-api", "Storage API").
		Description("S3-compatible storage API").
		BaseURL("https://api.stored.ge").
		Version("1.0.0").
		Build()

	if ref.ID != "storage-api" {
		t.Errorf("expected ID 'storage-api', got %s", ref.ID)
	}
	if ref.BaseURL != "https://api.stored.ge" {
		t.Error("BaseURL not set")
	}
	if ref.Version != "1.0.0" {
		t.Error("Version not set")
	}
}

func TestAPIReferenceBuilder_Endpoint(t *testing.T) {
	endpoint := NewEndpoint("GET", "/buckets").
		Summary("List buckets").
		Tags("Buckets").
		Response(200, "Success", "application/json", "[]Bucket", "[]").
		Build()

	ref := NewAPIReference("api", "API").
		Endpoint(endpoint).
		Build()

	if len(ref.Endpoints) != 1 {
		t.Error("Endpoint not added")
	}
}

func TestAPIReferenceBuilder_Model(t *testing.T) {
	model := NewModel("Bucket").
		Description("A storage bucket").
		Field("name", "string", "Bucket name", true).
		Field("created_at", "datetime", "Creation time", true).
		Build()

	ref := NewAPIReference("api", "API").
		Model(model).
		Build()

	if len(ref.Models) != 1 {
		t.Error("Model not added")
	}
}

func TestAPIReferenceBuilder_SDK(t *testing.T) {
	ref := NewAPIReference("api", "API").
		SDK(SDK{
			Name:       "stored-go",
			Language:   "go",
			InstallCmd: "go get github.com/stored/go-sdk",
		}).
		Build()

	if len(ref.SDKs) != 1 {
		t.Error("SDK not added")
	}
}

func TestAPIReferenceBuilder_Changelog(t *testing.T) {
	ref := NewAPIReference("api", "API").
		ChangelogEntry(ChangelogEntry{
			Version:  "1.1.0",
			Date:     time.Now(),
			Type:     ChangeAdded,
			Changes:  []string{"Added multipart upload"},
			Breaking: false,
		}).
		Build()

	if len(ref.Changelog) != 1 {
		t.Error("Changelog entry not added")
	}
}

func TestNewEndpoint(t *testing.T) {
	endpoint := NewEndpoint("PUT", "/buckets/{bucket}/objects/{key}").
		Summary("Upload object").
		Description("Upload an object to a bucket").
		Tags("Objects", "Upload").
		PathParam("bucket", "string", "Bucket name").
		PathParam("key", "string", "Object key").
		QueryParam("partNumber", "integer", "Part number", false).
		HeaderParam("Content-Type", "string", "Content type", true).
		Body("application/octet-stream", "binary", "Object data", "").
		Response(200, "Success", "application/json", "Object", `{"key":"test"}`).
		Response(404, "Not found", "application/json", "Error", "").
		Example("cURL", "bash", "curl -X PUT ...").
		Build()

	if endpoint.Method != "PUT" {
		t.Error("Method not set")
	}
	if len(endpoint.Parameters) != 4 {
		t.Errorf("expected 4 parameters, got %d", len(endpoint.Parameters))
	}
	if endpoint.RequestBody == nil {
		t.Error("RequestBody not set")
	}
	if len(endpoint.Responses) != 2 {
		t.Errorf("expected 2 responses, got %d", len(endpoint.Responses))
	}
	if len(endpoint.Examples) != 1 {
		t.Error("Example not added")
	}
}

func TestEndpointBuilder_Deprecated(t *testing.T) {
	endpoint := NewEndpoint("GET", "/old").
		Deprecated().
		Build()

	if !endpoint.Deprecated {
		t.Error("Deprecated not set")
	}
}

func TestNewModel(t *testing.T) {
	model := NewModel("Object").
		Description("A storage object").
		Field("key", "string", "Object key", true).
		Field("size", "integer", "Size in bytes", true).
		FieldWithConstraints("etag", "string", "Entity tag", "32 hex chars", true).
		Example(`{"key": "test.txt", "size": 1024}`).
		Build()

	if model.Name != "Object" {
		t.Error("Name not set")
	}
	if len(model.Fields) != 3 {
		t.Errorf("expected 3 fields, got %d", len(model.Fields))
	}
	if model.Fields[2].Constraints != "32 hex chars" {
		t.Error("Constraints not set")
	}
}

func TestDeveloperRenderer_RenderAPIReference(t *testing.T) {
	endpoint := NewEndpoint("GET", "/buckets").
		Summary("List buckets").
		Description("Returns all buckets").
		Tags("Buckets").
		QueryParam("limit", "integer", "Max results", false).
		Response(200, "Success", "application/json", "[]Bucket", `[{"name":"test"}]`).
		Example("cURL", "bash", "curl https://api.stored.ge/buckets").
		Build()

	model := NewModel("Bucket").
		Field("name", "string", "Bucket name", true).
		Build()

	ref := NewAPIReference("api", "Storage API").
		Description("S3-compatible storage").
		BaseURL("https://api.stored.ge").
		Version("1.0.0").
		Endpoint(endpoint).
		Model(model).
		SDK(SDK{
			Name:       "stored-go",
			Language:   "go",
			InstallCmd: "go get github.com/stored/go-sdk",
			QuickStart: "client := stored.New()",
		}).
		ChangelogEntry(ChangelogEntry{
			Version:  "1.0.0",
			Date:     time.Now(),
			Type:     ChangeAdded,
			Changes:  []string{"Initial release"},
			Breaking: false,
		}).
		Build()

	renderer := NewDeveloperRenderer()
	var buf bytes.Buffer

	err := renderer.RenderAPIReference(&buf, ref)
	if err != nil {
		t.Fatalf("RenderAPIReference failed: %v", err)
	}

	output := buf.String()

	if !strings.Contains(output, "# Storage API") {
		t.Error("Title not in output")
	}
	if !strings.Contains(output, "### GET /buckets") {
		t.Error("Endpoint not in output")
	}
	if !strings.Contains(output, "## Models") {
		t.Error("Models section not in output")
	}
	if !strings.Contains(output, "## SDKs") {
		t.Error("SDKs section not in output")
	}
	if !strings.Contains(output, "## Changelog") {
		t.Error("Changelog not in output")
	}

	t.Logf("API Reference:\n%s", output)
}

func TestDeveloperRenderer_DeprecatedEndpoint(t *testing.T) {
	endpoint := NewEndpoint("GET", "/old").
		Summary("Old endpoint").
		Deprecated().
		Response(200, "OK", "", "", "").
		Build()

	ref := NewAPIReference("api", "API").
		Endpoint(endpoint).
		Build()

	renderer := NewDeveloperRenderer()
	var buf bytes.Buffer

	err := renderer.RenderAPIReference(&buf, ref)
	if err != nil {
		t.Fatalf("failed: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "DEPRECATED") {
		t.Error("Deprecated flag not shown")
	}
}

func TestNewQuickStart(t *testing.T) {
	qs := NewQuickStart("Getting Started").
		Description("Learn how to use the API").
		Step("Install SDK", "Install the Go SDK", "go get github.com/stored/sdk", "bash").
		StepWithOutput("Create client", "Initialize the client", "client := stored.New()", "go", "Connected!").
		NextStep("Read the API reference").
		NextStep("Explore advanced features").
		Build()

	if qs.Title != "Getting Started" {
		t.Error("Title not set")
	}
	if len(qs.Steps) != 2 {
		t.Errorf("expected 2 steps, got %d", len(qs.Steps))
	}
	if qs.Steps[1].Output != "Connected!" {
		t.Error("Output not set")
	}
	if len(qs.NextSteps) != 2 {
		t.Error("NextSteps not set")
	}
}

func TestDeveloperRenderer_RenderQuickStart(t *testing.T) {
	qs := NewQuickStart("Quick Start").
		Description("Get started in 5 minutes").
		Step("Install", "Install the SDK", "pip install stored", "bash").
		StepWithOutput("Connect", "Create a client", "client = Stored()", "python", "OK").
		NextStep("Upload your first file").
		Build()

	renderer := NewDeveloperRenderer()
	var buf bytes.Buffer

	err := renderer.RenderQuickStart(&buf, qs)
	if err != nil {
		t.Fatalf("RenderQuickStart failed: %v", err)
	}

	output := buf.String()

	if !strings.Contains(output, "# Quick Start") {
		t.Error("Title not in output")
	}
	if !strings.Contains(output, "## Step 1: Install") {
		t.Error("Step 1 not in output")
	}
	if !strings.Contains(output, "**Output:**") {
		t.Error("Output not shown")
	}
	if !strings.Contains(output, "## Next Steps") {
		t.Error("Next steps not in output")
	}

	t.Logf("Quick Start:\n%s", output)
}

func TestSlugify(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"GET /buckets", "get--buckets"},
		{"PUT /buckets/{id}", "put--buckets-id"},
		{"Hello World", "hello-world"},
	}

	for _, tt := range tests {
		result := slugify(tt.input)
		if result != tt.expected {
			t.Errorf("slugify(%s) = %s, expected %s", tt.input, result, tt.expected)
		}
	}
}

func TestChangeType(t *testing.T) {
	types := []ChangeType{
		ChangeAdded,
		ChangeChanged,
		ChangeDeprecated,
		ChangeRemoved,
		ChangeFixed,
		ChangeSecurity,
	}

	for _, ct := range types {
		if ct == "" {
			t.Error("change type should not be empty")
		}
	}
}

func TestParameter(t *testing.T) {
	param := Parameter{
		Name:        "limit",
		In:          "query",
		Type:        "integer",
		Required:    false,
		Description: "Max results",
		Default:     "100",
		Example:     "50",
	}

	if param.Default != "100" {
		t.Error("Default not set")
	}
	if param.Example != "50" {
		t.Error("Example not set")
	}
}

func TestField(t *testing.T) {
	field := Field{
		Name:        "email",
		Type:        "string",
		Required:    true,
		Description: "User email",
		Default:     "",
		Constraints: "valid email format",
	}

	if field.Constraints != "valid email format" {
		t.Error("Constraints not set")
	}
}

func TestBreakingChangelog(t *testing.T) {
	entry := ChangelogEntry{
		Version:   "2.0.0",
		Date:      time.Now(),
		Type:      ChangeChanged,
		Changes:   []string{"Changed API response format"},
		Breaking:  true,
		Migration: "Update your response parsers",
	}

	if !entry.Breaking {
		t.Error("Breaking not set")
	}
	if entry.Migration == "" {
		t.Error("Migration not set")
	}
}
