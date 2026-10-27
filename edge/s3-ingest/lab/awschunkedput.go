//go:build ignore

// awschunkedput sends aws-chunked PutObject / UploadPart requests the way
// aws-cli ≥ 2.23 does over TLS (STREAMING-UNSIGNED-PAYLOAD-TRAILER, 1 MiB
// chunks, a CRC64NVME trailer, Transfer-Encoding: chunked — the SPEED
// bench's capture), signed with the lab tenant's key, through the Worker as
// an HTTP proxy. The lab's origin is plain HTTP, and botocore sends a
// trailer only over https (httpchecksum.py: "we'll only use trailers over
// TLS"), so aws-cli itself sends a header checksum in the lab; this is its
// TLS shape. -corrupt: the trailer names another checksum (one byte
// flipped), the body's length right. -multipart: the whole upload in
// -part-size parts, -concurrency at once (aws-cli's 8 MiB × 10), the
// Create and the Complete signed to the origin (lab/codec-proof.sh,
// WP-EDGE-2C.md).
//
//	go run lab/awschunkedput.go -endpoint http://127.0.0.1:8099 -proxy http://127.0.0.1:8787 \
//	  -bucket codeclab -key x.bin -file body.dat [-upload-id U -part 1 | -multipart] [-corrupt]
//
// The key pair is read from AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/xml"
	"flag"
	"fmt"
	"hash/crc64"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

// frame is data as aws-chunked with a CRC64NVME trailer (corrupt: one bit flipped).
func frame(data []byte, corrupt bool) *bytes.Buffer {
	sum := make([]byte, 8)
	binary.BigEndian.PutUint64(sum, crc64.Checksum(data, crc64.MakeTable(0x9a6c9329ac4bc9b5)))
	if corrupt {
		sum[7] ^= 1
	}
	var b bytes.Buffer
	for o := 0; o < len(data); o += 1 << 20 {
		n := min(1<<20, len(data)-o)
		fmt.Fprintf(&b, "%x\r\n", n)
		b.Write(data[o : o+n])
		b.WriteString("\r\n")
	}
	fmt.Fprintf(&b, "0\r\nx-amz-checksum-crc64nvme:%s\r\n\r\n", base64.StdEncoding.EncodeToString(sum))
	return &b
}

func creds() aws.Credentials {
	return aws.Credentials{AccessKeyID: os.Getenv("AWS_ACCESS_KEY_ID"), SecretAccessKey: os.Getenv("AWS_SECRET_ACCESS_KEY")}
}

// send signs and sends one aws-chunked PUT of data to u through client;
// returns the status, the S3 error code, the ETag.
func send(client *http.Client, u string, data []byte, corrupt bool) (int, string, string) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPut, u, nil)
	if err != nil {
		panic(err)
	}
	req.Header.Set("Content-Encoding", "aws-chunked")
	req.Header.Set("X-Amz-Content-Sha256", "STREAMING-UNSIGNED-PAYLOAD-TRAILER")
	req.Header.Set("X-Amz-Decoded-Content-Length", strconv.Itoa(len(data)))
	req.Header.Set("X-Amz-Trailer", "x-amz-checksum-crc64nvme")
	req.Header.Set("X-Amz-Sdk-Checksum-Algorithm", "CRC64NVME")
	if err := v4.NewSigner().SignHTTP(context.Background(), creds(), req, "STREAMING-UNSIGNED-PAYLOAD-TRAILER", "s3", "us-east-1", time.Now()); err != nil {
		panic(err)
	}
	req.Body = io.NopCloser(frame(data, corrupt))
	req.ContentLength = -1 // chunked, as aws-cli
	resp, err := client.Do(req)
	if err != nil {
		return 0, err.Error(), ""
	}
	defer func() { _ = resp.Body.Close() }()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, codeOf(out), resp.Header.Get("ETag")
}

func codeOf(out []byte) string {
	if i := bytes.Index(out, []byte("<Code>")); i >= 0 {
		if j := bytes.Index(out[i:], []byte("</Code>")); j > 0 {
			return string(out[i+6 : i+j])
		}
	}
	return ""
}

// plain signs and sends a small request straight to the origin (Create, Complete).
func plain(method, u string, body []byte) []byte {
	req, err := http.NewRequestWithContext(context.Background(), method, u, bytes.NewReader(body))
	if err != nil {
		panic(err)
	}
	h := sha256.Sum256(body)
	ph := hex.EncodeToString(h[:])
	req.Header.Set("X-Amz-Content-Sha256", ph)
	if err := v4.NewSigner().SignHTTP(context.Background(), creds(), req, ph, "s3", "us-east-1", time.Now()); err != nil {
		panic(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		panic(err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		panic(fmt.Sprintf("%s %s: %d %s", method, u, resp.StatusCode, out))
	}
	return out
}

func main() {
	endpoint := flag.String("endpoint", "", "the origin the client signs for")
	proxy := flag.String("proxy", "", "the Worker (wrangler dev) as an HTTP proxy")
	bucket := flag.String("bucket", "", "bucket")
	key := flag.String("key", "", "key")
	file := flag.String("file", "", "body file")
	uploadID := flag.String("upload-id", "", "an UploadPart's upload id")
	part := flag.Int("part", 0, "an UploadPart's part number")
	corrupt := flag.Bool("corrupt", false, "a trailer that is not the body's")
	multipart := flag.Bool("multipart", false, "the whole upload: Create, the parts through the Worker, Complete")
	partSize := flag.Int("part-size", 8<<20, "-multipart: the part size")
	concurrency := flag.Int("concurrency", 10, "-multipart: parts at once")
	flag.Parse()
	data, err := os.ReadFile(*file)
	if err != nil {
		panic(err)
	}
	pu, err := url.Parse(*proxy)
	if err != nil {
		panic(err)
	}
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(pu), MaxConnsPerHost: 64}}
	segs := strings.Split(*key, "/")
	for i := range segs {
		segs[i] = url.PathEscape(segs[i])
	}
	obj := *endpoint + "/" + *bucket + "/" + strings.Join(segs, "/")
	if !*multipart {
		u := obj
		if *uploadID != "" {
			u += "?partNumber=" + strconv.Itoa(*part) + "&uploadId=" + url.QueryEscape(*uploadID)
		}
		st, code, etag := send(client, u, data, *corrupt)
		fmt.Printf("%d %s %s\n", st, code, etag)
		return
	}
	// The whole upload, as aws-cli's s3 cp makes it.
	var created struct {
		UploadID string `xml:"UploadId"`
	}
	if err := xml.Unmarshal(plain(http.MethodPost, obj+"?uploads", nil), &created); err != nil {
		panic(err)
	}
	n := (len(data) + *partSize - 1) / *partSize
	etags := make([]string, n)
	statuses := map[string]int{}
	var mu sync.Mutex
	sem := make(chan struct{}, *concurrency)
	var wg sync.WaitGroup
	start := time.Now()
	for k := 0; k < n; k++ {
		wg.Add(1)
		sem <- struct{}{}
		go func(k int) {
			defer wg.Done()
			defer func() { <-sem }()
			body := data[k**partSize : min(len(data), (k+1)**partSize)]
			st, code, etag := send(client, obj+"?partNumber="+strconv.Itoa(k+1)+"&uploadId="+url.QueryEscape(created.UploadID), body, *corrupt)
			mu.Lock()
			statuses[fmt.Sprintf("%d %s", st, code)]++
			etags[k] = etag
			mu.Unlock()
		}(k)
	}
	wg.Wait()
	elapsed := time.Since(start)
	var x bytes.Buffer
	x.WriteString("<CompleteMultipartUpload>")
	for k, e := range etags {
		fmt.Fprintf(&x, "<Part><PartNumber>%d</PartNumber><ETag>%s</ETag></Part>", k+1, e)
	}
	x.WriteString("</CompleteMultipartUpload>")
	var done struct {
		ETag string `xml:"ETag"`
	}
	if err := xml.Unmarshal(plain(http.MethodPost, obj+"?uploadId="+url.QueryEscape(created.UploadID), x.Bytes()), &done); err != nil {
		panic(err)
	}
	fmt.Printf("parts %d %v in %.1f s; complete %s\n", n, statuses, elapsed.Seconds(), done.ETag)
}
