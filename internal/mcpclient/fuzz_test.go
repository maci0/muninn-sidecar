package mcpclient

import (
	"bytes"
	"errors"
	"testing"
)

// FuzzHealthURLFrom exercises the URL-derivation surface with arbitrary input.
// Invariant: never panic; on success the derived health URL must re-parse.
func FuzzHealthURLFrom(f *testing.F) {
	f.Add("http://127.0.0.1:8750/mcp")
	f.Add("https://example.com/mcp/")
	f.Add("")
	f.Add("://bad")
	f.Fuzz(func(t *testing.T, raw string) {
		got, err := healthURLFrom(raw)
		if err != nil {
			return
		}
		if _, err := healthURLFrom(got); err != nil {
			t.Fatalf("derived health URL %q does not re-parse: %v", got, err)
		}
	})
}

// FuzzClassifyResponse drives the untrusted side of the MCP boundary: whatever
// bytes a MuninnDB server returns, the status/body pair must classify into
// exactly one of three error kinds, and a success must hand the body back
// unchanged. A body that is neither JSON nor an error object is the common
// case (the caller parses it downstream), so it must pass through untouched.
func FuzzClassifyResponse(f *testing.F) {
	f.Add(200, `{"jsonrpc":"2.0","id":1,"result":{"content":[]}}`)
	f.Add(200, `{"jsonrpc":"2.0","error":{"code":-32601,"message":"no such tool"}}`)
	f.Add(200, `{"error":null}`)
	f.Add(200, `{"error":"a string, not an object"}`)
	f.Add(200, `not json at all`)
	f.Add(500, `{"error":{"code":-32000,"message":"overloaded"}}`)
	f.Add(404, `nope`)
	f.Add(302, `moved`)
	f.Fuzz(func(t *testing.T, status int, body string) {
		if status < 100 || status > 599 {
			t.Skip()
		}
		raw := []byte(body)
		got, err := classifyResponse(status, raw)

		var se *ServerError
		var ce *ClientError
		var re *RPCError
		switch {
		case status >= 500:
			if !errors.As(err, &se) {
				t.Fatalf("status %d: got %v, want *ServerError", status, err)
			}
			if se.Status != status {
				t.Fatalf("status %d: ServerError.Status = %d", status, se.Status)
			}
		case status >= 400:
			if !errors.As(err, &ce) {
				t.Fatalf("status %d: got %v, want *ClientError", status, err)
			}
			if errors.As(err, &se) {
				t.Fatalf("status %d: 4xx must not classify as retryable: %v", status, err)
			}
			if ce.Status != status {
				t.Fatalf("status %d: ClientError.Status = %d", status, ce.Status)
			}
		default:
			if errors.As(err, &se) || errors.As(err, &ce) {
				t.Fatalf("status %d: 2xx must not classify as an HTTP error: %v", status, err)
			}
			if err == nil {
				if !bytes.Equal(got, raw) {
					t.Fatalf("status %d: success body altered", status)
				}
			} else if !errors.As(err, &re) {
				t.Fatalf("status %d: got %v, want *RPCError", status, err)
			}
		}

		// No error kind carries a body back to the caller: a caller that ignores
		// the error must not mistake an error payload for stored memories.
		if err != nil && got != nil {
			t.Fatalf("status %d: error returned a body alongside %v", status, err)
		}
	})
}
