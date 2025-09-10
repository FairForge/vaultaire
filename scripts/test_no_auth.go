package main

import (
	"fmt"
	"log"
	"net/http"
)

func main() {
	// Mock S3 server for testing without auth
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Printf("Request: %s %s\n", r.Method, r.URL.Path)

		if r.Method == "GET" && r.URL.Path == "/" {
			w.Header().Set("Content-Type", "application/xml")
			_, err := fmt.Fprintf(w, `<ListAllMyBucketsResult><Buckets></Buckets></ListAllMyBucketsResult>`)
			if err != nil {
				log.Printf("Error writing response: %v", err)
			}
			return
		}

		http.NotFound(w, r)
	})

	fmt.Println("Mock S3 server on :8001")
	if err := http.ListenAndServe(":8001", nil); err != nil {
		log.Fatal(err)
	}
}
