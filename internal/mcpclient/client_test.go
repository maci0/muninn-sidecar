package mcpclient

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCallRPCError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","error":{"code":-32000,"message":"vault not found"},"id":1}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "", 5*time.Second)
	_, err := c.Call(context.Background(), "muninn_remember", map[string]any{"vault": "x"})
	if err == nil {
		t.Fatal("expected error for JSON-RPC error response, got nil")
	}
	var rpcErr *RPCError
	if !errors.As(err, &rpcErr) {
		t.Fatalf("expected *RPCError, got %T: %v", err, err)
	}
	if rpcErr.Message != "vault not found" {
		t.Fatalf("expected message %q, got %q", "vault not found", rpcErr.Message)
	}
}

func TestCallSuccess(t *testing.T) {
	var gotContentType, gotAccept, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotContentType = r.Header.Get("Content-Type")
		gotAccept = r.Header.Get("Accept")
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","result":{"content":[{"type":"text","text":"ok"}]},"id":1}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "tok", 5*time.Second)
	body, err := c.Call(context.Background(), "muninn_remember", map[string]any{"vault": "x"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(body) == 0 {
		t.Fatal("expected non-empty body for successful response")
	}
	// Content negotiation: request JSON, send JSON, carry the bearer token.
	if gotContentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotContentType)
	}
	if gotAccept != "application/json" {
		t.Errorf("Accept = %q, want application/json", gotAccept)
	}
	if gotAuth != "Bearer tok" {
		t.Errorf("Authorization = %q, want 'Bearer tok'", gotAuth)
	}
}

func TestCallHTTPClientError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
	}))
	defer srv.Close()

	c := New(srv.URL, "", 5*time.Second)
	_, err := c.Call(context.Background(), "muninn_remember", map[string]any{"vault": "x"})
	if err == nil {
		t.Fatal("expected error for 4xx response, got nil")
	}
	var clientErr *ClientError
	if !errors.As(err, &clientErr) {
		t.Fatalf("expected *ClientError, got %T: %v", err, err)
	}
	if clientErr.Status != 400 {
		t.Fatalf("expected status 400, got %d", clientErr.Status)
	}
}

func TestCallHTTPServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer srv.Close()

	c := New(srv.URL, "", 5*time.Second)
	_, err := c.Call(context.Background(), "muninn_remember", map[string]any{"vault": "x"})
	if err == nil {
		t.Fatal("expected error for 5xx response, got nil")
	}
	if got := err.Error(); got != "server error: HTTP 500" {
		t.Fatalf("got %q, want %q", got, "server error: HTTP 500")
	}
}

func TestHealthURLFrom(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"http://localhost:8750/mcp", "http://localhost:8750/mcp/health"},
		{"http://localhost:8750/mcp/", "http://localhost:8750/mcp/health"},
		{"http://example.com/api/mcp", "http://example.com/api/mcp/health"},
	}

	for _, tt := range tests {
		got, err := healthURLFrom(tt.input)
		if err != nil {
			t.Fatalf("healthURLFrom(%q) error: %v", tt.input, err)
		}
		if got != tt.want {
			t.Fatalf("healthURLFrom(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestErrorMessages(t *testing.T) {
	if got := (&ClientError{Status: 404}).Error(); got != "client error: HTTP 404" {
		t.Errorf("ClientError: %q", got)
	}
	if got := (&RPCError{Code: -32601, Message: "no method"}).Error(); got != "rpc error -32601: no method" {
		t.Errorf("RPCError: %q", got)
	}
}

func TestHealthCheck(t *testing.T) {
	t.Run("ok", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/mcp/health" {
				t.Errorf("unexpected health path %q", r.URL.Path)
			}
			w.WriteHeader(200)
		}))
		defer srv.Close()
		if err := New(srv.URL+"/mcp", "", time.Second).HealthCheck(); err != nil {
			t.Errorf("expected healthy, got %v", err)
		}
	})
	t.Run("5xx is error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(500)
		}))
		defer srv.Close()
		if err := HealthCheckAt(srv.URL+"/mcp", ""); err == nil {
			t.Error("expected error on 500 health")
		}
	})
	t.Run("unreachable is error", func(t *testing.T) {
		if err := HealthCheckAt("http://127.0.0.1:1/mcp", ""); err == nil {
			t.Error("expected error on unreachable")
		}
	})
}

// TestCallToolError pins the MCP tool-level failure: an HTTP 200 carrying
// result.isError means the tool refused, so the batch did not land. Reading it
// as success reports the captures flushed while the memories are lost.
func TestCallToolError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"isError":true,"content":[{"type":"text","text":"vault sidecar not found"}]}}`))
	}))
	defer srv.Close()

	_, err := New(srv.URL, "", 5*time.Second).Call(context.Background(), "muninn_remember_batch", map[string]any{})
	if err == nil {
		t.Fatal("expected an error for result.isError")
	}
	var rpcErr *RPCError
	if !errors.As(err, &rpcErr) {
		t.Fatalf("expected *RPCError, got %T: %v", err, err)
	}
	if !strings.Contains(rpcErr.Message, "vault sidecar not found") {
		t.Errorf("tool error text not surfaced: %q", rpcErr.Message)
	}
}

// TestCallResultIsNotAnError guards the other side: a normal tool result must
// keep passing through untouched.
func TestCallResultIsNotAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"ok"}]}}`))
	}))
	defer srv.Close()

	body, err := New(srv.URL, "", 5*time.Second).Call(context.Background(), "muninn_remember_batch", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"ok"`) {
		t.Errorf("unexpected body: %s", body)
	}
}

// TestContentTextsReturnsEveryTextBlock: a tool's payload rides in a text
// content block, and a server may put a human-readable summary in front of it.
// Returning only the first block would hand every caller a payload it cannot
// read, so all of them come back in order and non-text blocks are left out.
func TestContentTextsReturnsEveryTextBlock(t *testing.T) {
	body := []byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[
		{"type":"text","text":"Recalled 2 memories."},
		{"type":"image","data":"..."},
		{"type":"text","text":"{\"memories\":[]}"}
	]}}`)
	got, err := ContentTexts(body)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"Recalled 2 memories.", `{"memories":[]}`}
	if len(got) != len(want) {
		t.Fatalf("ContentTexts = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("ContentTexts[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// A body that is not a JSON-RPC result is a broken response, not one with no
// content: reporting the two the same way makes a corrupt reply
// indistinguishable from a server that had nothing to say.
func TestContentTextsRejectsNonJSONRPCBodies(t *testing.T) {
	for _, body := range []string{"", "<html>502</html>", "null", `{"unrelated":true}`} {
		if _, err := ContentTexts([]byte(body)); err == nil {
			t.Errorf("ContentTexts(%q) = nil error, want an error", body)
		}
	}
}

// A well-formed result with no content blocks is a real answer with nothing in
// it, which is not an error.
func TestContentTextsEmptyResultIsNotAnError(t *testing.T) {
	got, err := ContentTexts([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("ContentTexts = %q, want none", got)
	}
}

// TestTextContent: the status path reads the first non-empty text block, and
// reports false both for a body that does not parse and for one carrying no
// text block. A non-text block is skipped and an empty text block is not one.
func TestTextContent(t *testing.T) {
	for _, tc := range []struct {
		name   string
		body   string
		want   string
		wantOK bool
	}{
		{
			name:   "text block",
			body:   `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"{\"health\":\"good\"}"}]}}`,
			want:   `{"health":"good"}`,
			wantOK: true,
		},
		{
			// TextContent takes the first non-empty text block, so a summary
			// ahead of the payload is the answer. Callers that cannot rule a
			// summary out need ContentTexts, which returns every block.
			name:   "first non-empty text block, ahead of a later one",
			body:   `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"Vault has 47 memories."},{"type":"image","data":"..."},{"type":"text","text":"payload"}]}}`,
			want:   "Vault has 47 memories.",
			wantOK: true,
		},
		{
			name: "no text block",
			body: `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"image","data":"..."}]}}`,
		},
		{
			name: "empty text block",
			body: `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":""}]}}`,
		},
		{
			name: "empty result",
			body: `{"jsonrpc":"2.0","id":1,"result":{}}`,
		},
		{
			name: "unparsable body",
			body: "<html>502</html>",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := TextContent([]byte(tc.body))
			if ok != tc.wantOK {
				t.Fatalf("TextContent ok = %v, want %v (text %q)", ok, tc.wantOK, got)
			}
			if got != tc.want {
				t.Errorf("TextContent = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestCheckEnvelopeRejectsUnconfirmedWrites: a 2xx that is not a JSON-RPC
// response object confirms nothing was stored. A caller that discards the body
// and reports success on it loses a whole batch silently.
func TestCheckEnvelopeRejectsUnconfirmedWrites(t *testing.T) {
	valid := []string{
		`{"jsonrpc":"2.0","id":1,"result":{"id":"ok"}}`,
		`{"jsonrpc":"2.0","error":{"code":-32000,"message":"x"},"id":1}`,
		`{"result":{}}`,
	}
	for _, body := range valid {
		if err := CheckEnvelope([]byte(body)); err != nil {
			t.Errorf("CheckEnvelope(%s) = %v, want nil", body, err)
		}
	}
	invalid := []string{"", "<html>502 Bad Gateway</html>", `{"unrelated":true}`, "null"}
	for _, body := range invalid {
		if err := CheckEnvelope([]byte(body)); err == nil {
			t.Errorf("CheckEnvelope(%s) = nil, want an error", body)
		}
	}
}

// TestToolErrorHasItsOwnCode: a tool refusal sent no code of its own, and 0 is
// JSON-RPC's "no error"; a code-based classifier would read it as success.
func TestToolErrorHasItsOwnCode(t *testing.T) {
	if ToolErrorCode == 0 {
		t.Fatal("ToolErrorCode must not be JSON-RPC's reserved no-error code")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"isError":true,"content":[{"type":"text","text":"nope"}]}}`))
	}))
	defer srv.Close()

	_, err := New(srv.URL, "", 5*time.Second).Call(context.Background(), "muninn_remember_batch", map[string]any{})
	var rpcErr *RPCError
	if !errors.As(err, &rpcErr) {
		t.Fatalf("expected *RPCError, got %T: %v", err, err)
	}
	if rpcErr.Code != ToolErrorCode {
		t.Errorf("tool error code = %d, want %d", rpcErr.Code, ToolErrorCode)
	}
}

func TestDedupKeyIsContentAddressed(t *testing.T) {
	a := DedupKey("v", "concept", "content")
	if a != DedupKey("v", "concept", "content") {
		t.Fatal("DedupKey is not stable across calls for identical input")
	}
	if a == DedupKey("v", "concept", "other content") {
		t.Fatal("different content produced the same dedup key")
	}
	if a == DedupKey("other", "concept", "content") {
		t.Fatal("different vault produced the same dedup key")
	}
	if a == DedupKey("v", "other concept", "content") {
		t.Fatal("different concept produced the same dedup key")
	}
	if a == DedupKey("v", "concept", "content\x00other") {
		t.Fatal("a field separator inside content produced the same dedup key")
	}
}

// A memory's concept and content are captured conversation text, and NUL is a
// legal character in it. With a NUL separator rather than a length prefix, the
// NUL inside one field framed exactly like the separator between two, so these
// two distinct memories hashed to one dedup_key and the second write was
// discarded as a duplicate of the first.
func TestDedupKeyFieldsCannotShiftAcrossSeparators(t *testing.T) {
	shifted := DedupKey("v", "a\x00b", "c")
	straight := DedupKey("v", "a", "b\x00c")
	if shifted == straight {
		t.Fatalf("a NUL inside the concept framed like a field separator: both memories got dedup key %s", shifted)
	}
	if shifted == DedupKey("v", "a\x00bc", "") {
		t.Fatal("a concept ending in NUL collided with the same NUL used as a separator")
	}
	if straight == DedupKey("v", "a\x00", "bc") {
		t.Fatal("a concept ending in NUL collided with a NUL-prefixed content field")
	}
}

// A percent-encoded separator in the MCP path is part of the path, not a
// separator. Rewriting the decoded Path alone re-encoded it as a real "/", and
// the health check went to a path the server never serves.
func TestHealthURLFromKeepsEncodedSeparatorsEncoded(t *testing.T) {
	got, err := healthURLFrom("http://127.0.0.1:8750/rpc%2Fv1")
	if err != nil {
		t.Fatalf("healthURLFrom: %v", err)
	}
	if want := "http://127.0.0.1:8750/rpc%2Fv1/health"; got != want {
		t.Errorf("healthURLFrom = %q, want %q", got, want)
	}
}

// A server that quotes the memory it refused would otherwise hand the captured
// conversation straight back to the caller's log line, past the redaction the
// write path applies. Both error shapes (JSON-RPC error object and tool-level
// isError) go through the same scrub.
func TestServerErrorTextIsScrubbed(t *testing.T) {
	const secret = "ada.lovelace@example.com"

	for _, tc := range []struct {
		name string
		body string
	}{
		{"rpc error object", `{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"rejected: ` + secret + `"}}`},
		{"tool isError", `{"jsonrpc":"2.0","id":1,"result":{"isError":true,"content":[{"type":"text","text":"rejected: ` + secret + `"}]}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			_, err := New(srv.URL, "", 5*time.Second).Call(context.Background(), "muninn_remember", map[string]any{})
			if err == nil {
				t.Fatal("expected an error")
			}
			if strings.Contains(err.Error(), secret) {
				t.Errorf("server error text carried personal data into the error: %v", err)
			}
			if !strings.Contains(err.Error(), "rejected:") {
				t.Errorf("scrub dropped the diagnostic text: %v", err)
			}
		})
	}
}

// An error message long enough to be a payload rather than a sentence is
// truncated, so a server echoing a whole memory cannot fill a log line.
func TestServerErrorTextIsBounded(t *testing.T) {
	_, err := classifyResponse(200, []byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"`+
		strings.Repeat("x", maxErrorRunes*2)+`"}}`))
	var rpcErr *RPCError
	if !errors.As(err, &rpcErr) {
		t.Fatalf("expected *RPCError, got %T: %v", err, err)
	}
	if n := len([]rune(rpcErr.Message)); n > maxErrorRunes+1 {
		t.Errorf("message not bounded: %d runes", n)
	}
	if !strings.HasSuffix(rpcErr.Message, "…") {
		t.Error("truncation is not marked")
	}
}

// A rejection long enough to be truncated is still scrubbed: the cap must not
// become a way around the redaction the short path applies.
func TestServerErrorTextScrubsBeforeTruncating(t *testing.T) {
	const secret = "ada.lovelace@example.com"
	_, err := classifyResponse(200, []byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"rejected: `+
		secret+` `+strings.Repeat("x", maxErrorRunes*2)+`"}}`))
	var rpcErr *RPCError
	if !errors.As(err, &rpcErr) {
		t.Fatalf("expected *RPCError, got %T: %v", err, err)
	}
	if strings.Contains(rpcErr.Message, secret) {
		t.Errorf("over-long server error text carried personal data into the error: %v", rpcErr)
	}
}

// Truncation must not land mid-rune: a message ending in a multi-byte character
// has to come back intact or as U+FFFD-free text, not a replacement char.
func TestServerErrorTextTruncationKeepsRunes(t *testing.T) {
	msg := strings.Repeat("é", maxErrorRunes+50)
	_, err := classifyResponse(200, []byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"`+msg+`"}}`))
	var rpcErr *RPCError
	if !errors.As(err, &rpcErr) {
		t.Fatalf("expected *RPCError, got %T: %v", err, err)
	}
	if strings.ContainsRune(rpcErr.Message, '�') {
		t.Errorf("truncation split a rune: %q", rpcErr.Message[maxErrorRunes-3:])
	}
}

// A message long enough to be capped is long because the server quoted the
// memory it refused. The cap must come after the scrub, or the truncated error
// hands back the first maxErrorRunes of the very content every call site
// treats as loggable.
func TestServerErrorTextScrubsBeforeCapping(t *testing.T) {
	secret := "sk-ant-" + strings.Repeat("a", 40)
	// The secret sits at the head, so a cap taken before the scrub would keep it.
	msg := "content too long: " + secret + ", " + strings.Repeat("and more prose after it ", 20)
	_, err := classifyResponse(200, []byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"`+msg+`"}}`))
	var rpcErr *RPCError
	if !errors.As(err, &rpcErr) {
		t.Fatalf("expected *RPCError, got %T: %v", err, err)
	}
	if strings.Contains(rpcErr.Message, secret) {
		t.Errorf("capped error text carried the secret through unredacted: %q", rpcErr.Message)
	}
	if n := len([]rune(rpcErr.Message)); n > maxErrorRunes+1 {
		t.Errorf("message not bounded: %d runes", n)
	}
}

// The summary of a non-JSON body reaches a log line, so the cap must land on a
// rune boundary (no replacement character) and the text must be scrubbed: the
// server authors it, and a refusal can quote the memory it rejected.
func TestCheckEnvelopeSummaryIsRuneSafeAndScrubbed(t *testing.T) {
	body := append([]byte(strings.Repeat("a", 199)), []byte("€ secret=sk-live-ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789")...)
	err := CheckEnvelope(body)
	if err == nil {
		t.Fatal("a non-JSON body must be reported as an error")
	}
	msg := err.Error()
	if strings.Contains(msg, "�") {
		t.Errorf("summary cut mid-rune and carries a replacement character: %q", msg)
	}
	if strings.Contains(msg, "sk-live-") {
		t.Errorf("server-supplied body reached the error unscrubbed: %q", msg)
	}
}
