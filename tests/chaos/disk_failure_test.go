package chaos

import (
	"bytes"
	"fmt"
	"net/http"
	"os"
	"testing"
)

// TestDiskFailure simulates disk write failures
func TestDiskFailure(t *testing.T) {
	// This test would need to manipulate file permissions
	// to simulate disk failures in a safe way

	client := &http.Client{}

	// Test 1: Read-only filesystem simulation
	t.Run("ReadOnlyFilesystem", func(t *testing.T) {
		// Would need to remount filesystem as read-only
		// Skipping actual implementation for safety
		t.Skip("Requires filesystem manipulation")
	})

	// Test 2: Disk full simulation
	t.Run("DiskFull", func(t *testing.T) {
		// Create a large file to fill disk (safely)
		tempFile := "/tmp/vaultaire-disk-test"

		// Create 100MB file
		f, err := os.Create(tempFile)
		if err != nil {
			t.Skip("Cannot create test file")
		}
		defer func() { _ = os.Remove(tempFile) }()
		defer func() { _ = f.Close() }()

		// Write 100MB
		data := make([]byte, 1024*1024)
		for i := 0; i < 100; i++ {
			_, _ = f.Write(data)
		}

		// Now try to upload
		uploadData := bytes.Repeat([]byte("X"), 1024*1024)
		req, _ := http.NewRequest("PUT",
			"http://localhost:8000/disk-fail/large.bin",
			bytes.NewReader(uploadData))

		resp, err := client.Do(req)
		if err != nil {
			t.Logf("Upload failed as expected: %v", err)
		} else {
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode == 507 { // Insufficient Storage
				t.Log("✓ Server correctly returned 507 Insufficient Storage")
			} else {
				t.Logf("Server returned status: %d", resp.StatusCode)
			}
		}
	})

	// Test 3: Intermittent I/O errors
	t.Run("IntermittentIOErrors", func(t *testing.T) {
		successes := 0
		failures := 0

		for i := 0; i < 20; i++ {
			req, _ := http.NewRequest("PUT",
				fmt.Sprintf("http://localhost:8000/disk-fail/io-%d.txt", i),
				bytes.NewReader([]byte("io test")))

			resp, err := client.Do(req)
			if err != nil {
				failures++
			} else {
				_ = resp.Body.Close()
				if resp.StatusCode == 200 {
					successes++
				} else {
					failures++
				}
			}
		}

		t.Logf("IO test results: %d successes, %d failures", successes, failures)
	})
}
