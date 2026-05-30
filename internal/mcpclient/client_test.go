package mcpclient

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
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
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","result":{"content":[{"type":"text","text":"ok"}]},"id":1}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "", 5*time.Second)
	body, err := c.Call(context.Background(), "muninn_remember", map[string]any{"vault": "x"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(body) == 0 {
		t.Fatal("expected non-empty body for successful response")
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
	var serverErr *ServerError
	if !errors.As(err, &serverErr) {
		t.Fatalf("expected *ServerError, got %T: %v", err, err)
	}
	if serverErr.Status != 500 {
		t.Fatalf("expected status 500, got %d", serverErr.Status)
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
	if got := (&ServerError{Status: 503}).Error(); got != "server error: HTTP 503" {
		t.Errorf("ServerError: %q", got)
	}
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
