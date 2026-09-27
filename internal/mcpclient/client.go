// Package mcpclient provides a shared JSON-RPC 2.0 client for MuninnDB MCP calls.
// Used by both the store (async delivery) and inject (recall/enrichment) packages.
package mcpclient

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/maci0/muninn-sidecar/internal/redact"
)

// maxResponseSize caps MCP response body reads to prevent a misbehaving
// MuninnDB server from exhausting memory.
const maxResponseSize = 10 << 20 // 10 MiB

// requestID is a process-wide atomic counter for unique JSON-RPC request IDs.
var requestID atomic.Int64

// DedupKey derives the stable, content-addressed dedup_key for one memory
// write to MuninnDB. Re-deriving it for the same (vault, concept, content)
// yields the same key on every attempt, every call site and every process, so
// a retried or replayed write — a batch re-seeded by a rerun, an exchange
// delivered twice after a restart — collapses onto the stored memory instead
// of adding a second one. The concept alone is not enough: a re-asked question
// with a different answer is a new memory. SHA-256 keeps collisions out of
// reach for content-length memory.
func DedupKey(vault, concept, content string) string {
	sum := sha256.Sum256([]byte(vault + "\x00" + concept + "\x00" + content))
	return hex.EncodeToString(sum[:])
}

// NextRequestID reserves a fresh JSON-RPC request ID. A caller that will retry
// an operation must reserve the ID once and pass it to every attempt via
// CallWithID, so a retry is recognisable as the same logical operation rather
// than as a new one. Request IDs minted here are unique for the process.
func NextRequestID() int64 { return requestID.Add(1) }

// Client sends JSON-RPC 2.0 tools/call requests to a MuninnDB MCP endpoint.
type Client struct {
	url        string
	token      string
	httpClient *http.Client
}

// New creates a Client with the given endpoint, auth token, and HTTP timeout.
// Trailing slashes are stripped from url to prevent double-slash issues.
// TLS 1.3 is enforced for HTTPS connections to match the proxy's upstream policy.
//
// The client owns a private Transport, so a caller that builds one per call
// (rather than holding one for the session) must Close it: a fully-read
// response returns its connection to that transport's idle pool, where it sits
// with its read/write goroutines until the process exits. Long-lived holders
// (the store's writer, the injector's enricher) keep theirs for the session and
// close it at shutdown.
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

// Close releases the client's idle connections and their goroutines. It is safe
// to call on a client that never made a request, and safe to call more than
// once. In-flight requests are not interrupted; they finish and their
// connections close with them.
func (c *Client) Close() {
	if t, ok := c.httpClient.Transport.(*http.Transport); ok {
		t.CloseIdleConnections()
	}
}

// HealthCheckAt pings the MuninnDB health endpoint at mcpURL. Returns nil if
// reachable, or an error describing the failure. Uses a short (3s) timeout so
// it does not delay startup noticeably. Can be called without creating a Client.
//
// The one-shot client is closed before returning: the health body is drained,
// so without that the connection would go back to an idle pool nobody can ever
// reach and linger for the life of the process.
func HealthCheckAt(mcpURL, token string) error {
	c := New(mcpURL, token, 3*time.Second)
	defer c.Close()
	return c.HealthCheck()
}

// HealthCheck pings the MuninnDB health endpoint for this client's configured
// URL. The health path is derived by appending /health to the MCP path
// (e.g. http://127.0.0.1:8750/mcp → http://127.0.0.1:8750/mcp/health).
func (c *Client) HealthCheck() error {
	healthURL, err := healthURLFrom(c.url)
	if err != nil {
		return err
	}
	// The request uses the real URL (a password in the userinfo is part of how
	// the endpoint authenticates); only the error text names the redacted form,
	// since these errors reach logs and the operator's terminal.
	shown := redact.URL(healthURL)

	req, err := http.NewRequestWithContext(context.Background(), "GET", healthURL, nil)
	if err != nil {
		return fmt.Errorf("failed to create health request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("unreachable at %s: %w", shown, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode >= 400 {
		return fmt.Errorf("unhealthy (HTTP %d) at %s", resp.StatusCode, shown)
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

// maxErrorRunes caps the server-supplied text carried in an RPCError. A
// rejection message is a sentence; anything past this is a server echoing a
// whole payload, which would otherwise be pasted into a log line verbatim.
const maxErrorRunes = 300

// scrubServerText prepares server-supplied error text for use in an error
// value. The text crosses a trust boundary: it is authored by the memory server
// about the request it just refused, and a rejection that quotes the offending
// memory ("content too long: <the content>") hands back exactly the captured
// conversation the write path redacts everywhere else. Every call site treats
// the error as safe to log or print, so the scrub belongs here rather than at
// the log lines, where one new call site would undo it. Redaction runs before
// the cap, so an over-long rejection cannot escape the scrub by being over-long;
// the cap may then cut a marker redaction inserted, which costs the line some
// context, never secrecy.
//
// Redact first and cap second: a server message long enough to be capped is
// usually long because it quotes the offending memory, so truncating before the
// scrub would return the first maxErrorRunes of exactly the text that must not
// be logged.
func scrubServerText(s string) string {
	s = redact.Secrets(strings.TrimSpace(s))
	r := []rune(s)
	if len(r) > maxErrorRunes {
		return string(r[:maxErrorRunes]) + "…"
	}
	return s
}

// RPCError is a non-retryable error for JSON-RPC protocol-level failures
// (HTTP 200 with {"error": {...}} in the response body).
type RPCError struct {
	Code    int
	Message string
}

func (e *RPCError) Error() string { return fmt.Sprintf("rpc error %d: %s", e.Code, e.Message) }

// Call sends a JSON-RPC 2.0 tools/call request and returns the raw response body.
// Returns a *ClientError for 4xx (a permanent rejection) or an *RPCError for a
// JSON-RPC protocol-level error (HTTP 200 with an "error" field in the body).
//
// Call mints a fresh request ID per invocation, so calling it in a retry loop
// gives every attempt a distinct ID. A retried write should reserve one ID via
// NextRequestID and use CallWithID for every attempt instead.
//
// 5xx is returned as a plain wrapped error, which callers treat as retryable.
func (c *Client) Call(ctx context.Context, toolName string, args map[string]any) ([]byte, error) {
	return c.CallWithID(ctx, NextRequestID(), toolName, args)
}

// CallWithID is Call with a caller-supplied JSON-RPC request ID. Retries of one
// logical operation must reuse the same ID: it is the only field of the request
// that stays constant across attempts, so a server that collapses repeated
// writes can recognise the retry as the same operation rather than a new one.
func (c *Client) CallWithID(ctx context.Context, id int64, toolName string, args map[string]any) ([]byte, error) {
	payload := map[string]any{
		"jsonrpc": "2.0",
		"method":  "tools/call",
		"params": map[string]any{
			"name":      toolName,
			"arguments": args,
		},
		"id": id,
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
	// Explicitly negotiate a JSON response. An MCP-over-HTTP server capable of
	// both JSON and SSE may otherwise default to a text/event-stream reply, which
	// this client (a one-shot JSON-RPC caller) does not parse as a stream.
	req.Header.Set("Accept", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: request failed: %w", toolName, redact.URL(c.url), err)
	}
	defer resp.Body.Close()

	// Read one extra byte beyond the limit to detect oversized responses.
	// Without this, io.LimitReader silently truncates, producing invalid JSON
	// that fails downstream with a misleading parse error.
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize+1))
	if err != nil {
		return nil, fmt.Errorf("%s %s: read response: %w", toolName, redact.URL(c.url), err)
	}
	if int64(len(respBody)) > maxResponseSize {
		return nil, fmt.Errorf("%s %s: MCP response exceeds %d-byte limit", toolName, redact.URL(c.url), maxResponseSize)
	}

	return classifyResponse(resp.StatusCode, respBody)
}

// classifyResponse turns an HTTP status and body into either the raw success
// body or the error the caller must see. A 5xx is transient, a 4xx or a
// JSON-RPC error object is permanent: retrying either wastes a flush cycle or
// delays shutdown.
func classifyResponse(status int, body []byte) ([]byte, error) {
	if status >= 500 {
		return nil, &ServerError{Status: status}
	}
	if status >= 400 {
		return nil, &ClientError{Status: status}
	}

	// A misbehaving or overloaded server can return HTTP 200 with
	// {"jsonrpc":"2.0","error":{"message":"..."},"id":1} — this must not
	// be treated as success or the memory is silently lost.
	var rpcResp struct {
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &rpcResp) == nil && rpcResp.Error != nil {
		return nil, &RPCError{Code: rpcResp.Error.Code, Message: scrubServerText(rpcResp.Error.Message)}
	}

	// The MCP tool-level failure is the other in-band signal: HTTP 200 with
	// {"result":{"isError":true,"content":[{"type":"text","text":"vault x not found"}]}}
	// and no JSON-RPC error object. It means the tool refused, so the batch
	// did not land; reading it as success loses the memories silently and
	// reports them flushed.
	var toolResp struct {
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if json.Unmarshal(body, &toolResp) == nil && toolResp.Result.IsError {
		var texts []string
		for _, c := range toolResp.Result.Content {
			if c.Text != "" {
				texts = append(texts, c.Text)
			}
		}
		msg := scrubServerText(strings.Join(texts, "; "))
		if msg == "" {
			msg = "tool reported an error without a message"
		}
		return nil, &RPCError{Code: ToolErrorCode, Message: msg}
	}

	return body, nil
}

// ToolErrorCode is the code reported on an RPCError built from a tool-level
// refusal (result.isError). The server sends no code of its own for that
// shape, and 0 is JSON-RPC's "no error" code: a code-based classifier would
// read a refusal as success.
const ToolErrorCode = -32000

// CheckEnvelope reports whether a 2xx body is the JSON-RPC response object the
// protocol requires. A server answering 200 with truncated JSON, an HTML error
// page from an intermediary, or an empty body yields no confirmation that the
// write happened, and such a response is otherwise indistinguishable from a
// successful one. Callers that discard the response body must call this before
// reporting success; a caller that parses the body downstream has already
// learned what it holds and does not need it.
func CheckEnvelope(body []byte) error {
	var env struct {
		JSONRPC string          `json:"jsonrpc"`
		Result  json.RawMessage `json:"result"`
		Error   json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return fmt.Errorf("response is not a JSON-RPC object: %w (body: %s)", err, bodySummary(body))
	}
	if env.JSONRPC == "" && env.Result == nil && env.Error == nil {
		return fmt.Errorf("response is JSON but carries no jsonrpc, result, or error field: %s", bodySummary(body))
	}
	return nil
}

// bodySummary renders an untrusted response body for an error message, capped
// so a large HTML page does not land whole in a log line. The cut backs off to
// a rune boundary: a server that answers with a non-ASCII error page would
// otherwise have the cap land mid-character and the log line carry a replacement
// character for the rest of it.
func bodySummary(body []byte) string {
	const maxSummary = 200
	if len(body) <= maxSummary {
		return string(body)
	}
	cut := body[:maxSummary]
	for len(cut) > 0 && !utf8.RuneStart(cut[len(cut)-1]) {
		cut = cut[:len(cut)-1]
	}
	return string(cut) + "..."
}
