//go:build ignore

// sdk307 is aws-sdk-go-v2's PutObject and GetObject against probe307's
// origin A (Step 0.1 of WP-EDGE-3).
package main

import (
	"bytes"
	"context"
	"fmt"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

func main() {
	c := s3.New(s3.Options{Region: "us-east-1", BaseEndpoint: aws.String("http://127.0.0.1:8099"), UsePathStyle: true,
		Credentials: credentials.NewStaticCredentialsProvider("LAB307PROBE-ACCESS-KEY", "lab-307-probe-not-a-secret", "")})
	body := bytes.Repeat([]byte("x"), 1<<20)
	_, err := c.PutObject(context.Background(), &s3.PutObjectInput{Bucket: aws.String("lab"), Key: aws.String("sdk.bin"), Body: bytes.NewReader(body)})
	fmt.Println("sdk PutObject:", err)
	out, err := c.GetObject(context.Background(), &s3.GetObjectInput{Bucket: aws.String("lab"), Key: aws.String("sdk.bin")})
	if err == nil {
		b, _ := io.ReadAll(out.Body)
		fmt.Println("sdk GetObject: bytes", len(b))
	} else {
		fmt.Println("sdk GetObject:", err)
	}
}
