package proxy

import (
	"crypto/tls"
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

func (l *singleConnListener) Close() error { l.signalDone(); return nil }
func (l *singleConnListener) Addr() net.Addr { return l.conn.LocalAddr() }
func (l *singleConnListener) signalDone()  { l.closed.Do(func() { close(l.done) }) }

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
