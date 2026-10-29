//go:build docker

// Package docker holds the local image's integration test (WP-DX-1 PR C):
// `make docker`, then `go test -tags docker ./tests/docker/`. It brings the
// compose stack up under its own project name and port, reads the
// first-boot key pair from the log, and drives the S3 API with it.
package docker

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func compose(t *testing.T, project string, args ...string) string {
	t.Helper()
	root, err := filepath.Abs("../..")
	require.NoError(t, err)
	cmd := exec.Command("docker", append([]string{"compose", "-p", project, "-f", filepath.Join(root, "deploy/docker/compose.yml")}, args...)...)
	cmd.Env = append(os.Environ(), "STORED_PORT=18765")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	return string(out)
}

func signed(t *testing.T, method, url, ak, sk string, body []byte) *http.Response {
	t.Helper()
	r, err := http.NewRequest(method, url, bytes.NewReader(body))
	require.NoError(t, err)
	sum := sha256.Sum256(body)
	r.Header.Set("X-Amz-Content-Sha256", hex.EncodeToString(sum[:]))
	require.NoError(t, v4.NewSigner().SignHTTP(context.Background(), aws.Credentials{AccessKeyID: ak, SecretAccessKey: sk},
		r, hex.EncodeToString(sum[:]), "s3", "us-east-1", time.Now()))
	resp, err := http.DefaultClient.Do(r)
	require.NoError(t, err)
	return resp
}

func TestDockerImage_BootsAndAnswersTheOpenAPI(t *testing.T) {
	project := fmt.Sprintf("stored-test-%d", time.Now().UnixNano()%1_000_000)
	t.Cleanup(func() { compose(t, project, "down", "-v") })
	start := time.Now()
	compose(t, project, "up", "-d")
	base := "http://127.0.0.1:18765"
	require.Eventually(t, func() bool {
		r, err := http.Get(base + "/health")
		if err != nil {
			return false
		}
		_ = r.Body.Close()
		return r.StatusCode == http.StatusOK
	}, 90*time.Second, 500*time.Millisecond)
	t.Logf("healthy %s after up", time.Since(start).Round(time.Second))
	assert.Less(t, time.Since(start), time.Minute, "the image runs in under a minute")

	r, err := http.Get(base + "/openapi.json")
	require.NoError(t, err)
	var spec map[string]any
	require.NoError(t, json.NewDecoder(r.Body).Decode(&spec))
	_ = r.Body.Close()
	assert.Equal(t, "3.0.3", spec["openapi"])

	logs := compose(t, project, "logs", "vaultaire")
	ak := regexp.MustCompile(`AWS_ACCESS_KEY_ID=(\S+)`).FindStringSubmatch(logs)
	sk := regexp.MustCompile(`AWS_SECRET_ACCESS_KEY=(\S+)`).FindStringSubmatch(logs)
	require.Len(t, ak, 2, "the first-boot key pair is printed")
	require.Len(t, sk, 2)

	resp := signed(t, http.MethodPut, base+"/docker-test", ak[1], sk[1], nil)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body := make([]byte, 300_000)
	_, _ = rand.Read(body)
	resp = signed(t, http.MethodPut, base+"/docker-test/obj.bin", ak[1], sk[1], body)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp = signed(t, http.MethodGet, base+"/docker-test/obj.bin", ak[1], sk[1], nil)
	got, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, body, got, "the bytes come back")
}
