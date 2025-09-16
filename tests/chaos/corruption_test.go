package chaos

import (
	"bytes"
	"crypto/md5"
	"fmt"
	"io"
	"net/http"
	"testing"
)

// TestDataCorruption verifies data integrity
func TestDataCorruption(t *testing.T) {
	// First check if server is running
	healthResp, err := http.Get("http://localhost:8000/health")
	if err != nil {
		t.Skip("Server not running")
		return
	}
	_ = healthResp.Body.Close()

	client := &http.Client{}

	// Test if server requires authentication by trying a simple request
	testReq, _ := http.NewRequest("PUT", "http://localhost:8000/test-auth", bytes.NewReader([]byte("test")))
	testResp, err := client.Do(testReq)
	if err != nil {
		t.Skip("Cannot connect to server")
		return
	}
	defer func() { _ = testResp.Body.Close() }()

	if testResp.StatusCode == 403 || testResp.StatusCode == 401 {
		t.Skip("Server requires authentication - skipping chaos tests")
		return
	}

	// Test data with known checksums
	testCases := []struct {
		name string
		data []byte
	}{
		{"zeros", make([]byte, 1024)},
		{"pattern", bytes.Repeat([]byte("ABCD"), 256)},
		{"random", []byte("This is test data with known checksum")},
	}

	for _, tc := range testCases {
		// Calculate original checksum
		originalSum := md5.Sum(tc.data)

		// Upload data
		url := fmt.Sprintf("http://localhost:8000/corruption-test/%s.bin", tc.name)
		req, _ := http.NewRequest("PUT", url, bytes.NewReader(tc.data))
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("Upload failed: %v", err)
		}
		_ = resp.Body.Close()

		// Download and verify
		req, _ = http.NewRequest("GET", url, nil)
		resp, err = client.Do(req)
		if err != nil {
			t.Fatalf("Download failed: %v", err)
		}

		downloaded, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()

		downloadSum := md5.Sum(downloaded)

		if originalSum != downloadSum {
			t.Errorf("Data corruption detected for %s!", tc.name)
			t.Errorf("Original MD5: %x", originalSum)
			t.Errorf("Downloaded MD5: %x", downloadSum)
		} else {
			t.Logf("✓ Data integrity verified for %s", tc.name)
		}
	}
}

// TestBitFlipSimulation simulates random bit flips
func TestBitFlipSimulation(t *testing.T) {
	// First check if server is running
	healthResp, err := http.Get("http://localhost:8000/health")
	if err != nil {
		t.Skip("Server not running")
		return
	}
	_ = healthResp.Body.Close()

	client := &http.Client{}

	// Test if server requires authentication
	testReq, _ := http.NewRequest("PUT", "http://localhost:8000/test-auth", bytes.NewReader([]byte("test")))
	testResp, err := client.Do(testReq)
	if err != nil {
		t.Skip("Cannot connect to server")
		return
	}
	defer func() { _ = testResp.Body.Close() }()

	if testResp.StatusCode == 403 || testResp.StatusCode == 401 {
		t.Skip("Server requires authentication - skipping chaos tests")
		return
	}

	// Upload known data
	original := []byte("The quick brown fox jumps over the lazy dog")
	url := "http://localhost:8000/corruption-test/bitflip.txt"

	// Upload 10 times, verify each time
	corruptions := 0
	for i := 0; i < 10; i++ {
		// Upload
		req, _ := http.NewRequest("PUT", url, bytes.NewReader(original))
		resp, err := client.Do(req)
		if err != nil {
			continue
		}
		_ = resp.Body.Close()

		// Download
		req, _ = http.NewRequest("GET", url, nil)
		resp, err = client.Do(req)
		if err != nil {
			continue
		}
		downloaded, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()

		// Check for corruption
		if !bytes.Equal(original, downloaded) {
			corruptions++
			t.Logf("Corruption detected in iteration %d", i)
		}
	}

	if corruptions > 0 {
		t.Errorf("Data corruptions detected: %d/10", corruptions)
	} else {
		t.Log("✓ No data corruption in 10 iterations")
	}
}
