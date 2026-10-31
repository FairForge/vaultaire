package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stored mcp (WP-AGENT-1): a local MCP client's stdio, one JSON-RPC message
// per line, bridged to mcp.stored.cloud with the agent grant's key; a
// notification's 202 writes nothing back.
func TestMCP_BridgesStdioToTheServerWithTheGrantsKey(t *testing.T) {
	var auths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auths = append(auths, r.Header.Get("Authorization"))
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), `"notifications/initialized"`) {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}` + "\n"))
	}))
	defer srv.Close()
	in := strings.NewReader(`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n" + `{"jsonrpc":"2.0","id":1,"method":"tools/list"}` + "\n")
	var out bytes.Buffer
	err := runMCP(context.Background(), []string{"--url", srv.URL}, in, &out, func(k string) string {
		if k == "STORED_TOKEN" {
			return "sk_test_abc"
		}
		return ""
	})
	require.NoError(t, err)
	assert.Equal(t, `{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}`+"\n", out.String(), "one line out per answer")
	assert.Equal(t, []string{"Bearer sk_test_abc", "Bearer sk_test_abc"}, auths)

	err = runMCP(context.Background(), []string{"--url", srv.URL}, strings.NewReader(""), &out, func(string) string { return "" })
	assert.ErrorContains(t, err, "STORED_TOKEN", "no grant, no bridge")
	err = runMCP(context.Background(), []string{"--url", srv.URL}, strings.NewReader(""), &out, func(string) string { return "eyJhbGciOi.jwt" })
	assert.ErrorContains(t, err, "agent grant", "a person's JWT is not an agent's key")
}
