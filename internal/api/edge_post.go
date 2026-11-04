package api

// The edge's POST policy admit (WP-POST-1 PR B, Stage 1a Prompt 57): a
// browser's form upload to s3.stored.cloud. The host (the Worker) read the
// form up to its file part; this verifies the policy with the one
// verifier's fourth entrance (admitPostForm, the origin's own), builds the
// inner PUT of the file part the origin would serve, runs the prelude after
// authentication and the PUT's write decision (edgeAdmitWrite) — a whole,
// unsealed placement only — and records op 'put' with attrs.via 'post'.

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/FairForge/vaultaire/internal/auth"
)

// postPermissionOp is the permission a form upload needs: AWS's
// s3:PutObject (D-POST-4), as at the origin.
const postPermissionOp = auth.OpPutObject

// edgePostBodyCap is the largest request body the Worker receives
// (Cloudflare's 100 MB on Free and Pro: a larger body is answered 413 at
// the edge, before any Worker or origin sees it). A policy whose signed
// content-length-range max, plus the fields before the file and the
// closing delimiter, is past it is the origin's.
const edgePostBodyCap = 100_000_000

// edgePostTailMax bounds the closing delimiter ("\r\n--" + a boundary of at
// most 70 bytes + "--\r\n").
const edgePostTailMax = 80

func (s *Server) edgeAdmitPost(ctx context.Context, req edgeAdmitRequest) (edgeAdmitResponse, int) {
	if req.Method != http.MethodPost || req.Form == nil || req.Form.FileSize < 0 || req.Form.PreBytes < 0 {
		return passThrough("post_malformed")
	}
	bucket := postPolicyBucket(req.Path)
	if bucket == "" {
		return passThrough("no_bucket")
	}
	if !req.Has(edgeFeaturePostTail) {
		// A host that cannot prove the file part ends at the closing
		// delimiter before the backend has its last bytes would commit a
		// form the origin refuses (RP94-B-1/B-2): the origin's.
		return passThrough("post_tail_unproven")
	}
	outer := http.Header{}
	for k, v := range req.Headers {
		outer.Set(k, v)
	}
	fields := make(map[string]string, len(req.Form.Fields))
	for k, v := range req.Form.Fields {
		fields[strings.ToLower(k)] = v
	}
	errPath := &http.Request{Method: http.MethodPost, URL: &url.URL{Path: "/" + bucket}, Header: outer, RemoteAddr: req.ClientIP + ":0"}
	a, refused := s.admitPostForm(ctx, bucket, fields, req.Form.Filename, req.Form.FileSize, req.ClientIP, outer)
	if refused != nil {
		rec := newRelayRecorder()
		refused.write(rec, errPath)
		if refused.reason != "" {
			recordAuthFailureFor(refused.keyID, refused.reason, refused.keyKnown)
		}
		return refusalOf(rec)
	}
	for k := range a.header {
		if strings.HasPrefix(strings.ToLower(k), "x-amz-checksum-") {
			// The edge's link signs no checksum header (the backend would not
			// verify it): a form naming one is the origin's, which does
			// (RP94-4) — never admitted and left unchecked.
			return passThrough("post_checksum")
		}
	}
	_, max, bounded := a.policy.SizeRange()
	if !bounded {
		return passThrough("post_unbounded") // the signed size is what the edge admits by
	}
	if max+req.Form.PreBytes+edgePostTailMax > edgePostBodyCap {
		return passThrough("post_over_edge_cap")
	}
	if s.edge.minBytes > 0 && max < s.edge.minBytes {
		return passThrough(edgeReasonSmall)
	}

	// The inner PUT of the file part, as the origin builds it.
	headers := map[string]string{"host": req.Host, "content-length": strconv.FormatInt(req.Form.FileSize, 10)}
	if o := outer.Get("Origin"); o != "" {
		headers["origin"] = o // the commit's CORS headers (RP94-3)
	}
	for k, v := range a.header {
		headers[strings.ToLower(k)] = v[0]
	}
	put := req
	put.Op, put.Method, put.Path, put.Query, put.Headers, put.Form = edgeOpPut, http.MethodPut, "/"+bucket+"/"+escapeKeyPath(a.key), "", headers, nil
	d, sr, err := edgeDescriptor(put)
	if err != nil {
		return passThrough("bad_descriptor")
	}
	p, resp, status, decided := s.edgeAdmitAuthed(ctx, postPermissionOp, d, sr.WithContext(ctx), newRelayRecorder(), a.tenantID, a.scope)
	if decided {
		return resp, status
	}
	p.post = &edgePost{admission: a, base: "https://" + req.Host}
	return s.edgeAdmitWrite(put, p)
}
