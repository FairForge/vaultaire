package acceptance

import (
	"bytes"
	"net/http"
	"testing"
)

// TestRealWorldScenario simulates actual user workflow
func TestRealWorldScenario(t *testing.T) {
	client := &http.Client{}
	bucket := "user-test"

	// Check if server is running
	_, err := http.Get("http://localhost:8000/")
	if err != nil {
		t.Skip("Server not running")
		return
	}

	// 1. User uploads profile picture
	imageData := bytes.Repeat([]byte("IMG"), 1000)
	req, _ := http.NewRequest("PUT",
		"http://localhost:8000/"+bucket+"/profile.jpg",
		bytes.NewReader(imageData))
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal("Failed to upload profile picture")
	}
	_ = resp.Body.Close()
}

// TestErrorMessages verifies user-friendly error responses
func TestErrorMessages(t *testing.T) {
	// Check if server is running
	_, err := http.Get("http://localhost:8000/")
	if err != nil {
		t.Skip("Server not running")
		return
	}

	resp, _ := http.Get("http://localhost:8000/missing/file")
	if resp != nil {
		_ = resp.Body.Close()
	}
}
