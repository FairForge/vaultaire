package api

import (
	"bytes"
	"encoding/xml"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/FairForge/vaultaire/internal/tenant"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// deleteObjects is a helper that POSTs a batch delete request.
func deleteObjects(t *testing.T, server *Server, tnt *tenant.Tenant, bucket string, body string) (int, string) {
	t.Helper()
	req := httptest.NewRequest("POST", "/"+bucket+"?delete", strings.NewReader(body))
	ctx := tenant.WithTenant(req.Context(), tnt)
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()
	server.handleS3Request(w, req)
	return w.Code, w.Body.String()
}

func TestDeleteObjects_MultipleExisting(t *testing.T) {
	server, tnt, tempDir, cleanup := setupCopyTestServer(t)
	defer cleanup()

	putObject(t, server, tnt, tempDir, "bucket1", "a.txt", "alpha")
	putObject(t, server, tnt, tempDir, "bucket1", "b.txt", "beta")
	putObject(t, server, tnt, tempDir, "bucket1", "c.txt", "gamma")

	body := `<?xml version="1.0" encoding="UTF-8"?>
<Delete>
  <Object><Key>a.txt</Key></Object>
  <Object><Key>b.txt</Key></Object>
  <Object><Key>c.txt</Key></Object>
</Delete>`

	code, respBody := deleteObjects(t, server, tnt, "bucket1", body)
	require.Equal(t, 200, code, "batch delete should succeed: %s", respBody)

	var result DeleteResult
	require.NoError(t, xml.Unmarshal([]byte(respBody), &result))
	assert.Len(t, result.Deleted, 3)
	assert.Empty(t, result.Errors)

	// Verify all three keys are gone.
	for _, k := range []string{"a.txt", "b.txt", "c.txt"} {
		code, _ := getObject(t, server, tnt, "bucket1", k)
		assert.Equal(t, 404, code, "%s should be deleted", k)
	}
}

func TestDeleteObjects_MixedExistingAndMissing(t *testing.T) {
	// S3 DELETE is idempotent — deleting a missing key is not an error.
	server, tnt, tempDir, cleanup := setupCopyTestServer(t)
	defer cleanup()

	putObject(t, server, tnt, tempDir, "bucket1", "exists.txt", "hi")

	body := `<?xml version="1.0" encoding="UTF-8"?>
<Delete>
  <Object><Key>exists.txt</Key></Object>
  <Object><Key>missing.txt</Key></Object>
</Delete>`

	code, respBody := deleteObjects(t, server, tnt, "bucket1", body)
	require.Equal(t, 200, code)

	var result DeleteResult
	require.NoError(t, xml.Unmarshal([]byte(respBody), &result))
	assert.Len(t, result.Deleted, 2, "both keys reported as deleted (idempotent)")
	assert.Empty(t, result.Errors)
}

func TestDeleteObjects_QuietMode(t *testing.T) {
	server, tnt, tempDir, cleanup := setupCopyTestServer(t)
	defer cleanup()

	putObject(t, server, tnt, tempDir, "bucket1", "q1.txt", "q1")
	putObject(t, server, tnt, tempDir, "bucket1", "q2.txt", "q2")

	body := `<?xml version="1.0" encoding="UTF-8"?>
<Delete>
  <Quiet>true</Quiet>
  <Object><Key>q1.txt</Key></Object>
  <Object><Key>q2.txt</Key></Object>
</Delete>`

	code, respBody := deleteObjects(t, server, tnt, "bucket1", body)
	require.Equal(t, 200, code)

	var result DeleteResult
	require.NoError(t, xml.Unmarshal([]byte(respBody), &result))
	assert.Empty(t, result.Deleted, "quiet mode: no Deleted entries")
	assert.Empty(t, result.Errors)
}

func TestDeleteObjects_MalformedXML(t *testing.T) {
	server, tnt, _, cleanup := setupCopyTestServer(t)
	defer cleanup()

	code, body := deleteObjects(t, server, tnt, "bucket1", "not xml at all")
	assert.Equal(t, 400, code)
	assert.Contains(t, body, "MalformedXML")
}

func TestDeleteObjects_EmptyObjectList(t *testing.T) {
	server, tnt, _, cleanup := setupCopyTestServer(t)
	defer cleanup()

	body := `<?xml version="1.0" encoding="UTF-8"?>
<Delete></Delete>`

	code, respBody := deleteObjects(t, server, tnt, "bucket1", body)
	assert.Equal(t, 400, code, "empty object list is invalid: %s", respBody)
	assert.Contains(t, respBody, "MalformedXML")
}

func TestDeleteObjects_TooManyKeys(t *testing.T) {
	server, tnt, _, cleanup := setupCopyTestServer(t)
	defer cleanup()

	var buf bytes.Buffer
	buf.WriteString(`<?xml version="1.0" encoding="UTF-8"?><Delete>`)
	for i := 0; i < 1001; i++ {
		buf.WriteString("<Object><Key>k</Key></Object>")
	}
	buf.WriteString(`</Delete>`)

	code, respBody := deleteObjects(t, server, tnt, "bucket1", buf.String())
	assert.Equal(t, 400, code, "1001 keys exceeds max: %s", respBody)
	assert.Contains(t, respBody, "MalformedXML")
}

func TestDeleteObjects_OperationDetection(t *testing.T) {
	// Verify the S3 parser classifies POST /{bucket}?delete as DeleteObjects.
	logger, _ := zap.NewDevelopment()
	parser := NewS3Parser(logger)

	req := httptest.NewRequest("POST", "/mybucket?delete", nil)
	s3Req, err := parser.ParseRequest(req)
	require.NoError(t, err)
	assert.Equal(t, "DeleteObjects", s3Req.Operation)
	assert.Equal(t, "mybucket", s3Req.Bucket)
}
