//go:build ignore

// probe307 answers Step 0.1 of WP-EDGE-3 (docs/reviews/WP-EDGE-3.md): do
// S3 clients follow a 307 on PutObject and GetObject to ANOTHER host, and
// do they sign the redirected request for that host? Origin A (:8099)
// answers PUT and GET of an object with 307 Location: http://127.0.0.1:8100
// <path>?<query> (HEAD 404 before a PUT landed, else 200; anything else
// 200 empty); origin B (:8100) records every request — method, Host,
// whether it carries an Authorization, whether that Authorization is A's
// byte for byte (not re-signed) and whether its signature VERIFIES for B's
// host with the lab key — and answers PUT 200 with an ETag and GET with
// the body it stored. One line per request on stdout; the table is the
// operator's.
//
//	go run deploy/node/lab/probe307.go   (then point a client at :8099)
package main

import (
	"context"
	"crypto/md5" // #nosec G501 -- the lab's ETag
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

const ak, sk = "AKIALAB307PROBE00000", "lab307probesecretkey000000000000000000000"

var (
	mu       sync.Mutex
	stored   = map[string][]byte{}
	lastAuth = map[string]string{} // path → A's Authorization
)

func verifies(r *http.Request, body []byte) string {
	auth := r.Header.Get("Authorization")
	if auth == "" {
		if r.URL.Query().Get("X-Amz-Signature") != "" {
			return "presigned"
		}
		return "unsigned"
	}
	// Re-sign the request as received, with the same date and signed
	// headers, and compare the signatures.
	i := strings.Index(auth, "SignedHeaders=")
	if i < 0 {
		return "not-sigv4"
	}
	signed := strings.Split(strings.SplitN(auth[i+len("SignedHeaders="):], ",", 2)[0], ";")
	date, err := time.Parse("20060102T150405Z", r.Header.Get("X-Amz-Date"))
	if err != nil {
		return "no-date"
	}
	payload := r.Header.Get("X-Amz-Content-Sha256")
	if payload == "" {
		s := sha256.Sum256(body)
		payload = hex.EncodeToString(s[:])
	}
	cp, _ := http.NewRequest(r.Method, "http://"+r.Host+r.URL.RequestURI(), nil)
	for _, h := range signed {
		if h == "host" {
			continue
		}
		cp.Header[http.CanonicalHeaderKey(h)] = r.Header.Values(h)
	}
	cp.ContentLength = r.ContentLength
	region := "us-east-1"
	if j := strings.Index(auth, "Credential="); j >= 0 {
		parts := strings.Split(strings.SplitN(auth[j+11:], ",", 2)[0], "/")
		if len(parts) >= 3 {
			region = parts[2]
		}
	}
	signer := v4.NewSigner(func(o *v4.SignerOptions) { o.DisableURIPathEscaping = true })
	if err := signer.SignHTTP(context.Background(), aws.Credentials{AccessKeyID: ak, SecretAccessKey: sk}, cp, payload, "s3", region, date); err != nil {
		return "sign-error"
	}
	sig := func(a string) string { k := strings.Index(a, "Signature="); return a[k+10:] }
	if sig(cp.Header.Get("Authorization")) == sig(auth) {
		return "valid-for-B"
	}
	return "INVALID-for-B"
}

func main() {
	a := http.NewServeMux()
	a.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		key := r.URL.Path
		switch {
		case (r.Method == http.MethodPut || r.Method == http.MethodGet) && strings.Count(key, "/") >= 2 && len(r.URL.Query()) <= 1:
			mu.Lock()
			lastAuth[key] = r.Header.Get("Authorization")
			mu.Unlock()
			n, _ := io.Copy(io.Discard, r.Body)
			fmt.Printf("A %s %s host=%s cl=%d read=%d → 307\n", r.Method, r.URL.RequestURI(), r.Host, r.ContentLength, n)
			w.Header().Set("Location", "http://127.0.0.1:8100"+r.URL.RequestURI())
			w.WriteHeader(http.StatusTemporaryRedirect)
		case r.Method == http.MethodHead:
			mu.Lock()
			b, ok := stored[key]
			mu.Unlock()
			if !ok && strings.Contains(key, "-get") {
				b, ok = make([]byte, 1<<20), true // a key that "exists" for the GET case
			}
			fmt.Printf("A HEAD %s → %v\n", key, ok)
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			sum := md5.Sum(b) // #nosec G401
			w.Header().Set("ETag", `"`+hex.EncodeToString(sum[:])+`"`)
			w.Header().Set("Content-Length", fmt.Sprint(len(b)))
			w.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
		case r.Method == http.MethodGet && strings.Count(key, "/") == 1:
			// A listing (rclone lists before it reads): one key that exists.
			fmt.Printf("A LIST %s\n", r.URL.RequestURI())
			w.Header().Set("Content-Type", "application/xml")
			_, _ = fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><ListBucketResult><Name>lab</Name><Prefix>%s</Prefix><KeyCount>1</KeyCount><MaxKeys>1000</MaxKeys><IsTruncated>false</IsTruncated><Contents><Key>rc-get.bin</Key><Size>1048576</Size><ETag>"x"</ETag><LastModified>2026-10-27T00:00:00.000Z</LastModified><StorageClass>STANDARD</StorageClass></Contents></ListBucketResult>`, r.URL.Query().Get("prefix"))
		default:
			fmt.Printf("A %s %s (answered 200)\n", r.Method, r.URL.RequestURI())
		}
	})
	b := http.NewServeMux()
	b.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		same := r.Header.Get("Authorization") != "" && r.Header.Get("Authorization") == lastAuth[r.URL.Path]
		mu.Unlock()
		fmt.Printf("B %s %s host=%s body=%d auth_is_A's=%v signature=%s\n", r.Method, r.URL.RequestURI(), r.Host, len(body), same, verifies(r, body))
		switch r.Method {
		case http.MethodPut:
			if dec := r.Header.Get("X-Amz-Decoded-Content-Length"); dec != "" {
				fmt.Printf("B   aws-chunked body, decoded length %s\n", dec)
			}
			mu.Lock()
			stored[r.URL.Path] = body
			mu.Unlock()
			sum := md5.Sum(body) // #nosec G401
			w.Header().Set("ETag", `"`+hex.EncodeToString(sum[:])+`"`)
		case http.MethodGet:
			mu.Lock()
			data, ok := stored[r.URL.Path]
			mu.Unlock()
			if !ok {
				data = make([]byte, 1<<20)
			}
			w.Header().Set("Content-Length", fmt.Sprint(len(data)))
			_, _ = w.Write(data)
		}
	})
	go func() { _ = http.ListenAndServe("127.0.0.1:8100", b) }() // #nosec G114 -- a lab probe
	fmt.Println("probe307: A on :8099 (307 → :8100), B on :8100")
	_ = http.ListenAndServe("127.0.0.1:8099", a) // #nosec G114 -- a lab probe
}
