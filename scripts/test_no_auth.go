// cmd/test/main.go
package main

import (
    "fmt"
    "net/http"
)

func main() {
    // Simple S3-like server without auth for testing
    http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
        if r.Method == "GET" && r.URL.Path == "/" {
            // ListBuckets
            w.Header().Set("Content-Type", "application/xml")
            fmt.Fprintf(w, `<ListAllMyBucketsResult><Buckets></Buckets></ListAllMyBucketsResult>`)
            return
        }
        // Add more handlers as needed
    })
    
    fmt.Println("Test server running on :8001 (no auth)")
    http.ListenAndServe(":8001", nil)
}
