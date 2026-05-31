package proxy

import (
	"crypto/tls"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// handleConnect terminates a CONNECT tunnel and intercepts its TLS traffic. The
// agent (configured with HTTPS_PROXY pointing at msc, and trusting msc's CA)
// sends `CONNECT host:443`; msc replies 200, completes a TLS handshake using a
// leaf cert minted for the requested host, then serves the decrypted HTTP
// requests through the same instrument → forward pipeline as the plain proxy,
// re-originating TLS to the real host. This catches agents that ignore a
// base-URL env override (codex ChatGPT-mode, grok session auth, agy).
func (p *Proxy) handleConnect(w http.ResponseWriter, r *http.Request) {
	target := r.Host // "host:port"
	if target == "" {
		target = r.URL.Host
	}

	hj, ok := w.(http.Hijacker)
	if !ok {
		writeJSONError(w, http.StatusInternalServerError, "proxy: CONNECT not supported")
		return
	}
	clientConn, _, err := hj.Hijack()
	if err != nil {
		slog.Debug("mitm: hijack failed", "target", target, "err", err)
		return
	}
	defer clientConn.Close()

	// Scope interception: only TLS-terminate hosts we care about (the agent's LLM
	// API). Everything else is blind-tunneled untouched, so package registries,
	// OAuth, and cert-pinned services keep working and aren't needlessly decrypted.
	if !p.shouldInterceptHost(stripPort(target)) {
		p.blindTunnel(clientConn, target)
		return
	}

	if _, err := clientConn.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n")); err != nil {
		return
	}

	tlsConn := tls.Server(clientConn, &tls.Config{
		MinVersion: tls.VersionTLS12,
		GetCertificate: func(chi *tls.ClientHelloInfo) (*tls.Certificate, error) {
			name := chi.ServerName
			if name == "" {
				name = stripPort(target)
			}
			return p.ca.LeafFor(name)
		},
	})
	if err := tlsConn.Handshake(); err != nil {
		slog.Debug("mitm: TLS handshake failed", "target", target, "err", err)
		return
	}
	slog.Debug("mitm: intercepting tunnel", "target", target, "sni", tlsConn.ConnectionState().ServerName)

	// One reverse proxy per tunnel, forwarding to the real host over TLS.
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = "https"
			pr.Out.URL.Host = target
			pr.Out.Host = stripPort(target)
		},
		Transport:      p.mitmTransport,
		ModifyResponse: p.captureResponse,
		ErrorHandler:   p.errorHandler,
		FlushInterval:  -1,
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		// Protocol upgrades (e.g. codex ChatGPT-mode streams responses over a
		// WebSocket) can't be routed through the capturing reverse-proxy — it
		// errors on the 101. Splice those raw to the backend so the agent works;
		// the upgraded stream isn't captured (yet).
		if isUpgradeRequest(req) {
			p.spliceUpgrade(w, req, target)
			return
		}
		// Decrypted request: give it an absolute URL targeting the real host so
		// the shared pipeline and the reverse proxy treat it like the plain path.
		req.URL.Scheme = "https"
		req.URL.Host = target
		ir, proceed := p.instrument(w, req, time.Now())
		if !proceed {
			return
		}
		rp.ServeHTTP(w, ir)
	})

	// Serve HTTP/1.x (incl. keep-alive) over the single decrypted connection.
	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 30 * time.Second,
		ReadTimeout:       5 * time.Minute,
		WriteTimeout:      10 * time.Minute,
	}
	_ = srv.Serve(newSingleConnListener(tlsConn)) // returns once the tunnel conn closes
}

// isUpgradeRequest reports whether req is an HTTP protocol upgrade (WebSocket
// etc.): an "Upgrade" header plus a "Connection: Upgrade" token (case-insensitive).
func isUpgradeRequest(req *http.Request) bool {
	if req.Header.Get("Upgrade") == "" {
		return false
	}
	for _, v := range req.Header.Values("Connection") {
		for _, tok := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(tok), "upgrade") {
				return true
			}
		}
	}
	return false
}

// spliceUpgrade handles an intercepted protocol-upgrade request by re-originating
// TLS to the real backend and copying bytes verbatim in both directions. The
// capturing reverse-proxy can't drive a 101 upgrade under MITM, so this keeps the
// agent working at the cost of not capturing the upgraded stream.
func (p *Proxy) spliceUpgrade(w http.ResponseWriter, req *http.Request, target string) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		writeJSONError(w, http.StatusInternalServerError, "proxy: upgrade not supported")
		return
	}
	clientConn, clientBuf, err := hj.Hijack()
	if err != nil {
		slog.Debug("mitm: upgrade hijack failed", "target", target, "err", err)
		return
	}
	defer clientConn.Close()

	cfg := p.mitmTransport.TLSClientConfig.Clone()
	cfg.ServerName = stripPort(target)
	// Bound the dial so a black-hole upgrade target can't hang this goroutine and
	// its hijacked connection indefinitely (mirrors blindTunnel's DialTimeout).
	backend, err := tls.DialWithDialer(&net.Dialer{Timeout: 30 * time.Second}, "tcp", target, cfg)
	if err != nil {
		slog.Debug("mitm: upgrade backend dial failed", "target", target, "err", err)
		clientConn.Write([]byte("HTTP/1.1 502 Bad Gateway\r\n\r\n"))
		return
	}
	defer backend.Close()

	// Forward the original upgrade request verbatim (origin-form URI + all
	// headers, incl. Upgrade/Connection/Sec-WebSocket-*).
	if err := req.Write(backend); err != nil {
		slog.Debug("mitm: upgrade request write failed", "target", target, "err", err)
		return
	}
	slog.Debug("mitm: splicing upgrade", "target", target, "proto", req.Header.Get("Upgrade"))
	if p.stats != nil {
		p.stats.Upgraded.Add(1)
	}

	// Forward both directions verbatim; tap a best-effort copy to decode the
	// WebSocket-framed exchange (forwarding is never blocked by capture).
	// clientBuf may hold bytes already read past the upgrade request.
	p.spliceWithCapture(clientConn, clientBuf.Reader, backend, target)
}

// shouldInterceptHost reports whether a CONNECT target host should be
// TLS-terminated (vs blind-tunneled). True for the upstream host, any configured
// MITMHosts, or everything when "*" was configured. host must be port-stripped.
func (p *Proxy) shouldInterceptHost(host string) bool {
	if p.mitmAll {
		return true
	}
	return p.mitmHosts[strings.ToLower(host)]
}

// blindTunnel forwards an opaque TCP stream between the client and the real
// target without touching TLS — a plain CONNECT proxy. Used for hosts we don't
// intercept. The 200 is sent only after the upstream dial succeeds so the client
// sees a real failure if the host is unreachable.
func (p *Proxy) blindTunnel(clientConn net.Conn, target string) {
	upstream, err := net.DialTimeout("tcp", target, 30*time.Second)
	if err != nil {
		slog.Debug("mitm: blind-tunnel dial failed", "target", target, "err", err)
		clientConn.Write([]byte("HTTP/1.1 502 Bad Gateway\r\n\r\n"))
		return
	}
	defer upstream.Close()

	if _, err := clientConn.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n")); err != nil {
		return
	}
	slog.Debug("mitm: blind-tunnel", "target", target)

	// Pipe both directions; return when either side closes.
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) {
		io.Copy(dst, src)
		// Unblock the peer copy: a half-close lets the other direction drain.
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			cw.CloseWrite()
		}
		done <- struct{}{}
	}
	go cp(upstream, clientConn)
	go cp(clientConn, upstream)
	<-done
}

// stripPort returns host without a trailing :port, leaving bracketed IPv6 and
// bare hosts intact.
func stripPort(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return strings.TrimSuffix(strings.TrimPrefix(hostport, "["), "]")
}

// singleConnListener adapts one already-accepted net.Conn into a net.Listener so
// http.Server.Serve can drive request parsing and keep-alive over it. Accept
// yields the (close-notifying) conn once; the second Accept blocks until that
// conn closes — when the tunnel ends, http.Server closes it, which unblocks
// Accept with an error so Serve returns and handleConnect can clean up.
type singleConnListener struct {
	conn     net.Conn
	done     chan struct{}
	closed   sync.Once
	accepted atomic.Bool
}

func newSingleConnListener(c net.Conn) *singleConnListener {
	l := &singleConnListener{done: make(chan struct{})}
	l.conn = &notifyConn{Conn: c, onClose: l.signalDone}
	return l
}

func (l *singleConnListener) Accept() (net.Conn, error) {
	if l.accepted.CompareAndSwap(false, true) {
		return l.conn, nil
	}
	<-l.done
	return nil, net.ErrClosed
}

func (l *singleConnListener) Close() error   { l.signalDone(); return nil }
func (l *singleConnListener) Addr() net.Addr { return l.conn.LocalAddr() }
func (l *singleConnListener) signalDone()    { l.closed.Do(func() { close(l.done) }) }

// notifyConn calls onClose exactly once when the connection is closed, so the
// listener learns the served connection has ended.
type notifyConn struct {
	net.Conn
	once    sync.Once
	onClose func()
}

func (c *notifyConn) Close() error {
	c.once.Do(c.onClose)
	return c.Conn.Close()
}

var _ net.Listener = (*singleConnListener)(nil)
