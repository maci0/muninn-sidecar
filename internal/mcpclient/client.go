// Package mcpclient provides a shared JSON-RPC 2.0 client for MuninnDB MCP calls.
// Used by both the store (async delivery) and inject (recall/enrichment) packages.
package mcpclient

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
)

// maxResponseSize caps MCP response body reads to prevent a misbehaving
// MuninnDB server from exhausting memory.
const maxResponseSize = 10 << 20 // 10 MiB

// requestID is a process-wide atomic counter for unique JSON-RPC request IDs.
var requestID atomic.Int64

// Client sends JSON-RPC 2.0 tools/call requests to a MuninnDB MCP endpoint.
type Client struct {
	url        string
	token      string
	httpClient *http.Client
}

// New creates a Client with the given endpoint, auth token, and HTTP timeout.
// Trailing slashes are stripped from url to prevent double-slash issues.
// TLS 1.3 is enforced for HTTPS connections to match the proxy's upstream policy.
func New(rawURL, token string, timeout time.Duration) *Client {
	return &Client{
		url:   strings.TrimRight(rawURL, "/"),
		token: token,
		httpClient: &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS13},
				MaxIdleConnsPerHost: 10,
				IdleConnTimeout:     90 * time.Second,
			},
		},
	}
}

// HealthCheckAt pings the MuninnDB health endpoint at mcpURL. Returns nil if
// reachable, or an error describing the failure. Uses a short (3s) timeout so
// it does not delay startup noticeably. Can be called without creating a Client.
func HealthCheckAt(mcpURL, token string) error {
	return New(mcpURL, token, 3*time.Second).HealthCheck()
}

// HealthCheck pings the MuninnDB health endpoint for this client's configured
// URL. The health path is derived by appending /health to the MCP path
// (e.g. http://127.0.0.1:8750/mcp → http://127.0.0.1:8750/mcp/health).
func (c *Client) HealthCheck() error {
	healthURL, err := healthURLFrom(c.url)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(context.Background(), "GET", healthURL, nil)
	if err != nil {
		return fmt.Errorf("failed to create health request: %w", err)
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("unreachable at %s: %w", healthURL, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode >= 400 {
		return fmt.Errorf("unhealthy (HTTP %d) at %s", resp.StatusCode, healthURL)
	}
	return nil
}

// healthURLFrom derives the health endpoint URL by appending /health to the MCP
// path (e.g. http://127.0.0.1:8750/mcp → http://127.0.0.1:8750/mcp/health).
func healthURLFrom(mcpURL string) (string, error) {
	u, err := url.Parse(mcpURL)
	if err != nil {
		return "", fmt.Errorf("invalid MCP URL: %w", err)
	}
	u.Path = strings.TrimSuffix(u.Path, "/") + "/health"
	return u.String(), nil
}

// ServerError is a retryable error for 5xx responses.
type ServerError struct{ Status int }

func (e *ServerError) Error() string { return fmt.Sprintf("server error: HTTP %d", e.Status) }

// ClientError is a non-retryable error for 4xx responses.
type ClientError struct{ Status int }

func (e *ClientError) Error() string { return fmt.Sprintf("client error: HTTP %d", e.Status) }

// RPCError is a non-retryable error for JSON-RPC protocol-level failures
// (HTTP 200 with {"error": {...}} in the response body).
type RPCError struct {
	Code    int
	Message string
}

func (e *RPCError) Error() string { return fmt.Sprintf("rpc error %d: %s", e.Code, e.Message) }

// Call sends a JSON-RPC 2.0 tools/call request and returns the raw response body.
// Returns a *ServerError for 5xx, *ClientError for 4xx, or *RPCError for a
// JSON-RPC protocol-level error (HTTP 200 with an "error" field in the body).
func (c *Client) Call(ctx context.Context, toolName string, args map[string]any) ([]byte, error) {
	payload := map[string]any{
		"jsonrpc": "2.0",
		"method":  "tools/call",
		"params": map[string]any{
			"name":      toolName,
			"arguments": args,
		},
		"id": requestID.Add(1),
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", c.url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	// Read one extra byte beyond the limit to detect oversized responses.
	// Without this, io.LimitReader silently truncates, producing invalid JSON
	// that fails downstream with a misleading parse error.
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize+1))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if int64(len(respBody)) > maxResponseSize {
		return nil, fmt.Errorf("MCP response exceeds %d-byte limit", maxResponseSize)
	}

	if resp.StatusCode >= 500 {
		return nil, &ServerError{Status: resp.StatusCode}
	}
	if resp.StatusCode >= 400 {
		return nil, &ClientError{Status: resp.StatusCode}
	}

	// Check for JSON-RPC protocol-level errors (HTTP 200 with error body).
	// A misbehaving or overloaded server can return HTTP 200 with
	// {"jsonrpc":"2.0","error":{"message":"..."},"id":1} — this must not
	// be treated as success or the memory is silently lost.
	var rpcResp struct {
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(respBody, &rpcResp) == nil && rpcResp.Error != nil {
		return nil, &RPCError{Code: rpcResp.Error.Code, Message: rpcResp.Error.Message}
	}

	return respBody, nil
}
