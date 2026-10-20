package api

// WP-SSEC-1 B.1 (Stage 1a Prompt 17 Part B): the SSE-C key on UploadPart and
// UploadPartCopy. An upload created with the customer's key keeps the key's
// MD5 on its row (096) — never the key; every part must carry a key of that
// MD5 and is sealed under it as it is staged (stageUploadPart), as one 0x04
// stream bound to the object and the part number (crypto.StreamIdentity.Part).
// Complete carries no key and needs none.

import (
	"database/sql"
	"errors"
	"net/http"

	"github.com/FairForge/vaultaire/internal/crypto"
	"go.uber.org/zap"
)

// partSeal is a staged part's SSE-C seal: the key (cleared after the part
// is staged) and the identity the part's stream is bound to.
type partSeal struct {
	key []byte
	id  crypto.StreamIdentity
}

// clear zeroes the key.
func (p *partSeal) clear() {
	if p != nil {
		clear(p.key)
		p.key = nil
	}
}

// AWS's answers to a key that does not fit the upload.
const (
	ssecPartKeyMissing   = "The multipart upload was initiated with a customer-provided key: every part must carry the same key (x-amz-server-side-encryption-customer-algorithm, -key and -key-MD5)."
	ssecPartKeyDifferent = "The provided encryption key does not match the key used to initiate the multipart upload."
	ssecPartKeyUnwanted  = "The encryption parameters are not applicable to this object: the multipart upload was not initiated with a customer-provided key."
)

// uploadPartSeal decides a part's seal from the upload row and the request:
// nil (a plain part) when the upload has no key and the request none; a
// seal when both carry the same key MD5. Anything else is 400
// InvalidArgument, written to w, nothing staged. A row that cannot be read
// is a 500.
func (s *Server) uploadPartSeal(w http.ResponseWriter, r *http.Request, tenantID, bucket, object, uploadID string, partNumber int) (*partSeal, bool) {
	if s.db == nil {
		if crypto.HasSSECHeaders(r) {
			WriteS3ErrorWithContext(w, ErrInvalidArgument, r.URL.Path, generateRequestID(), WithMessage(ssecPartKeyUnwanted))
			return nil, false
		}
		return nil, true
	}
	var uploadMD5 string
	err := s.db.QueryRowContext(r.Context(),
		`SELECT ssec_key_md5 FROM multipart_uploads WHERE upload_id = $1 AND tenant_id = $2`, uploadID, tenantID).Scan(&uploadMD5)
	if errors.Is(err, sql.ErrNoRows) {
		WriteS3Error(w, ErrNoSuchUpload, r.URL.Path, generateRequestID())
		return nil, false
	}
	if err != nil {
		s.logger.Error("upload part: upload row unreadable", zap.Error(err), zap.String("upload_id", uploadID))
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return nil, false
	}
	switch {
	case uploadMD5 == "" && !crypto.HasSSECHeaders(r):
		return nil, true
	case uploadMD5 == "":
		WriteS3ErrorWithContext(w, ErrInvalidArgument, r.URL.Path, generateRequestID(), WithMessage(ssecPartKeyUnwanted))
		return nil, false
	case !crypto.HasSSECHeaders(r):
		WriteS3ErrorWithContext(w, ErrInvalidArgument, r.URL.Path, generateRequestID(), WithMessage(ssecPartKeyMissing))
		return nil, false
	}
	key, kErr := crypto.ParseSSECHeaders(r)
	if kErr != nil {
		WriteS3ErrorWithContext(w, ErrInvalidArgument, r.URL.Path, generateRequestID(), WithMessage(kErr.Error()))
		return nil, false
	}
	if crypto.SSECKeyMD5(key) != uploadMD5 {
		clear(key)
		WriteS3ErrorWithContext(w, ErrInvalidArgument, r.URL.Path, generateRequestID(), WithMessage(ssecPartKeyDifferent))
		return nil, false
	}
	return &partSeal{key: key, id: crypto.StreamIdentity{Tenant: tenantID, Bucket: bucket, Key: object, Part: partNumber}}, true
}
