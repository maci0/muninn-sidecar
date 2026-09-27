package proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
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

// TestBlindTunnelDrainsBothDirections proves a client that half-closes after
// sending its request still receives the server's full response: blindTunnel
// must wait for both copy directions, not return on the first.
func TestBlindTunnelDrainsBothDirections(t *testing.T) {
	// Upstream server: read the request until the client's half-close (EOF),
	// then reply and close. Models a server that answers only after the request.
	upLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer upLn.Close()
	go func() {
		c, err := upLn.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		io.ReadAll(c) // drains until the client's CloseWrite -> EOF
		io.WriteString(c, "SERVER-RESPONSE")
	}()

	// Client side: a real TCP pair so CloseWrite propagates a FIN.
	clLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer clLn.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := clLn.Accept()
		if err != nil {
			accepted <- nil
			return
		}
		accepted <- c
	}()
	testEnd, err := net.Dial("tcp", clLn.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer testEnd.Close()
	clientConn := <-accepted
	if clientConn == nil {
		t.Fatal("client accept failed")
	}
	defer clientConn.Close()

	go (&Proxy{}).blindTunnel(clientConn, upLn.Addr().String(), "req-test")

	testEnd.SetDeadline(time.Now().Add(5 * time.Second))
	br := bufio.NewReader(testEnd)
	// Consume the "200 Connection established" header block.
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("reading CONNECT 200: %v", err)
		}
		if line == "\r\n" {
			break
		}
	}
	io.WriteString(testEnd, "REQUEST")
	testEnd.(*net.TCPConn).CloseWrite() // half-close: request done, response pending

	rest, err := io.ReadAll(br)
	if err != nil {
		t.Fatalf("reading server response through tunnel: %v", err)
	}
	if !strings.Contains(string(rest), "SERVER-RESPONSE") {
		t.Errorf("tunnel truncated the response after client half-close: got %q", rest)
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
		// Pure lookup: never panics and is deterministic. (Separate vars so the
		// repeated call isn't flagged as an identical-expression comparison.)
		first := p.shouldInterceptHost(host)
		second := p.shouldInterceptHost(host)
		if first != second {
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
	if p.mitmRootCAs() != pool {
		t.Error("SetMITMRoots did not apply the pool to the MITM forward leg")
	}
	// The pool must reach the handshake itself, not just the stored field.
	if got := p.mitmTLSDialConfig("api.example.com:443").RootCAs; got != pool {
		t.Error("forward-leg TLS config did not carry the configured roots")
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

func TestSameHost(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"api.openai.com", "api.openai.com", true},
		{"API.OpenAI.com", "api.openai.com", true},      // DNS is case-insensitive
		{"api.openai.com.", "api.openai.com", true},     // explicit DNS root dot
		{"api.openai.com:443", "api.openai.com", false}, // caller must stripPort first
		{"api.openai.com", "evil.example.com", false},
		{"", "api.openai.com", false},
		{"", "", true},
	}
	for _, c := range cases {
		if got := sameHost(c.a, c.b); got != c.want {
			t.Errorf("sameHost(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

// TestMITMRejectsMismatchedSNI pins that a leaf is only ever minted for the
// host the client opened the tunnel to. The client here names one host in the
// CONNECT and a different one in the ClientHello; the CA is trusted by every
// agent msc launches, so minting for the SNI would hand any client that can
// reach the proxy a valid certificate for any name on demand.
func TestMITMRejectsMismatchedSNI(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	upstreamPool := x509.NewCertPool()
	upstreamPool.AddCert(upstream.Certificate())

	ca := mustCA(t)
	st := store.New("http://127.0.0.1:1", "", "t", &stats.Stats{})
	p, err := New(Config{
		ListenAddr: "127.0.0.1:0", Upstream: "https://unused.invalid",
		Store: st, CA: ca, MITMHosts: []string{"*"},
	})
	if err != nil {
		t.Fatal(err)
	}
	p.SetMITMRoots(upstreamPool)
	addr, err := p.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer p.Shutdown(context.Background())

	raw, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()

	// Tunnel to the real upstream, but claim a different name in the handshake.
	target := strings.TrimPrefix(upstream.URL, "https://")
	if _, err := io.WriteString(raw, "CONNECT "+target+" HTTP/1.1\r\nHost: "+target+"\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(raw)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT returned %d, want 200", resp.StatusCode)
	}

	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(ca.CertPEM()) {
		t.Fatal("could not add msc CA to client pool")
	}
	tc := tls.Client(raw, &tls.Config{RootCAs: caPool, ServerName: "evil.example.com"})
	// The client sees only the server's TLS alert ("internal error"), not the
	// reason, so the assertion is the property itself: no leaf is minted for a
	// name the client did not tunnel to. TestSameHost covers the comparison.
	if err := tc.Handshake(); err == nil {
		t.Fatal("handshake succeeded for an SNI that does not match the tunnel target; msc minted a leaf for an arbitrary name")
	}
}

// TestMITMConnectHandshakeTimeout pins the bound on the TLS handshake of a
// hijacked CONNECT tunnel. After Hijack the http.Server no longer owns the
// connection, so its ReadHeaderTimeout stops applying: a client that opens
// CONNECT, reads the 200, and then sends no ClientHello would hold the handling
// goroutine and both sockets forever.
func TestMITMConnectHandshakeTimeout(t *testing.T) {
	st := store.New("http://127.0.0.1:1", "", "t", &stats.Stats{})
	p, err := New(Config{
		ListenAddr: "127.0.0.1:0", Upstream: "https://api.example.invalid",
		Store: st, CA: mustCA(t), MITMHosts: []string{"*"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Shorten before Start, so no handler goroutine can observe the change.
	p.handshakeTimeout = 200 * time.Millisecond
	addr, err := p.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer p.Shutdown(context.Background())

	// Intercept-all, so the CONNECT target need not be reachable: the handshake
	// happens before any per-request forwarding.
	resp, _, raw := connectStatus(t, addr, "silent.invalid:443")
	defer raw.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("CONNECT status %d, want 200", resp.StatusCode)
	}

	// Send no ClientHello; msc must give up on the handshake and drop the
	// tunnel rather than holding it open indefinitely.
	raw.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadAll(raw); err != nil {
		t.Fatalf("reading the abandoned tunnel: %v", err)
	}
}

// TestSetMITMRootsConcurrentWithDialing pins the root pool as guarded shared
// state. SetMITMRoots is public and documented as usable while the proxy serves,
// and every forward-leg handshake reads the pool; storing it by writing the
// transport's TLSClientConfig instead raced both the transport's per-dial clone
// and the upgrade splice's Clone. Two goroutines hammer the writer and the
// handshake-side reader for a fixed number of rounds, so under -race the overlap
// is dense enough to be caught rather than sampled. Without the lock the reader
// can also observe a pool from the other side of a swap mid-handshake.
func TestSetMITMRootsConcurrentWithDialing(t *testing.T) {
	ca := mustCA(t)
	st := store.New("http://127.0.0.1:1", "", "t", &stats.Stats{})
	p, err := New(Config{
		ListenAddr: "127.0.0.1:0",
		Upstream:   "https://unused.invalid",
		AgentName:  "claude",
		Store:      st,
		CA:         ca,
	})
	if err != nil {
		t.Fatal(err)
	}
	poolA, poolB := x509.NewCertPool(), x509.NewCertPool()

	const rounds = 20000
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for range rounds {
			p.SetMITMRoots(poolA)
			p.SetMITMRoots(poolB)
		}
	}()
	go func() {
		defer wg.Done()
		for range rounds {
			if roots := p.mitmTLSDialConfig("api.example.com:443").RootCAs; roots != nil && roots != poolA && roots != poolB {
				t.Errorf("handshake read a pool that was never installed")
				return
			}
		}
	}()
	wg.Wait()
}

// TestSetMITMRootsDuringForwarding drives real forward-leg traffic from several
// goroutines while the root pool is swapped underneath. Both pools trust the
// self-signed test upstream, so every handshake must still verify: a swap must
// neither fail a request in flight nor leave a stale pool installed.
func TestSetMITMRootsDuringForwarding(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "upstream-ok")
	}))
	defer upstream.Close()

	ca := mustCA(t)
	st := store.New("http://127.0.0.1:1", "", "t", &stats.Stats{})
	p, err := New(Config{
		ListenAddr: "127.0.0.1:0",
		Upstream:   "https://unused.invalid",
		AgentName:  "claude",
		Store:      st,
		CA:         ca,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Two pools that both verify the self-signed test upstream, so a swap
	// mid-flight cannot turn into a failed handshake.
	poolA, poolB := x509.NewCertPool(), x509.NewCertPool()
	poolA.AddCert(upstream.Certificate())
	poolB.AddCert(upstream.Certificate())
	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(ca.CertPEM()) {
		t.Fatal("could not add msc CA to client pool")
	}
	p.SetMITMRoots(poolA)

	addr, err := p.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer p.Shutdown(context.Background())

	proxyURL, _ := url.Parse("http://" + addr)
	client := &http.Client{
		Transport: &http.Transport{
			Proxy:           http.ProxyURL(proxyURL),
			TLSClientConfig: &tls.Config{RootCAs: caPool},
		},
		Timeout: 10 * time.Second,
	}

	stop := make(chan struct{})
	errs := make(chan error, 64)
	var wg, swapperWG sync.WaitGroup
	swapperWG.Add(1)
	go func() {
		defer swapperWG.Done()
		for {
			select {
			case <-stop:
				return
			default:
				p.SetMITMRoots(poolA)
				p.SetMITMRoots(poolB)
			}
		}
	}()
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 10 {
				// Close idle connections so the next request re-dials and
				// actually reads the pool instead of reusing a pooled conn.
				p.mitmTransport.CloseIdleConnections()
				resp, err := client.Get(upstream.URL + "/v1/messages")
				if err != nil {
					errs <- err
					return
				}
				body, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				if string(body) != "upstream-ok" {
					errs <- fmt.Errorf("body = %q", body)
					return
				}
			}
		}()
	}
	wg.Wait()
	close(stop)
	swapperWG.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("forward leg failed while roots were being swapped: %v", err)
	}
	st.Drain()
}

// TestTunnelLogLinesCarryRequestID pins the correlation contract on the tunnel
// path. Every line handleConnect, blindTunnel, and copyTunnel emit happens
// before the tunnel serves a request of its own, so the CONNECT's correlation ID
// is the only thing tying a failed tunnel to the agent turn that opened it.
func TestTunnelLogLinesCarryRequestID(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)

	// Dialing an unreachable target fails before any byte moves, and the
	// failure line is the one an operator has to be able to attribute. A real
	// TCP pair, not net.Pipe: the pipe is unbuffered, so blindTunnel's write
	// would block with no reader.
	upLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer upLn.Close()
	clientConn, err := net.Dial("tcp", upLn.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer clientConn.Close()
	go func() { io.Copy(io.Discard, clientConn) }() // drain whatever blindTunnel writes

	(&Proxy{}).blindTunnel(clientConn, "127.0.0.1:1", "req-42")

	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		if line == "" {
			continue
		}
		var entry struct {
			Msg       string `json:"msg"`
			RequestID string `json:"request_id"`
		}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("log line is not valid JSON: %v (%s)", err, line)
		}
		if entry.Msg != "mitm: blind-tunnel dial failed" {
			continue
		}
		if entry.RequestID != "req-42" {
			t.Errorf("tunnel log line lost the CONNECT's request_id: %s", line)
		}
		return
	}
	t.Fatalf("blind-tunnel dial failure was not logged: %s", logs.String())
}
