package main

// -raw: the multipart calls one by one, timed, with the driver's transport
// (HTTP/1.1, tuned pool) but no uploader — what Vail itself costs per phase.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/FairForge/vaultaire/internal/drivers"
)

type rawResult struct {
	PartSize    int64     `json:"part_size"`
	Parts       int       `json:"parts"`
	Concurrency int       `json:"concurrency"`
	CreateSec   float64   `json:"create_s"`
	PartsSec    float64   `json:"parts_s"`
	PartsMBps   float64   `json:"parts_mb_s"`
	PartSecs    []float64 `json:"part_s"`
	CompleteSec float64   `json:"complete_s"`
	TotalMBps   float64   `json:"total_mb_s"`
	Error       string    `json:"error,omitempty"`
}

func runRaw(ctx context.Context, ak, sk, bucket, endpoint, prefix string, partSize int64, parts, conc int) rawResult {
	if endpoint == "" {
		endpoint = "https://la1.geyserdata.com"
	}
	c := s3.New(s3.Options{
		Region:                     "us-west-2",
		Credentials:                credentials.NewStaticCredentialsProvider(ak, sk, ""),
		BaseEndpoint:               aws.String(endpoint),
		UsePathStyle:               true,
		HTTPClient:                 drivers.TunedHTTPClient(drivers.WithHTTP1Only()),
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
	})
	r := rawResult{PartSize: partSize, Parts: parts, Concurrency: conc, PartSecs: make([]float64, parts)}
	key := fmt.Sprintf("%s/raw-%d-%d-%d.bin", prefix, partSize, parts, time.Now().UnixNano())
	body := make([]byte, partSize)
	_, _ = rand.NewChaCha8([32]byte{1}).Read(body)

	t0 := time.Now()
	cr, err := c.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: &bucket, Key: &key})
	r.CreateSec = time.Since(t0).Seconds()
	if err != nil {
		r.Error = "create: " + err.Error()
		return r
	}
	done := make([]types.CompletedPart, parts)
	sem := make(chan struct{}, conc)
	var wg sync.WaitGroup
	var mu sync.Mutex
	t1 := time.Now()
	for i := 0; i < parts; i++ {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			s := time.Now()
			out, err := c.UploadPart(ctx, &s3.UploadPartInput{Bucket: &bucket, Key: &key, UploadId: cr.UploadId,
				PartNumber: aws.Int32(int32(i + 1)), Body: bytes.NewReader(body), ContentLength: aws.Int64(partSize)})
			mu.Lock()
			defer mu.Unlock()
			r.PartSecs[i] = time.Since(s).Seconds()
			if err != nil && r.Error == "" {
				r.Error = fmt.Sprintf("part %d: %v", i+1, err)
				return
			}
			if err == nil {
				done[i] = types.CompletedPart{PartNumber: aws.Int32(int32(i + 1)), ETag: out.ETag}
			}
		}(i)
	}
	wg.Wait()
	r.PartsSec = time.Since(t1).Seconds()
	r.PartsMBps = float64(partSize*int64(parts)) / 1e6 / r.PartsSec
	if r.Error != "" {
		_, _ = c.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{Bucket: &bucket, Key: &key, UploadId: cr.UploadId})
		return r
	}
	t2 := time.Now()
	_, err = c.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{Bucket: &bucket, Key: &key, UploadId: cr.UploadId,
		MultipartUpload: &types.CompletedMultipartUpload{Parts: done}})
	r.CompleteSec = time.Since(t2).Seconds()
	r.TotalMBps = float64(partSize*int64(parts)) / 1e6 / time.Since(t0).Seconds()
	if err != nil {
		r.Error = "complete: " + err.Error()
	}
	_, _ = c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &bucket, Key: &key})
	return r
}

func rawMain(ak, sk, bucket, endpoint, prefix string, partSize int64, parts, conc []int) {
	enc := json.NewEncoder(os.Stdout)
	for _, p := range parts {
		for _, cc := range conc {
			_ = enc.Encode(runRaw(context.Background(), ak, sk, bucket, endpoint, prefix, partSize, p, cc))
		}
	}
}
