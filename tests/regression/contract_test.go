package regression

import (
	"encoding/xml"
	"net/http"
	"testing"
)

// TestS3XMLContract verifies S3 XML response format
func TestS3XMLContract(t *testing.T) {
	resp, err := http.Get("http://localhost:8000/bucket/")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	var result struct {
		XMLName xml.Name `xml:"ListBucketResult"`
		Name    string   `xml:"Name"`
	}

	if err := xml.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("Invalid S3 XML response: %v", err)
	}

	if result.XMLName.Local != "ListBucketResult" {
		t.Error("Response missing ListBucketResult root element")
	}
}

// TestS3ErrorContract verifies error responses match S3 format
func TestS3ErrorContract(t *testing.T) {
	resp, err := http.Get("http://localhost:8000/nonexistent/key")
	if err != nil {
		t.Skip("Server not running")
		return
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != 404 {
		t.Errorf("Expected 404, got %d", resp.StatusCode)
	}

	var errResp struct {
		Code    string `xml:"Code"`
		Message string `xml:"Message"`
	}

	_ = xml.NewDecoder(resp.Body).Decode(&errResp)
	if errResp.Code == "" {
		t.Error("S3 error response missing Code field")
	}
}
