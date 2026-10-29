package api

import (
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"io/fs"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Structure tests of plan 43.7 (Stage 1a Prompt 42 PR B): the S3 secrets
// have ONE opener, handed out in one place, and every writer of a secret
// column is known and wraps.

// productGoFiles parses every non-test Go file under internal/ and cmd/.
func productGoFiles(t *testing.T) (*token.FileSet, map[string]*ast.File) {
	t.Helper()
	fset := token.NewFileSet()
	files := map[string]*ast.File{}
	for _, root := range []string{"../../internal", "../../cmd"} {
		require.NoError(t, filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if err != nil {
				return err
			}
			files[strings.TrimPrefix(filepath.ToSlash(path), "../../")] = f
			return nil
		}))
	}
	return fset, files
}

// enclosingFunc names the function a position sits in ("" at file level).
func enclosingFunc(f *ast.File, pos token.Pos) string {
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || pos < fd.Pos() || pos > fd.End() {
			continue
		}
		if fd.Recv != nil && len(fd.Recv.List) == 1 {
			typ := fd.Recv.List[0].Type
			if st, ok := typ.(*ast.StarExpr); ok {
				typ = st.X
			}
			if id, ok := typ.(*ast.Ident); ok {
				return id.Name + "." + fd.Name.Name
			}
		}
		return fd.Name.Name
	}
	return ""
}

func exprString(fset *token.FileSet, e ast.Expr) string {
	var b strings.Builder
	_ = printer.Fprint(&b, fset, e)
	return b.String()
}

// TestNewAuth_EveryCallSiteHasTheOpener: an auth.Auth is built in ONE place
// (Server.s3Auth, with the server's key store); every signer on a tenant's
// behalf (PrimaryPair) and every STS mint gets the opener from there; the
// auth service's writers get the key store where the master key is loaded.
// A new call site that forgets the opener would read wrapped rows as 500s —
// or, with a plaintext fallback, defeat 43.7 — so it fails here first.
func TestNewAuth_EveryCallSiteHasTheOpener(t *testing.T) {
	fset, files := productGoFiles(t)
	var newAuth, primaryPair, sts, exportSvc, setKeys []string
	for path, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			at := path + ":" + enclosingFunc(f, call.Pos())
			if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "NewAccountExportService" {
				exportSvc = append(exportSvc, at+" signer="+exprString(fset, call.Args[4]))
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			args := make([]string, len(call.Args))
			for i, a := range call.Args {
				args[i] = exprString(fset, a)
			}
			switch {
			case sel.Sel.Name == "NewAuth" && exprString(fset, sel.X) == "auth":
				newAuth = append(newAuth, at+"("+strings.Join(args, ", ")+")")
			case sel.Sel.Name == "PrimaryPair":
				primaryPair = append(primaryPair, at+" on "+exprString(fset, sel.X))
			case sel.Sel.Name == "GenerateSTSToken":
				sts = append(sts, at+" keys="+args[2])
			case sel.Sel.Name == "SetCredentialKeys":
				setKeys = append(setKeys, at+"("+strings.Join(args, ", ")+")")
			}
			return true
		})
	}
	sort.Strings(setKeys)
	assert.Equal(t, []string{"internal/api/credential_auth.go:Server.s3Auth(s.db, s.logger, s.credentialKeys())"}, newAuth,
		"auth.NewAuth is called in one place, with the key store")
	assert.ElementsMatch(t, []string{
		"internal/api/presigned.go:Server.handleGetPresignedURL on s.s3Auth()",
		"internal/api/account_export.go:AccountExportService.DownloadURL on s.signer",
	}, primaryPair)
	assert.Equal(t, []string{"internal/api/server.go:NewServer signer=s.s3Auth()"}, exportSvc)
	assert.ElementsMatch(t, []string{
		"internal/api/sts_routes.go:Server.handleSTSCreateToken keys=s.credentialKeys()",
		"internal/api/platform_keys.go:Server.handlePlatformCreateSTS keys=s.credentialKeys()", // a platform's child (WP-PLAT-1)
	}, sts)
	var dropWired bool
	ast.Inspect(files["internal/api/server.go"], func(n ast.Node) bool {
		if as, ok := n.(*ast.AssignStmt); ok && len(as.Lhs) == 1 &&
			exprString(fset, as.Lhs[0]) == "s.keyReencrypt.dropPlaintext" && exprString(fset, as.Rhs[0]) == "s.credentialPlaintextDropped" {
			dropWired = true
		}
		return true
	})
	assert.True(t, dropWired, "NewServer hands the key_reencrypt job the credential_plaintext_drop flag (PR C)")
	assert.Equal(t, []string{
		"internal/api/server.go:NewServer(s.credentialKeys())",
		"internal/api/server.go:Server.SetAuthService(s.credentialKeys())",
	}, setKeys, "the auth service's writers get the key store where it is loaded")
}

// credentialWriters is every product SQL statement that writes a secret
// column of api_keys / sts_tokens / tenants, by file:function — with how it
// is held to plan 43.7. A new writer fails the test until it is listed here
// (and wraps).
var credentialWriters = map[string]string{
	"internal/auth/auth.go:AuthService.persistNewAccount INSERT INTO api_keys":                "wraps (sealSecret, the registration's primary)",
	"internal/auth/auth.go:AuthService.persistNewAccount INSERT INTO tenants":                 "the mirror's plaintext (nulled by credential_plaintext_drop, dropped with D-12)",
	"internal/auth/apikey.go:AuthService.persistAPIKey INSERT INTO api_keys":                  "wraps (sealSecret: new keys and a rotation's successor)",
	"internal/auth/apikey.go:AuthService.rotateRows UPDATE tenants":                           "the mirror's plaintext (as above)",
	"internal/auth/sts.go:GenerateSTSToken INSERT INTO sts_tokens":                            "wraps (sealSecret)",
	"internal/database/postgres.go:Postgres.CreateTenant INSERT INTO tenants":                 "no product caller (its own test only)",
	"internal/api/credential_backfill.go:KeyReencryptRunner.wrapOne UPDATE api_keys":          "the backfill: writes the wrapped form only",
	"internal/api/credential_backfill.go:KeyReencryptRunner.wrapOne UPDATE sts_tokens":        "the backfill: writes the wrapped form only",
	"internal/api/credential_backfill.go:KeyReencryptRunner.dropOne UPDATE api_keys":          "PR C: nulls the plaintext after the wrap opened back to it",
	"internal/api/credential_backfill.go:KeyReencryptRunner.dropOne UPDATE sts_tokens":        "PR C: nulls the plaintext after the wrap opened back to it",
	"internal/api/credential_backfill.go:KeyReencryptRunner.dropPlaintextPass UPDATE tenants": "PR C: nulls the mirror once the live primary has no plaintext",
}

func TestCredentialWriters_AllWrapWhenKeysExist(t *testing.T) {
	_, files := productGoFiles(t)
	insertRe := regexp.MustCompile(`(?is)\bINSERT\s+INTO\s+(api_keys|sts_tokens|tenants)\b`)
	updateRe := regexp.MustCompile(`(?is)\bUPDATE\s+(api_keys|sts_tokens|tenants)\s+(?:\w+\s+)?SET\b`)
	secretRe := regexp.MustCompile(`(?i)\bsecret_(key|wrapped)\b`)
	found := map[string]string{} // writer → statement
	for path, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			sql, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			at := path + ":" + enclosingFunc(f, lit.Pos())
			if m := insertRe.FindStringSubmatch(sql); m != nil && secretRe.MatchString(sql) {
				found[at+" INSERT INTO "+strings.ToLower(m[1])] = sql
			}
			if m := updateRe.FindStringSubmatch(sql); m != nil && secretRe.MatchString(sql) {
				found[at+" UPDATE "+strings.ToLower(m[1])] = sql
			}
			return true
		})
	}
	var got, want []string
	for k := range found {
		got = append(got, k)
	}
	for k := range credentialWriters {
		want = append(want, k)
	}
	sort.Strings(got)
	sort.Strings(want)
	assert.Equal(t, want, got, "every writer of a secret column is listed in credentialWriters")
	for at, sql := range found {
		if strings.Contains(at, "INSERT INTO api_keys") || strings.Contains(at, "INSERT INTO sts_tokens") {
			assert.Contains(t, sql, "secret_wrapped", "%s writes a secret without its wrapped form", at)
			assert.Contains(t, sql, "secret_key_version", "%s writes a wrap without its version", at)
		}
	}
}
