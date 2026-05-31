package proxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
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
		// No MITMHosts -> default intercept-all, so the 127.0.0.1 test upstream
		// (not the Config.Upstream host) is still terminated.
	})
	if err != nil {
		t.Fatal(err)
	}
	// The MITM forward leg verifies the real upstream's cert — trust the test
	// server's self-signed cert there.
	upstreamPool := x509.NewCertPool()
	upstreamPool.AddCert(upstream.Certificate())
	p.SetMITMRoots(upstreamPool)

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

// TestMITMBlindTunnel proves a non-intercepted host passes through untouched:
// the client trusts ONLY the real upstream's cert (not msc's CA), so a
// successful TLS session can only mean msc blind-tunneled rather than forging a
// leaf. If msc had intercepted, the client would see a msc-minted cert it does
// not trust and the handshake would fail.
func TestMITMBlindTunnel(t *testing.T) {
	var hits atomic.Int64
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Write([]byte(`upstream-ok`))
	}))
	defer upstream.Close()

	ca, err := mitm.LoadOrCreateCA(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	st := store.New("http://127.0.0.1:1", "", "t", &stats.Stats{})
	p, err := New(Config{
		ListenAddr:   "127.0.0.1:0",
		Upstream:     "https://api.example.invalid", // upstream host != the test server's 127.0.0.1
		AgentName:    "claude",
		Store:        st,
		CapturePaths: []string{"/v1/messages"},
		CA:           ca,
		// Scoped mode (non-empty, no "*"): only the upstream host is intercepted,
		// so the 127.0.0.1 test server must be blind-tunneled.
		MITMHosts: []string{"api.example.invalid"},
	})
	if err != nil {
		t.Fatal(err)
	}
	addr, err := p.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer p.Shutdown(context.Background())

	// Client trusts the REAL upstream cert only, NOT msc's CA.
	upstreamPool := x509.NewCertPool()
	upstreamPool.AddCert(upstream.Certificate())
	proxyURL, _ := url.Parse("http://" + addr)
	client := &http.Client{
		Transport: &http.Transport{
			Proxy:           http.ProxyURL(proxyURL),
			TLSClientConfig: &tls.Config{RootCAs: upstreamPool},
		},
		Timeout: 10 * time.Second,
	}

	resp, err := client.Get(upstream.URL + "/v1/messages")
	if err != nil {
		t.Fatalf("blind-tunnel request failed (msc likely intercepted instead of tunneling): %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "upstream-ok" {
		t.Errorf("unexpected body %q", body)
	}
	// The cert the client saw must be the upstream's own, not a msc-minted leaf.
	if resp.TLS == nil || len(resp.TLS.PeerCertificates) == 0 {
		t.Fatal("no peer certificate")
	}
	if resp.TLS.PeerCertificates[0].Issuer.CommonName == "muninn-sidecar local CA" {
		t.Error("client saw a msc-minted cert; host was intercepted, not tunneled")
	}
	if hits.Load() != 1 {
		t.Errorf("upstream hits = %d, want 1", hits.Load())
	}
}

// TestMITMSpliceUpgrade drives an intercepted WebSocket-style upgrade end to
// end: client -> CONNECT -> msc TLS-terminate -> detect Upgrade -> raw splice to
// a 101-echo backend. Proves the agent's upgraded stream works through MITM.
func TestMITMSpliceUpgrade(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isUpgradeRequest(r) {
			http.Error(w, "expected upgrade", http.StatusBadRequest)
			return
		}
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		io.WriteString(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		line, _ := buf.ReadString('\n') // read what the client sends post-upgrade
		io.WriteString(conn, "echo:"+line)
	}))
	defer upstream.Close()

	ca := mustCA(t)
	sessionStats := &stats.Stats{}
	st := store.New("http://127.0.0.1:1", "", "t", sessionStats)
	p, err := New(Config{ListenAddr: "127.0.0.1:0", Upstream: "https://api.example.invalid", Store: st, CA: ca, MITMHosts: []string{"*"}, Stats: sessionStats})
	if err != nil {
		t.Fatal(err)
	}
	upstreamPool := x509.NewCertPool()
	upstreamPool.AddCert(upstream.Certificate())
	p.SetMITMRoots(upstreamPool) // the splice's TLS dial must trust the test backend
	addr, err := p.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer p.Shutdown(context.Background())

	hostport := strings.TrimPrefix(upstream.URL, "https://") // 127.0.0.1:PORT

	// Manual client: CONNECT, then TLS (trusting msc CA), then the ws upgrade.
	raw, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	fmt.Fprintf(raw, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", hostport, hostport)
	br := bufio.NewReader(raw)
	connResp, err := http.ReadResponse(br, &http.Request{Method: "CONNECT"})
	if err != nil || connResp.StatusCode != 200 {
		t.Fatalf("CONNECT failed: resp=%v err=%v", connResp, err)
	}

	caPool := x509.NewCertPool()
	caPool.AppendCertsFromPEM(ca.CertPEM())
	tconn := tls.Client(raw, &tls.Config{RootCAs: caPool, ServerName: "127.0.0.1"})
	if err := tconn.Handshake(); err != nil {
		t.Fatalf("client TLS handshake with msc leaf failed: %v", err)
	}
	fmt.Fprintf(tconn, "GET /ws HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: dGVzdA==\r\nSec-WebSocket-Version: 13\r\n\r\n", "127.0.0.1")

	tbr := bufio.NewReader(tconn)
	upResp, err := http.ReadResponse(tbr, nil)
	if err != nil {
		t.Fatalf("reading upgrade response: %v", err)
	}
	if upResp.StatusCode != 101 {
		t.Fatalf("expected 101 through MITM splice, got %d", upResp.StatusCode)
	}

	// Post-upgrade bytes must round-trip through the splice.
	io.WriteString(tconn, "hello\n")
	echo, err := tbr.ReadString('\n')
	if err != nil {
		t.Fatalf("reading echo: %v", err)
	}
	if echo != "echo:hello\n" {
		t.Errorf("splice round-trip = %q, want %q", echo, "echo:hello\n")
	}
	// The upgrade must be counted as spliced-but-uncaptured.
	if got := sessionStats.Upgraded.Load(); got != 1 {
		t.Errorf("Upgraded counter = %d, want 1", got)
	}
}

func TestIsUpgradeRequest(t *testing.T) {
	mk := func(upgrade, conn string) *http.Request {
		r := httptest.NewRequest("GET", "/", nil)
		r.Header.Del("Connection")
		if upgrade != "" {
			r.Header.Set("Upgrade", upgrade)
		}
		if conn != "" {
			r.Header.Set("Connection", conn)
		}
		return r
	}
	cases := []struct {
		up, conn string
		want     bool
	}{
		{"websocket", "Upgrade", true},
		{"websocket", "keep-alive, Upgrade", true}, // multi-token
		{"h2c", "upgrade", true},                   // case-insensitive token
		{"websocket", "keep-alive", false},         // no upgrade token
		{"", "Upgrade", false},                     // no Upgrade header
		{"", "", false},
	}
	for _, c := range cases {
		if got := isUpgradeRequest(mk(c.up, c.conn)); got != c.want {
			t.Errorf("isUpgradeRequest(up=%q conn=%q) = %v, want %v", c.up, c.conn, got, c.want)
		}
	}
}

func FuzzIsUpgradeRequest(f *testing.F) {
	f.Add("websocket", "Upgrade")
	f.Add("", "keep-alive")
	f.Fuzz(func(t *testing.T, upgrade, conn string) {
		r := httptest.NewRequest("GET", "/", nil)
		r.Header.Set("Upgrade", upgrade)
		r.Header.Set("Connection", conn)
		// Never panics; if it reports an upgrade, the Upgrade header is non-empty.
		if isUpgradeRequest(r) && r.Header.Get("Upgrade") == "" {
			t.Fatal("reported upgrade with empty Upgrade header")
		}
	})
}

// connectStatus opens a raw CONNECT to the proxy for target and returns the
// status code of the CONNECT response.
func connectStatus(t *testing.T, proxyAddr, target string) (*http.Response, *bufio.Reader, net.Conn) {
	t.Helper()
	raw, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(raw, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
	br := bufio.NewReader(raw)
	resp, err := http.ReadResponse(br, &http.Request{Method: "CONNECT"})
	if err != nil {
		raw.Close()
		t.Fatalf("reading CONNECT response: %v", err)
	}
	return resp, br, raw
}

func TestMITMBlindTunnelDialFailure(t *testing.T) {
	// Scoped MITM: 127.0.0.1 is not allowlisted, so it's blind-tunneled. The
	// target port has nothing listening, so the dial fails and msc must reply 502.
	st := store.New("http://127.0.0.1:1", "", "t", &stats.Stats{})
	p, err := New(Config{
		ListenAddr: "127.0.0.1:0", Upstream: "https://api.example.invalid",
		Store: st, CA: mustCA(t), MITMHosts: []string{"api.example.invalid"},
	})
	if err != nil {
		t.Fatal(err)
	}
	addr, err := p.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer p.Shutdown(context.Background())

	resp, _, raw := connectStatus(t, addr, "127.0.0.1:1") // port 1: connection refused
	defer raw.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("blind-tunnel to unreachable target: status %d, want 502", resp.StatusCode)
	}
}

func TestMITMSpliceUpgradeBackendUnreachable(t *testing.T) {
	// Intercept-all: CONNECT to an unreachable target is TLS-terminated, then an
	// upgrade request triggers spliceUpgrade whose backend dial fails -> 502 over
	// the decrypted connection.
	ca := mustCA(t)
	st := store.New("http://127.0.0.1:1", "", "t", &stats.Stats{})
	p, err := New(Config{
		ListenAddr: "127.0.0.1:0", Upstream: "https://api.example.invalid",
		Store: st, CA: ca, MITMHosts: []string{"*"}, Stats: &stats.Stats{},
	})
	if err != nil {
		t.Fatal(err)
	}
	addr, err := p.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer p.Shutdown(context.Background())

	resp, _, raw := connectStatus(t, addr, "127.0.0.1:1")
	defer raw.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("CONNECT (intercept) status %d, want 200", resp.StatusCode)
	}
	caPool := x509.NewCertPool()
	caPool.AppendCertsFromPEM(ca.CertPEM())
	tconn := tls.Client(raw, &tls.Config{RootCAs: caPool, ServerName: "127.0.0.1"})
	if err := tconn.Handshake(); err != nil {
		t.Fatalf("TLS handshake with msc leaf: %v", err)
	}
	fmt.Fprintf(tconn, "GET /ws HTTP/1.1\r\nHost: 127.0.0.1\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: dGVzdA==\r\n\r\n")
	upResp, err := http.ReadResponse(bufio.NewReader(tconn), nil)
	if err != nil {
		t.Fatalf("reading upgrade response: %v", err)
	}
	if upResp.StatusCode != http.StatusBadGateway {
		t.Errorf("splice with unreachable backend: status %d, want 502", upResp.StatusCode)
	}
}

func TestShouldInterceptHost(t *testing.T) {
	p, err := New(Config{
		ListenAddr: "127.0.0.1:0",
		Upstream:   "https://api.anthropic.com",
		Store:      store.New("http://127.0.0.1:1", "", "t", &stats.Stats{}),
		CA:         mustCA(t),
		MITMHosts:  []string{"api.openai.com"},
	})
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]bool{
		"api.anthropic.com":     true, // upstream host
		"API.Anthropic.com":     true, // case-insensitive
		"api.openai.com":        true, // allowlisted
		"registry.npmjs.org":    false,
		"api.githubcopilot.com": false,
		"":                      false,
	}
	for host, want := range cases {
		if got := p.shouldInterceptHost(host); got != want {
			t.Errorf("shouldInterceptHost(%q) = %v, want %v", host, got, want)
		}
	}

	// "*" intercepts everything.
	pAll, _ := New(Config{
		ListenAddr: "127.0.0.1:0", Upstream: "https://api.anthropic.com",
		Store: store.New("http://127.0.0.1:1", "", "t", &stats.Stats{}), CA: mustCA(t),
		MITMHosts: []string{"*"},
	})
	if !pAll.shouldInterceptHost("anything.example.com") {
		t.Error(`"*" should intercept all hosts`)
	}
}

func FuzzShouldInterceptHost(f *testing.F) {
	p, err := New(Config{
		ListenAddr: "127.0.0.1:0", Upstream: "https://api.anthropic.com",
		Store: store.New("http://127.0.0.1:1", "", "t", &stats.Stats{}), CA: mustCA(f),
	})
	if err != nil {
		f.Fatal(err)
	}
	f.Add("api.anthropic.com")
	f.Add("")
	f.Fuzz(func(t *testing.T, host string) {
		// Pure lookup: never panics and is deterministic.
		if p.shouldInterceptHost(host) != p.shouldInterceptHost(host) {
			t.Fatal("non-deterministic result")
		}
	})
}

func mustCA(tb testing.TB) *mitm.CA {
	tb.Helper()
	ca, err := mitm.LoadOrCreateCA(tb.TempDir())
	if err != nil {
		tb.Fatal(err)
	}
	return ca
}

func TestSetMITMRoots(t *testing.T) {
	ca, err := mitm.LoadOrCreateCA(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	st := store.New("http://127.0.0.1:1", "", "t", &stats.Stats{})
	p, err := New(Config{ListenAddr: "127.0.0.1:0", Upstream: "https://x.invalid", Store: st, CA: ca})
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	p.SetMITMRoots(pool)
	if p.mitmTransport.TLSClientConfig.RootCAs != pool {
		t.Error("SetMITMRoots did not apply the pool to the MITM transport")
	}

	// Guard: a zero-value Proxy (no transport) is a safe no-op, not a panic.
	(&Proxy{}).SetMITMRoots(pool)
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
