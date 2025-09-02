// test_quotaless_api.go
package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
)

func main() {
	// Test PUT
	data := bytes.NewReader([]byte("Stored.ge test data"))
	req, _ := http.NewRequest("PUT", "http://localhost:8000/mybucket/test.txt", data)
	req.Header.Set("X-Tenant-ID", "test-tenant")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		panic(err)
	}
	fmt.Printf("PUT Response: %d\n", resp.StatusCode)

	// Test GET
	req, _ = http.NewRequest("GET", "http://localhost:8000/mybucket/test.txt", nil)
	req.Header.Set("X-Tenant-ID", "test-tenant")

	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		panic(err)
	}
	body, _ := io.ReadAll(resp.Body)
	fmt.Printf("GET Response: %d, Body: %s\n", resp.StatusCode, body)
}
