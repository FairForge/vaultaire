package main

// stored mcp (WP-AGENT-1, Stage 1a Prompt 55): a local MCP client (an IDE, a
// desktop app) speaks stdio; this bridges it to Stored's MCP server
// (https://mcp.stored.cloud/mcp, or --url / STORED_MCP_URL) with the agent
// grant's key in STORED_TOKEN — sk_live_… for the account, or the grant's
// sk_test_… to rehearse on the sandbox. One JSON-RPC message per line in,
// one answer per line out (the MCP stdio transport); a notification is
// answered by nothing. The tools are the server's: there is one
// implementation of them (edge/mcp/).

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const defaultMCPURL = "https://mcp.stored.cloud/mcp"

func runMCP(ctx context.Context, args []string, in io.Reader, out io.Writer, getenv func(string) string) error {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	url := fs.String("url", "", "the MCP server (default $STORED_MCP_URL, else "+defaultMCPURL+")")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *url == "" {
		*url = getenv("STORED_MCP_URL")
	}
	if *url == "" {
		*url = defaultMCPURL
	}
	token := strings.TrimSpace(getenv("STORED_TOKEN"))
	if token == "" {
		return errors.New("set STORED_TOKEN to the agent grant's key (sk_live_… or sk_test_…)")
	}
	if !strings.HasPrefix(token, "sk_live_") && !strings.HasPrefix(token, "sk_test_") {
		return errors.New("STORED_TOKEN must be an agent grant's key (sk_live_… or sk_test_…), never a person's sign-in token")
	}
	client := &http.Client{Timeout: 5 * time.Minute}
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, *url, bytes.NewReader(line))
		if err != nil {
			return fmt.Errorf("mcp: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("Authorization", "Bearer "+token)
		res, err := client.Do(req)
		if err != nil {
			return fmt.Errorf("mcp: %w", err)
		}
		body, err := io.ReadAll(io.LimitReader(res.Body, 32<<20))
		_ = res.Body.Close()
		if err != nil {
			return fmt.Errorf("mcp: read the answer: %w", err)
		}
		switch {
		case res.StatusCode == http.StatusAccepted:
			continue
		case res.StatusCode == http.StatusUnauthorized:
			return errors.New("mcp: the server refused the key (revoked, expired, or agents are off for the account): connect again from the dashboard")
		case res.StatusCode >= 300:
			return fmt.Errorf("mcp: the server answered %d: %s", res.StatusCode, strings.TrimSpace(string(body)))
		}
		if _, err := out.Write(append(bytes.TrimSpace(body), '\n')); err != nil {
			return err
		}
	}
	return sc.Err()
}
