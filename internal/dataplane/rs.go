package dataplane

// An RS write at a Go host (WP-EDGE-4 PR B, edge_rs; plan 40.10 slice 3):
// the body read once, encoded stripe by stripe (internal/ec) into K data and
// M parity shards, each streamed to its presigned PUT on its leg through an
// io.Pipe — K+M PUTs at once, one stripe in memory — while the body's MD5
// (the object's ETag) is computed. Every shard must land: a shard that did
// not is the commit's edge_error (the client's 503; the control plane queues
// what landed). Never in the Worker (the SPEED bench: 26–29 MB/s there).

import (
	"context"
	"crypto/md5" // #nosec G401 -- the S3 ETag of the body
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/ec"
)

func (h *Host) rsWrite(ctx context.Context, req *http.Request, body io.Reader, size int64, desc AdmitRequest, res *Result) {
	admit := res.Admit
	g := admit.RS
	started := time.Now()
	commit := CommitRequest{AdmitID: admit.AdmitID, Outcome: OutcomeEdgeError}
	l := ec.Layout{K: g.K, M: g.M, Stripe: g.Stripe, Size: size}
	if size < 0 || len(g.Shards) != g.K+g.M || l.ShardBytes() != g.ShardBytes {
		h.log().Warn("data plane: an RS grant this host cannot follow — edge_error", zap.String("admit_id", admit.AdmitID))
		res.Relay = h.commitWrite(ctx, commit, desc)
		return
	}
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	writers := make([]io.Writer, len(g.Shards))
	pipes := make([]*io.PipeWriter, len(g.Shards))
	etags := make([]string, len(g.Shards))
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	for i, sh := range g.Shards {
		pr, pw := io.Pipe()
		writers[sh.Index], pipes[sh.Index] = pw, pw
		wg.Add(1)
		go func(sh ShardLink, pr *io.PipeReader) {
			defer wg.Done()
			etag, err := h.putShard(cctx, sh, g.ShardBytes, pr)
			_ = pr.CloseWithError(err)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = fmt.Errorf("shard %d on %s: %w", sh.Index, sh.Leg, err)
				}
				cancel()
				return
			}
			etags[sh.Index] = etag
		}(sh, pr)
		_ = i
	}
	sum := md5.New() // #nosec G401
	encErr := ec.Encode(l, io.TeeReader(body, sum), writers)
	for _, pw := range pipes {
		_ = pw.CloseWithError(encErr)
	}
	wg.Wait()
	if encErr == nil {
		encErr = firstErr
	}
	commit.ShardETags = etags
	if encErr == nil {
		commit.Outcome = OutcomeOK
		commit.BodyMD5 = hex.EncodeToString(sum.Sum(nil))
	} else {
		commit.BackendCode = truncate(encErr.Error(), 120)
		h.log().Warn("data plane: RS write failed — edge_error", zap.Error(encErr), zap.String("admit_id", admit.AdmitID))
	}
	commit.DurationMS = time.Since(started).Milliseconds()
	res.Relay = h.commitWrite(ctx, commit, desc)
	if commit.Outcome != OutcomeOK {
		res.Relay = slowDown(req.URL.Path, desc.RequestID, "the data plane could not store your upload. Retry.")
		return
	}
	h.log().Info("data plane: rs", zap.String("admit_id", admit.AdmitID), zap.Int64("size", size),
		zap.Int("k", g.K), zap.Int("m", g.M), zap.Int64("ms", commit.DurationMS))
}

// putShard streams one shard to its link and answers the backend's ETag.
func (h *Host) putShard(ctx context.Context, sh ShardLink, n int64, body io.Reader) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, sh.URL, body)
	if err != nil {
		return "", err
	}
	for k, v := range sh.SendHeaders {
		if k != "content-length" && k != "host" {
			req.Header.Set(k, v)
		}
	}
	req.ContentLength = n
	resp, err := h.client().Do(req)
	if err != nil {
		return "", err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("answered %d", resp.StatusCode)
	}
	return resp.Header.Get("ETag"), nil
}
