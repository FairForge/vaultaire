package api

import (
	"github.com/FairForge/vaultaire/internal/auth"
	"github.com/FairForge/vaultaire/internal/crypto"
)

// The S3 secrets' one opener (Stage 1a Prompt 42, plan 43.7): every
// credential lookup and every signer on a tenant's behalf gets the tenant key
// store from here, so a wrapped secret is opened the same way on the header
// path, the presigned path, whoami, the presigned-URL route and the export
// link (TestNewAuth_EveryCallSiteHasTheOpener pins the call sites).

// credentialKeys is the tenant key store as the auth package's seam — a nil
// interface (never a typed nil) without a master key. It carries the
// credential_plaintext_drop flag (PR C): the writers drop the plaintext
// while it is on.
func (s *Server) credentialKeys() auth.CredentialKeys {
	if s.tenantKeys == nil {
		return nil
	}
	return credentialKeyring{TenantKeys: s.tenantKeys, drop: s.credentialPlaintextDropped}
}

// credentialKeyring is the key store plus the drop switch
// (auth.PlaintextDropper).
type credentialKeyring struct {
	*crypto.TenantKeys
	drop func() bool
}

func (k credentialKeyring) DropPlaintext() bool { return k.drop != nil && k.drop() }

// s3Auth builds the S3 verifier: THE place an auth.Auth is made in this
// package.
func (s *Server) s3Auth() *auth.Auth {
	a := auth.NewAuth(s.db, s.logger, s.credentialKeys())
	a.MissingSignedHeader = s.missingSignedHeaderHint
	return a
}
