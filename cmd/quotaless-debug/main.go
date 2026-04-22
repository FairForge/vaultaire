package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"os"
	"strings"
	"time"

	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/credentials"
)

func main() {
	ak := os.Getenv("QUOTALESS_ACCESS_KEY")
	sk := "gatewaysecret"
	ep := "https://srv1.quotaless.cloud:8000"

	creds, _ := credentials.NewStaticCredentialsProvider(ak, sk, "").Retrieve(context.Background())
	signer := v4.NewSigner()

	fmt.Println("=== Test 1: HEAD /data/ ===")
	req, _ := http.NewRequest("HEAD", ep+"/data/", nil)
	_ = signer.SignHTTP(context.Background(), creds, req, "UNSIGNED-PAYLOAD", "s3", "us-east-1", time.Now())
	dump, _ := httputil.DumpRequestOut(req, false)
	fmt.Print(string(dump))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
	} else {
		fmt.Printf("→ %d\n", resp.StatusCode)
		_ = resp.Body.Close()
	}

	fmt.Println("\n=== Test 2: PUT with UNSIGNED-PAYLOAD + x-amz-content-sha256 header ===")
	body := "hello quotaless test"
	req2, _ := http.NewRequest("PUT", ep+"/data/personal-files/sigv4-test.txt", strings.NewReader(body))
	req2.ContentLength = int64(len(body))
	req2.Header.Set("x-amz-content-sha256", "UNSIGNED-PAYLOAD")
	_ = signer.SignHTTP(context.Background(), creds, req2, "UNSIGNED-PAYLOAD", "s3", "us-east-1", time.Now())
	dump2, _ := httputil.DumpRequestOut(req2, false)
	fmt.Print(string(dump2))
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
	} else {
		b, _ := io.ReadAll(resp2.Body)
		fmt.Printf("→ %d: %s\n", resp2.StatusCode, string(b))
		_ = resp2.Body.Close()
	}

	fmt.Println("\n=== Test 3: PUT with computed payload hash ===")
	body3 := "hello quotaless computed hash"
	h := sha256.Sum256([]byte(body3))
	payloadHash := hex.EncodeToString(h[:])
	req3, _ := http.NewRequest("PUT", ep+"/data/personal-files/sigv4-test2.txt", strings.NewReader(body3))
	req3.ContentLength = int64(len(body3))
	_ = signer.SignHTTP(context.Background(), creds, req3, payloadHash, "s3", "us-east-1", time.Now())
	dump3, _ := httputil.DumpRequestOut(req3, false)
	fmt.Print(string(dump3))
	resp3, err := http.DefaultClient.Do(req3)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
	} else {
		b, _ := io.ReadAll(resp3.Body)
		fmt.Printf("→ %d: %s\n", resp3.StatusCode, string(b))
		_ = resp3.Body.Close()
	}
}
