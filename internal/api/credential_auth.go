package api

import (
	"github.com/FairForge/vaultaire/internal/auth"
)

// The S3 secrets' one opener (Stage 1a Prompt 42, plan 43.7): every
// credential lookup and every signer on a tenant's behalf gets the tenant key
// store from here, so a wrapped secret is opened the same way on the header
// path, the presigned path, whoami, the presigned-URL route and the export
// link (TestNewAuth_EveryCallSiteHasTheOpener pins the call sites).

// credentialKeys is the tenant key store as the auth package's seam — a nil
// interface (never a typed nil) without a master key.
func (s *Server) credentialKeys() auth.CredentialKeys {
	if s.tenantKeys == nil {
		return nil
	}
	return s.tenantKeys
}

// s3Auth builds the S3 verifier: THE place an auth.Auth is made in this
// package.
func (s *Server) s3Auth() *auth.Auth {
	a := auth.NewAuth(s.db, s.logger, s.credentialKeys())
	a.MissingSignedHeader = s.missingSignedHeaderHint
	return a
}
