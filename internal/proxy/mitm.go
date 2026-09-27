package proxy

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/maci0/muninn-sidecar/internal/reqid"
)

// tunnelDialTimeout bounds the upstream dial for both the splice-upgrade and
// blind-tunnel paths, so a black-hole target can't hang a hijacked connection's
// goroutine indefinitely.
const tunnelDialTimeout = 30 * time.Second

// handshakeTimeout bounds the TLS handshake on a hijacked CONNECT tunnel. Once
// the connection is hijacked the http.Server no longer owns it, so its
// ReadHeaderTimeout no longer applies: a client that opens CONNECT and then
// stalls (or sends no ClientHello at all) would otherwise pin the handling
// goroutine and both sockets forever. Copied into Proxy.handshakeTimeout at
// construction so the value is fixed before any handler goroutine can read it.
const handshakeTimeout = 30 * time.Second

// upgradeHandshakeTimeout bounds the wait for the backend's reply to a spliced
// upgrade request. tunnelDialTimeout bounds reaching the backend, but a backend
// that accepts the connection and never answers would otherwise pin the hijacked
// client conn, the backend conn, and the serving goroutine forever. Copied into
// Proxy.upgradeHandshakeTimeout at construction, like handshakeTimeout.
const upgradeHandshakeTimeout = 30 * time.Second

// tunnelIdleTimeout bounds the gap between two chunks flowing through a
// hijacked tunnel. Once a CONNECT tunnel is hijacked the http.Server no longer
// owns the socket, so neither its ReadTimeout nor IdleTimeout applies: a peer
// that stops sending without closing (a stalled backend, a client that
// finishes its turn and leaves the socket open) would otherwise pin two
// goroutines and two sockets for the life of the process. A tunnel that is
// genuinely idle longer than this is not carrying a request, so tearing it
// down costs the client one reconnect.
const tunnelIdleTimeout = 5 * time.Minute

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
	clientConn, clientBuf, err := hj.Hijack()
	if err != nil {
		slog.Debug("mitm: hijack failed", "target", target, "err", err)
		return
	}
	defer clientConn.Close()

	// A client may pipeline bytes (e.g. the TLS ClientHello) right after the
	// CONNECT header without waiting for the 200; the server's bufio reader has
	// already consumed them, so drain them ahead of the raw conn or the
	// handshake/tunnel would stall waiting for data that never re-arrives.
	if n := clientBuf.Reader.Buffered(); n > 0 {
		pending, _ := clientBuf.Reader.Peek(n) // never fails for already-buffered bytes
		clientConn = &prefixConn{
			Conn: clientConn,
			r:    io.MultiReader(bytes.NewReader(append([]byte(nil), pending...)), clientConn),
		}
	}

	// Scope interception: only TLS-terminate hosts we care about (the agent's LLM
	// API). Everything else is blind-tunneled untouched, so package registries,
	// OAuth, and cert-pinned services keep working and aren't needlessly decrypted.
	if !p.shouldInterceptHost(stripPort(target)) {
		p.blindTunnel(clientConn, target)
		return
	}

	if _, err := clientConn.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n")); err != nil {
		slog.Debug("mitm: could not confirm tunnel to client", "target", target, "err", err)
		return
	}

	tlsConn := tls.Server(clientConn, &tls.Config{
		MinVersion: tls.VersionTLS12,
		GetCertificate: func(chi *tls.ClientHelloInfo) (*tls.Certificate, error) {
			// The leaf is minted for the host the client opened the tunnel to, not
			// for whatever name the ClientHello happens to carry. Without this
			// check the CA (which every agent msc launches is told to trust) signs
			// a valid certificate for any name on demand, so any client reaching
			// the proxy could present a cert for a different session's upstream.
			if name := chi.ServerName; name != "" && !sameHost(name, stripPort(target)) {
				return nil, fmt.Errorf("mitm: SNI %q does not match tunnel target %q", name, stripPort(target))
			}
			return p.ca.LeafFor(stripPort(target))
		},
	})
	if err := clientConn.SetReadDeadline(time.Now().Add(p.handshakeTimeout)); err != nil {
		slog.Debug("mitm: could not set handshake deadline", "target", target, "err", err)
		return
	}
	if err := tlsConn.Handshake(); err != nil {
		slog.Debug("mitm: TLS handshake failed", "target", target, "err", err)
		return
	}
	// Drop the deadline: the tunnel is now a long-lived connection whose reads
	// are bounded by the http.Server serving it (ReadHeaderTimeout/ReadTimeout).
	if err := clientConn.SetReadDeadline(time.Time{}); err != nil {
		slog.Debug("mitm: could not clear handshake deadline", "target", target, "err", err)
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
		// One CONNECT can carry many requests over a keep-alive tunnel, and these
		// are new requests built by the tunnel's own http.Server, so they arrive
		// with the outer tunnel's context and nothing else. Mint per request, the
		// same way the plain path does in ServeHTTP, so a log line from inside a
		// tunnel can be tied to its turn like any other.
		req = req.WithContext(withRequestID(req.Context(), nextRequestID()))

		if p.stats != nil {
			p.stats.Requests.Add(1)
		}

		// Protocol upgrades (e.g. codex ChatGPT-mode streams responses over a
		// WebSocket) can't be routed through the capturing reverse-proxy — it
		// errors on the 101. Splice those raw to the backend so the agent works;
		// the tap decodes codex's Responses envelope and stores it, but any other
		// upgrade is spliced without capture.
		if isUpgradeRequest(req) {
			p.spliceUpgrade(w, req, target)
			return
		}
		// Decrypted request: give it an absolute URL targeting the real host so
		// the shared pipeline and the reverse proxy treat it like the plain path.
		req.URL.Scheme = "https"
		req.URL.Host = target
		w = newIdleDeadlineWriter(w, p.writeIdleTimeout)
		ir, proceed := p.instrument(w, req, p.now())
		if !proceed {
			return
		}
		rp.ServeHTTP(w, ir)
	})

	// Serve HTTP/1.x (incl. keep-alive) over the single decrypted connection.
	// WriteTimeout is 0 for the reason in Proxy.New: the per-write idle bound
	// installed by the handler above is what a streaming turn needs, and an
	// absolute one would truncate a long turn mid-stream.
	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 30 * time.Second,
		ReadTimeout:       5 * time.Minute,
	}
	// Serve returns once the tunnel conn closes, so the returned error is
	// always a shutdown outcome rather than a fault. Log anything else (an
	// accept or parse failure) at debug: the tunnel is torn down either way,
	// but a dropped reason here is the only trace of a failed MITM exchange.
	if err := srv.Serve(newSingleConnListener(tlsConn)); err != nil && !errors.Is(err, net.ErrClosed) {
		slog.Debug("mitm: tunnel server stopped", "target", target, "err", err)
	}
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
// agent working; the tap in startWSTap captures the codex Responses envelope
// (see wsExchange) and passes every other protocol through undecoded.
func (p *Proxy) spliceUpgrade(w http.ResponseWriter, req *http.Request, target string) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		writeJSONError(w, http.StatusInternalServerError, "proxy: upgrade not supported")
		return
	}
	clientConn, clientBuf, err := hj.Hijack()
	if err != nil {
		slog.Debug("mitm: upgrade hijack failed", reqid.Field, requestID(req.Context()), "target", target, "err", err)
		return
	}
	defer clientConn.Close()

	cfg := p.mitmTLSDialConfig(target)
	// Bound the dial so a black-hole upgrade target can't hang this goroutine and
	// its hijacked connection indefinitely (mirrors blindTunnel's DialTimeout).
	backend, err := tls.DialWithDialer(&net.Dialer{Timeout: tunnelDialTimeout}, "tcp", target, cfg)
	if err != nil {
		slog.Debug("mitm: upgrade backend dial failed", reqid.Field, requestID(req.Context()), "target", target, "err", err)
		writeStatus(clientConn, req.Context(), target, "502 Bad Gateway")
		return
	}
	defer backend.Close()

	// Forward the original upgrade request verbatim (origin-form URI + all
	// headers, incl. Upgrade/Connection/Sec-WebSocket-*).
	if err := req.Write(backend); err != nil {
		slog.Debug("mitm: upgrade request write failed", reqid.Field, requestID(req.Context()), "target", target, "err", err)
		return
	}
	slog.Debug("mitm: splicing upgrade", reqid.Field, requestID(req.Context()), "target", target, "proto", req.Header.Get("Upgrade"))
	if p.stats != nil {
		p.stats.Upgraded.Add(1)
	}

	// Forward both directions verbatim; tap a best-effort copy to decode the
	// WebSocket-framed exchange (forwarding is never blocked by capture).
	// clientBuf may hold bytes already read past the upgrade request.
	p.spliceWithCapture(clientConn, clientBuf.Reader, backend, target, requestID(req.Context()))
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
	upstream, err := net.DialTimeout("tcp", target, tunnelDialTimeout)
	if err != nil {
		slog.Debug("mitm: blind-tunnel dial failed", "target", target, "err", err)
		writeStatus(clientConn, nil, target, "502 Bad Gateway")
		return
	}
	defer upstream.Close()

	if _, err := clientConn.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n")); err != nil {
		slog.Debug("mitm: could not confirm tunnel to client", "target", target, "err", err)
		return
	}
	slog.Debug("mitm: blind-tunnel", "target", target)

	// Pipe both directions; return once both sides finish. The CloseWrite
	// half-close unblocks the peer copy, so waiting for both cannot deadlock,
	// and a half-closing client still receives the server's full response.
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) {
		// A copy error here is the normal end of one direction (a peer closing
		// mid-stream); log it so a tunnel that dies for any other reason leaves
		// a trace instead of ending silently.
		if err := copyTunnel(dst, src, src, target); err != nil {
			slog.Debug("mitm: tunnel copy ended with an error", "target", target, "err", err)
		}
		// Unblock the peer copy: a half-close lets the other direction drain.
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			cw.CloseWrite()
		}
		done <- struct{}{}
	}
	go cp(upstream, clientConn)
	go cp(clientConn, upstream)
	<-done
	<-done
}

// writeStatus sends a bare status line to a hijacked client, for the paths
// where the CONNECT could not be honored and the request never reaches an
// http.ResponseWriter. A failed write means the client is already gone; it is
// still worth a trace, since the agent sees only a closed socket and the log
// is the only record of why the tunnel was refused.
func writeStatus(clientConn net.Conn, ctx context.Context, target, status string) {
	if _, err := fmt.Fprintf(clientConn, "HTTP/1.1 %s\r\nContent-Length: 0\r\n\r\n", status); err != nil {
		slog.Debug("mitm: could not report tunnel failure to client",
			reqid.Field, requestID(ctx), "target", target, "status", status, "err", err)
	}
}

// copyTunnel pipes r (reads taken from src, which is normally src itself or a
// buffered reader over it) into dst, re-arming tunnelIdleTimeout on the read and
// the write side before every chunk. A deadline error is returned like any other
// copy error; the caller logs it and closes its half.
func copyTunnel(dst net.Conn, src net.Conn, r io.Reader, target string) error {
	buf := make([]byte, 32*1024)
	for {
		if err := src.SetReadDeadline(time.Now().Add(tunnelIdleTimeout)); err != nil {
			slog.Debug("mitm: could not set tunnel read deadline", "target", target, "err", err)
		}
		if err := dst.SetWriteDeadline(time.Now().Add(tunnelIdleTimeout)); err != nil {
			slog.Debug("mitm: could not set tunnel write deadline", "target", target, "err", err)
		}
		n, rerr := r.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return fmt.Errorf("write to tunnel peer: %w", werr)
			}
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				return nil
			}
			return fmt.Errorf("read from tunnel peer: %w", rerr)
		}
	}
}

// prefixConn is a net.Conn whose reads drain r (buffered bytes the HTTP server
// consumed past the CONNECT header, chained ahead of the conn) instead of the
// conn directly. Writes and everything else pass through.
type prefixConn struct {
	net.Conn
	r io.Reader
}

func (c *prefixConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// CloseWrite forwards a half-close to the wrapped conn so tunnel splices can
// drain the opposite direction. No-op if unsupported.
func (c *prefixConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}

// stripPort returns host without a trailing :port, unwrapping the brackets
// from an IPv6 literal and leaving bare hosts intact.
func stripPort(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return strings.TrimSuffix(strings.TrimPrefix(hostport, "["), "]")
}

// sameHost reports whether two host names name the same endpoint, comparing
// them the way DNS does: case-insensitively, and treating the DNS root's
// explicit trailing dot as the same name ("API.OpenAI.com." == "api.openai.com").
func sameHost(a, b string) bool {
	canon := func(h string) string {
		return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(h)), ".")
	}
	return canon(a) == canon(b)
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

// CloseWrite forwards a half-close to the wrapped conn (e.g. tls.Conn) so
// tunnel splices can drain the opposite direction. No-op if unsupported.
func (c *notifyConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}

var _ net.Listener = (*singleConnListener)(nil)
