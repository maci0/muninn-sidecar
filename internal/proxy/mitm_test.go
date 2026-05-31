package proxy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/maci0/muninn-sidecar/internal/inject"
	"github.com/maci0/muninn-sidecar/internal/mitm"
	"github.com/maci0/muninn-sidecar/internal/stats"
	"github.com/maci0/muninn-sidecar/internal/store"
)

// TestMITMInterceptsHTTPS drives the full TLS-MITM path: a client that trusts
// msc's CA and routes through it as an HTTPS proxy sends `CONNECT` to an HTTPS
// upstream; msc terminates TLS with a minted leaf, runs the recall/inject +
// capture pipeline on the decrypted request, and re-originates TLS to the real
// upstream. We assert the upstream saw the *enriched* body and the exchange was
// captured — proving interception works without the agent overriding any URL.
func TestMITMInterceptsHTTPS(t *testing.T) {
	var (
		upstreamMu   sync.Mutex
		upstreamBody string
	)
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		upstreamMu.Lock()
		upstreamBody = string(body)
		upstreamMu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id":    "msg_mitm",
			"model": "claude-3-opus",
			"content": []map[string]string{
				{"type": "text", "text": "ok"},
			},
			"usage": map[string]any{"input_tokens": 10, "output_tokens": 5},
		})
	}))
	defer upstream.Close()

	var (
		storeMu    sync.Mutex
		storeCalls []string
	)
	muninn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodyStr := string(body)
		var rpc struct {
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		json.Unmarshal(body, &rpc)
		switch rpc.Params.Name {
		case "muninn_where_left_off":
			w.Write(fakeWhereLeftOffEmpty())
		case "muninn_recall":
			w.Write(fakeRecallResponse([]map[string]any{
				{"id": "mem1", "concept": "Go preference", "content": "User prefers Go for backend services", "score": 0.92},
			}))
		case "muninn_remember", "muninn_remember_batch":
			storeMu.Lock()
			storeCalls = append(storeCalls, bodyStr)
			storeMu.Unlock()
			w.WriteHeader(200)
			w.Write([]byte(`{"jsonrpc":"2.0","result":{"id":"ok"},"id":1}`))
		default:
			w.WriteHeader(200)
			w.Write([]byte(`{"jsonrpc":"2.0","result":{},"id":1}`))
		}
	}))
	defer muninn.Close()

	ca, err := mitm.LoadOrCreateCA(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	sessionStats := &stats.Stats{}
	st := store.New(muninn.URL, "", "test", sessionStats)
	injector := inject.New(inject.Config{
		MCPURL:  muninn.URL,
		Vault:   "test",
		Budget:  2048,
		Timeout: 2 * time.Second,
		Stats:   sessionStats,
	})

	p, err := New(Config{
		ListenAddr:   "127.0.0.1:0",
		Upstream:     "https://unused.invalid", // MITM forwards to the CONNECT target, not this
		AgentName:    "claude",
		Store:        st,
		CapturePaths: []string{"/v1/messages"},
		Injector:     injector,
		CA:           ca,
	})
	if err != nil {
		t.Fatal(err)
	}
	// The MITM forward leg verifies the real upstream's cert — trust the test
	// server's self-signed cert there.
	upstreamPool := x509.NewCertPool()
	upstreamPool.AddCert(upstream.Certificate())
	p.mitmTransport.TLSClientConfig.RootCAs = upstreamPool

	addr, err := p.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer p.Shutdown(context.Background())

	// Client trusts msc's CA and uses msc as its HTTPS proxy (CONNECT).
	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(ca.CertPEM()) {
		t.Fatal("could not add msc CA to client pool")
	}
	proxyURL, _ := url.Parse("http://" + addr)
	client := &http.Client{
		Transport: &http.Transport{
			Proxy:           http.ProxyURL(proxyURL),
			TLSClientConfig: &tls.Config{RootCAs: caPool},
		},
		Timeout: 10 * time.Second,
	}

	reqBody := `{"model":"claude-3-opus","system":"You are helpful","messages":[{"role":"user","content":"What language should I use?"}]}`
	resp, err := client.Post(upstream.URL+"/v1/messages", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("MITM request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200 through MITM, got %d", resp.StatusCode)
	}

	upstreamMu.Lock()
	got := upstreamBody
	upstreamMu.Unlock()
	if !strings.Contains(got, "retrieved-context") {
		t.Errorf("upstream did not receive enriched body through MITM: %s", got)
	}
	if !strings.Contains(got, "prefers Go") {
		t.Errorf("injected memory missing from MITM-forwarded body: %s", got)
	}

	st.Drain()
	storeMu.Lock()
	calls := strings.Join(storeCalls, " ")
	storeMu.Unlock()
	if calls == "" {
		t.Error("exchange was not captured through MITM")
	}
	if strings.Contains(calls, "retrieved-context") {
		t.Error("captured exchange should not contain injected context")
	}
	if sessionStats.Injections.Load() != 1 {
		t.Errorf("expected 1 injection through MITM, got %d", sessionStats.Injections.Load())
	}
}

func TestSingleConnListener(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c2.Close()
	l := newSingleConnListener(c1)

	// Addr reflects the wrapped connection.
	if l.Addr() == nil {
		t.Error("Addr returned nil")
	}

	// First Accept yields the conn; closing it must unblock the second Accept
	// with net.ErrClosed so http.Server.Serve can return.
	got, err := l.Accept()
	if err != nil || got == nil {
		t.Fatalf("first Accept: conn=%v err=%v", got, err)
	}

	accepted := make(chan error, 1)
	go func() {
		_, err := l.Accept()
		accepted <- err
	}()
	got.Close() // notifyConn.Close signals done

	select {
	case err := <-accepted:
		if err != net.ErrClosed {
			t.Errorf("second Accept err = %v, want net.ErrClosed", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("second Accept did not unblock after conn close")
	}

	// Close is idempotent (sync.Once) and safe to call again.
	if err := l.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

func TestStripPort(t *testing.T) {
	cases := map[string]string{
		"api.openai.com:443": "api.openai.com",
		"api.openai.com":     "api.openai.com",
		"[2001:db8::1]:443":  "2001:db8::1",
		"[2001:db8::1]":      "2001:db8::1",
		"127.0.0.1:8080":     "127.0.0.1",
		"127.0.0.1":          "127.0.0.1",
	}
	for in, want := range cases {
		if got := stripPort(in); got != want {
			t.Errorf("stripPort(%q) = %q, want %q", in, got, want)
		}
	}
}

func FuzzStripPort(f *testing.F) {
	f.Add("api.openai.com:443")
	f.Add("[2001:db8::1]:443")
	f.Add("")
	f.Fuzz(func(t *testing.T, hostport string) {
		// Crash-safety: any string (including malformed CONNECT targets) must be
		// handled without panicking. For well-formed host:port, the port is gone.
		got := stripPort(hostport)
		if h, _, err := net.SplitHostPort(hostport); err == nil && got != h {
			t.Fatalf("stripPort(%q) = %q, want host %q", hostport, got, h)
		}
	})
}
