// Package mcpclient provides a shared JSON-RPC 2.0 client for MuninnDB MCP calls.
// Used by both the store (async delivery) and inject (recall/enrichment) packages.
package mcpclient

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/binary"
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
//
// Each field is length-prefixed rather than separated by a NUL byte. The three
// fields are captured conversation text, and a NUL is a legal character in it,
// so a separator alone is ambiguous: the concept "a\x00b" with content "c" and
// the concept "a" with content "b\x00c" framed to the same bytes and hashed to
// one dedup_key, and the second memory was dropped as a duplicate of the first.
// A big-endian length in front of every field makes the framing unambiguous for
// any input. This changes the key for every memory, so a store seeded before
// the change reads every pre-existing memory as new.
func DedupKey(vault, concept, content string) string {
	h := sha256.New()
	for _, field := range [3]string{vault, concept, content} {
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], uint64(len(field)))
		h.Write(length[:])
		io.WriteString(h, field)
	}
	return hex.EncodeToString(h.Sum(nil))
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
//
// The append happens on the escaped path, with RawPath set alongside Path, so a
// percent-encoded separator in the MCP path survives. Assigning to Path alone
// would drop RawPath's claim to be an encoding of Path, and String() re-encodes
// from the decoded form: an MCP URL ending in "/rpc%2Fv1" became "/rpc/v1/health",
// a health request to a path the server never serves.
func healthURLFrom(mcpURL string) (string, error) {
	u, err := url.Parse(mcpURL)
	if err != nil {
		return "", fmt.Errorf("invalid MCP URL: %w", err)
	}
	escaped := strings.TrimSuffix(u.EscapedPath(), "/") + "/health"
	path, err := url.PathUnescape(escaped)
	if err != nil {
		return "", fmt.Errorf("invalid MCP URL path: %w", err)
	}
	u.Path, u.RawPath = path, escaped
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
// 5xx is returned as a *ServerError, the type callers match on to decide a
// failure is retryable.
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

// ContentTexts returns the text of every text-typed content block in a
// successful JSON-RPC result, in the order the server sent them. A tool's
// payload is carried as text inside a content block rather than as the result
// value itself, so decoding that block is the first step of reading any
// response body. The blocks are returned whole rather than the first one alone:
// a server may prepend a human-readable summary block, so a caller parsing a
// payload out of a block may have to look past one it cannot read. An error
// means the body is not a JSON-RPC result with content blocks at all, which is
// a broken response rather than an empty one.
func ContentTexts(body []byte) ([]string, error) {
	// A body that is JSON but not a JSON-RPC response decodes into the struct
	// below as an empty result, so without this it would read as a reply with
	// nothing in it. CheckEnvelope is what tells the two apart.
	if err := CheckEnvelope(body); err != nil {
		return nil, err
	}
	var resp struct {
		Result struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("response is not a JSON-RPC object: %w (body: %s)", err, bodySummary(body))
	}
	var texts []string
	for _, c := range resp.Result.Content {
		if c.Type == "text" {
			texts = append(texts, c.Text)
		}
	}
	return texts, nil
}

// TextBlock is one content block of a JSON-RPC result.
type TextBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// TextResponse is the JSON-RPC envelope every MuninnDB tool replies in. The
// tool's own payload is carried as text inside a content block, not as the
// result value itself.
type TextResponse struct {
	Result struct {
		Content []TextBlock `json:"content"`
	} `json:"result"`
}

// DecodeTextResponse parses a JSON-RPC response body into its envelope.
func DecodeTextResponse(body []byte) (TextResponse, error) {
	var resp TextResponse
	err := json.Unmarshal(body, &resp)
	return resp, err
}

// TextContent extracts the text of the first text-typed content block in a
// JSON-RPC response. It reports false for a body that does not parse and for
// one carrying no text block: a broken response and an empty one are different
// failures, so callers decide which one to raise.
func TextContent(body []byte) (string, bool) {
	resp, err := DecodeTextResponse(body)
	if err != nil {
		return "", false
	}
	for _, c := range resp.Result.Content {
		if c.Type == "text" && c.Text != "" {
			return c.Text, true
		}
	}
	return "", false
}

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
// so a large HTML page does not land whole in a log line, and scrubbed for the
// same reason scrubServerText exists: the server authors this text about the
// request it just refused, so it can quote the memory that was refused. The cut
// backs off to a rune boundary: a server that answers with a non-ASCII error
// page would otherwise have the cap land mid-character and the log line carry
// a replacement character for the rest of it.
func bodySummary(body []byte) string {
	const maxSummary = 200
	if len(body) <= maxSummary {
		return scrubServerText(string(body))
	}
	cut := body[:maxSummary]
	for len(cut) > 0 {
		// A trailing RuneError of one byte is the start of a rune the cap cut in
		// half; a width above 1 is a real U+FFFD the server sent. Dropping the
		// partial rune is what keeps a replacement character out of the line.
		if r, size := utf8.DecodeLastRune(cut); r != utf8.RuneError || size > 1 {
			break
		}
		cut = cut[:len(cut)-1]
	}
	return scrubServerText(string(cut)) + "…"
}
