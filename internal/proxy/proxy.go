// Package proxy implements a transparent reverse proxy that captures
// LLM API traffic for MuninnDB.
package proxy

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/maci0/muninn-sidecar/internal/apiformat"
	"github.com/maci0/muninn-sidecar/internal/inject"
	"github.com/maci0/muninn-sidecar/internal/mitm"
	"github.com/maci0/muninn-sidecar/internal/reqid"
	"github.com/maci0/muninn-sidecar/internal/stats"
	"github.com/maci0/muninn-sidecar/internal/store"
)

// Storer enqueues a captured exchange for async delivery to MuninnDB.
// A nil Storer discards captures. Implemented by *store.MuninnStore.
type Storer interface {
	Store(*store.CapturedExchange)
}

// maxStreamBuf caps the incremental SSE line buffer to prevent OOM.
// Partial lines exceeding this limit are dropped (logged at warn level).
const maxStreamBuf = 1 << 20 // 1 MiB

// maxTextAccum caps accumulated assistant text from SSE deltas.
const maxTextAccum = 16 << 10 // 16 KiB

// maxDecompressSize caps gzip decompression to prevent gzip-bomb OOM attacks
// from a malicious or compromised upstream. If exceeded, the compressed body
// is served unchanged.
const maxDecompressSize = 50 << 20 // 50 MiB

// maxRequestBodySize caps request body buffering to prevent OOM from a
// malicious or misbehaving agent. Requests exceeding this limit are rejected
// with 413.
const maxRequestBodySize = 50 << 20 // 50 MiB

// maxNonStreamBodySize caps non-streaming response body buffering. Responses
// exceeding this limit are rejected to prevent OOM from a compromised upstream.
const maxNonStreamBodySize = 50 << 20 // 50 MiB

// StatusPath is the local status endpoint served by the proxy, e.g.
// `curl http://127.0.0.1:<port>/__msc/health`. It answers "is the sidecar up,
// what has it captured, how long are the turns taking, and is anything failing"
// without the operator having to read the log stream. The path is under a
// reserved prefix no agent API uses, and it is served before the capture
// pipeline so it is never forwarded upstream or stored as a memory.
//
// The status code is liveness only: the endpoint does not probe MuninnDB, so a
// MuninnDB outage cannot fail it, and a sidecar that is up but not saving
// memories is better diagnosed than restarted. What it cannot answer in the
// status code it answers in the body — "degraded" with the reasons (see
// serveStatus). Reachability is checked once at startup (msc refuses to launch
// without --force) and later failures show up as save errors in the snapshot.
const StatusPath = "/__msc/health"

// Proxy is a transparent reverse proxy that sits between a coding agent and
// its LLM API upstream. All traffic is forwarded, but only requests matching
// CapturePaths are recorded to MuninnDB (asynchronously). The agent sees the
// proxy as the real API because we override its base-URL env var (e.g.
// ANTHROPIC_BASE_URL) to point here.
//
// Streaming (SSE) responses are handled specially: the body is wrapped so
// chunks flow through to the agent in real-time while text deltas and tool
// names are accumulated from the stream to build a synthetic Anthropic-format
// response, falling back to the last data line only if no text deltas or tool
// names are captured.
type Proxy struct {
	listenAddr     string                 // resolved after Start() when port is :0
	upstream       *url.URL               // real LLM API (e.g. https://api.anthropic.com)
	agentName      string                 // "claude", "codex", etc. — used for tagging
	store          Storer                 // async MuninnDB writer
	capturePaths   []string               // path substrings to capture; empty = capture all
	excludePaths   []string               // path substrings to exclude from capture (checked first)
	filterPatterns []string               // tool name patterns to strip from stored bodies; empty non-nil = no filtering
	injector       *inject.Injector       // optional memory injector (nil = disabled)
	server         *http.Server           // underlying HTTP server
	reverseProxy   *httputil.ReverseProxy // stdlib reverse proxy with our hooks
	ca             *mitm.CA               // non-nil enables TLS-MITM of CONNECT tunnels
	mitmTransport  *http.Transport        // TLS transport to real upstream hosts (MITM)
	mitmHosts      map[string]bool        // hosts to TLS-terminate; others are blind-tunneled
	mitmAll        bool                   // intercept every CONNECT host (allowlist contained "*")
	stats          *stats.Stats           // optional session counters (nil = no recording)
	clock          Clock                  // source of capture timestamps and durations
	started        time.Time              // when Start() began serving (status endpoint)

	// Deadlines for the pre-request half of a hijacked tunnel, fixed at
	// construction so no goroutine ever reads a value another one can change.
	// Both cover a peer that has been reached but never answers; both are
	// cleared once the handshake completes.
	handshakeTimeout        time.Duration // CONNECT tunnel: client-side TLS handshake
	upgradeHandshakeTimeout time.Duration // spliced upgrade: backend's reply
	writeIdleTimeout        time.Duration // gap between response writes after which the agent's conn is cut

	// mitmRoots holds the pool the MITM forward leg verifies real upstreams
	// with, guarded because SetMITMRoots can be called while the proxy serves
	// and every upstream dial reads it. It is applied through the transport's
	// DialTLSContext rather than by mutating the transport's TLSClientConfig:
	// http.Transport clones that config on every dial from its own goroutines,
	// so writing RootCAs into it would race with those clones.
	mitmRootsMu sync.RWMutex
	mitmRoots   *x509.CertPool
}

// Config holds the parameters for creating a Proxy.
type Config struct {
	ListenAddr     string           // e.g. "127.0.0.1:0" for random port
	Upstream       string           // real API URL to forward to
	AgentName      string           // agent name for tagging in MuninnDB
	Store          Storer           // MuninnDB writer; nil = discard captures
	CapturePaths   []string         // path substrings to capture; empty = capture all
	ExcludePaths   []string         // path substrings to exclude from capture (checked first)
	FilterPatterns []string         // tool name patterns to strip; nil = defaultFilterPatterns; []string{} = disable all filtering
	Injector       *inject.Injector // optional memory injector; nil = disabled
	CA             *mitm.CA         // non-nil enables TLS-MITM: CONNECT tunnels are terminated and intercepted
	MITMHosts      []string         // extra hosts to TLS-terminate (besides the upstream host); "*" intercepts all. Others are blind-tunneled untouched.
	Stats          *stats.Stats     // optional session counters (e.g. MITM upgraded-stream count)
	Clock          Clock            // source of capture timestamps/durations; nil = SystemClock
}

// New creates a Proxy. Use ListenAddr "127.0.0.1:0" in Config to bind to a
// random available port. The actual address is available via ListenAddr()
// after Start().
func New(cfg Config) (*Proxy, error) {
	upstream, err := url.Parse(cfg.Upstream)
	if err != nil {
		return nil, fmt.Errorf("invalid upstream URL %q: %w", cfg.Upstream, err)
	}

	filterPatterns := cfg.FilterPatterns
	if filterPatterns == nil {
		filterPatterns = defaultFilterPatterns
	}

	clock := cfg.Clock
	if clock == nil {
		clock = SystemClock{}
	}

	p := &Proxy{
		listenAddr:     cfg.ListenAddr,
		upstream:       upstream,
		agentName:      cfg.AgentName,
		store:          cfg.Store,
		capturePaths:   toLowerSlice(cfg.CapturePaths),
		excludePaths:   toLowerSlice(cfg.ExcludePaths),
		filterPatterns: toLowerSlice(filterPatterns),
		injector:       cfg.Injector,
		ca:             cfg.CA,
		stats:          cfg.Stats,
		clock:          clock,

		handshakeTimeout:        handshakeTimeout,
		upgradeHandshakeTimeout: upgradeHandshakeTimeout,
		writeIdleTimeout:        writeIdleTimeout,
	}

	// MITM defaults to intercepting every CONNECT host: that's the whole point —
	// catch agents that talk to unexpected hosts (e.g. codex ChatGPT-mode hits a
	// backend that isn't its resolved api.openai.com upstream). Passing MITMHosts
	// opts into scoping: only the upstream host plus the listed hosts are
	// TLS-terminated, and everything else is blind-tunneled untouched (so package
	// registries and cert-pinned services keep working). "*" forces intercept-all.
	p.mitmHosts = map[string]bool{}
	if len(cfg.MITMHosts) == 0 {
		p.mitmAll = true
	} else {
		if h := strings.ToLower(upstream.Hostname()); h != "" {
			p.mitmHosts[h] = true
		}
		for _, h := range cfg.MITMHosts {
			if h == "*" {
				p.mitmAll = true
				continue
			}
			p.mitmHosts[strings.ToLower(strings.TrimSpace(h))] = true
		}
	}

	// Upstream transport timeouts. ResponseHeaderTimeout only bounds the wait
	// for the response headers, never the body, so a long SSE/stream response is
	// unaffected; without it an upstream that accepts the connection and then
	// goes quiet pins the agent's turn and the proxy's connection forever.
	// 5 minutes covers a slow-to-first-byte LLM endpoint while still failing
	// instead of hanging.
	dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		DialContext:           dialer.DialContext,
		TLSHandshakeTimeout:   30 * time.Second,
		ResponseHeaderTimeout: 5 * time.Minute,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS13},
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   100,
		IdleConnTimeout:       90 * time.Second,
	}
	// Separate transport for MITM forwarding to arbitrary real hosts. TLS1.2 floor
	// (some upstreams still require it) with normal cert verification of the real
	// server — msc only forges the agent-facing side, never trusts a bad upstream.
	// The TLS handshake is driven by dialMITMTLS (not by the transport's own
	// TLSClientConfig) so the verification roots can be swapped at runtime
	// without racing the transport's per-dial clone.
	p.mitmTransport = &http.Transport{
		DialContext:           dialer.DialContext,
		DialTLSContext:        p.dialMITMTLS,
		TLSHandshakeTimeout:   30 * time.Second,
		ResponseHeaderTimeout: 5 * time.Minute,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   100,
		IdleConnTimeout:       90 * time.Second,
	}

	// Hand the store its capture-side normalization so the heavy body parsing
	// happens on the store's worker, not on the agent's request path. Installed
	// here, before New returns, so it is in place before any capture is queued.
	if s, ok := cfg.Store.(preparerSetter); ok {
		s.SetPreparer(p.prepareExchange)
	}

	p.reverseProxy = &httputil.ReverseProxy{
		// Rewrite instead of Director: Director silently appends
		// X-Forwarded-For headers, which leaks the proxy's presence to
		// the upstream API. Rewrite gives full control and keeps requests
		// byte-identical to what the agent SDK would normally send.
		Rewrite:        p.rewrite,
		Transport:      transport,
		ModifyResponse: p.captureResponse,
		ErrorHandler:   p.errorHandler,
		// FlushInterval -1 disables buffering so SSE events stream through
		// to the agent immediately rather than being batched by the proxy.
		FlushInterval: -1,
	}

	// Long timeouts: LLM API calls routinely take 30-120s for large contexts.
	// ReadHeaderTimeout is kept short to prevent slow-header (slowloris) attacks
	// even though the server is loopback-only.
	//
	// WriteTimeout stays 0 deliberately. Go arms it once per request, when the
	// headers are read, so it is an absolute cap on the whole response, not an
	// idle bound: a turn that streams for longer than that is cut off mid-stream
	// and the agent sees an i/o timeout. Response-bound latency is unbounded
	// here (a large context with extended thinking, a queued upstream), so the
	// response leg is bounded per write instead — see idleDeadlineWriter.
	p.server = &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           p,
		ReadHeaderTimeout: 30 * time.Second,
		ReadTimeout:       5 * time.Minute,
		IdleTimeout:       120 * time.Second,
	}

	return p, nil
}

// ListenAddr returns the actual address the proxy is listening on.
func (p *Proxy) ListenAddr() string { return p.listenAddr }

// now reads the injected clock, or the system clock when none was configured.
func (p *Proxy) now() time.Time { return clockOrSystem(p.clock).Now() }

// since reads elapsed time from the injected clock, or the system clock.
func (p *Proxy) since(t time.Time) time.Duration { return clockOrSystem(p.clock).Since(t) }

// SetMITMRoots overrides the root CAs used to verify real upstream servers on
// the MITM forward leg. By default the system trust store is used; supply a
// custom pool for environments with a private upstream CA (e.g. a corporate
// egress proxy) or for tests that forward to a self-signed server. No-op when
// MITM is disabled (no CA configured).
//
// Safe to call while the proxy is serving: the pool is read under a lock by
// every forward-leg handshake, and connections already established keep the
// roots they were verified with.
func (p *Proxy) SetMITMRoots(pool *x509.CertPool) {
	if p.mitmTransport == nil {
		return
	}
	p.mitmRootsMu.Lock()
	p.mitmRoots = pool
	p.mitmRootsMu.Unlock()
}

// mitmRootCAs returns the current root pool for the MITM forward leg, or nil
// when the system trust store applies.
func (p *Proxy) mitmRootCAs() *x509.CertPool {
	p.mitmRootsMu.RLock()
	defer p.mitmRootsMu.RUnlock()
	return p.mitmRoots
}

// dialMITMTLS is the MITM forward leg's TLS handshake: a normal client dial
// (full cert verification of the real server, TLS1.2 floor) that reads the
// current root pool under the lock. It replaces the transport's own TLS setup
// so a SetMITMRoots call after Start cannot race the transport cloning its
// TLSClientConfig on another goroutine.
func (p *Proxy) dialMITMTLS(ctx context.Context, network, addr string) (net.Conn, error) {
	return tls.DialWithDialer(&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}, network, addr, p.mitmTLSDialConfig(addr))
}

// mitmTLSDialConfig builds the client TLS config for a forward-leg connection to
// target: TLS1.2 floor (some upstreams still require it) and normal verification
// of the real server — msc only forges the agent-facing side, never trusts a bad
// upstream. The root pool is read under the lock; connections already
// established keep the roots they were verified with.
func (p *Proxy) mitmTLSDialConfig(target string) *tls.Config {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: stripPort(target)}
	if roots := p.mitmRootCAs(); roots != nil {
		cfg.RootCAs = roots
	}
	return cfg
}

// Start begins listening. Returns the resolved listen address (with actual
// port if :0 was used). The server runs in a background goroutine.
func (p *Proxy) Start() (string, error) {
	ln, err := net.Listen("tcp", p.listenAddr)
	if err != nil {
		return "", err
	}
	addr := ln.Addr().String()
	p.listenAddr = addr
	p.started = p.now()

	slog.Debug("proxy listening", "addr", addr, "upstream", redactURL(p.upstream))

	go func() {
		if err := p.server.Serve(ln); err != nil && err != http.ErrServerClosed {
			slog.Error("proxy server error", "err", err, "addr", addr)
		}
	}()

	return addr, nil
}

// Shutdown gracefully stops the proxy server, allowing in-flight requests
// to complete within the given context deadline.
func (p *Proxy) Shutdown(ctx context.Context) error {
	return p.server.Shutdown(ctx)
}

// ServeHTTP is the main handler. It buffers the request body (needed for
// capture), stashes metadata in the request context, then delegates to the
// stdlib reverse proxy which calls rewrite -> upstream -> captureResponse.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Every request gets a correlation ID before anything else happens, so a
	// log line from any stage of the pipeline (inject, upstream, capture) can
	// be tied back to this one turn. It stays in the request context: the
	// forwarded request must stay byte-identical to what the agent sent.
	r = r.WithContext(withRequestID(r.Context(), nextRequestID()))

	if r.URL.Path == StatusPath && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
		p.serveStatus(w)
		return
	}

	// Everything the agent sends past this point counts as a request, including
	// the CONNECT that opens a tunnel: it is one more thing the agent asked for,
	// and the requests decrypted inside the tunnel are counted there instead.
	if p.stats != nil {
		p.stats.Requests.Add(1)
	}

	// MITM mode: an agent that routes via HTTPS_PROXY opens a tunnel with CONNECT.
	// Terminate TLS and intercept the decrypted traffic (the same pipeline).
	// CONNECT and the upgrades spliced inside a tunnel hijack the connection, so
	// they run against the raw writer; the deadline wrapper goes on after them.
	if r.Method == http.MethodConnect && p.ca != nil {
		p.handleConnect(w, r)
		return
	}

	w = newIdleDeadlineWriter(w, p.writeIdleTimeout)
	r, ok := p.instrument(w, r, p.now())
	if !ok {
		return // instrument already wrote an error response
	}
	p.reverseProxy.ServeHTTP(w, r)
}

// serveStatus answers StatusPath with the session's operational state: what
// the sidecar is proxying, how long it has been up, and the capture/injection
// counters, so an operator can tell a working sidecar from a silently failing
// one without reading logs. Non-GET methods fall through to the proxy, so a
// captured agent API path is never shadowed.
//
// The response separates liveness ("status" is "ok" whenever the handler runs,
// so the process can be restarted correctly) from health ("degraded", with the
// reasons). A sidecar that is up but not saving memories still answers 200 —
// restarting it would not help, and a load balancer that killed it would take
// the agent's API path down with it — so the failure is reported in the body
// instead of the status code.
func (p *Proxy) serveStatus(w http.ResponseWriter) {
	var snap stats.Snapshot
	if p.stats != nil {
		snap = p.stats.Snapshot()
	}
	queue := p.storeQueue()

	body := struct {
		Status   string         `json:"status"`
		Degraded bool           `json:"degraded"`
		Reasons  []string       `json:"degraded_reasons,omitempty"`
		Agent    string         `json:"agent"`
		Upstream string         `json:"upstream"`
		UptimeS  int64          `json:"uptime_s"`
		StoreQ   storeQueue     `json:"store_queue"`
		Stats    stats.Snapshot `json:"stats"`
	}{
		Status:   "ok",
		Agent:    p.agentName,
		Upstream: redactURL(p.upstream),
		StoreQ:   queue,
		Stats:    snap,
	}
	body.Reasons = degradedReasons(snap, queue)
	body.Degraded = len(body.Reasons) > 0

	if !p.started.IsZero() {
		body.UptimeS = int64(p.since(p.started).Seconds())
	}

	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	if err := enc.Encode(body); err != nil {
		// The status line is already committed; the partial body is all the agent
		// or operator gets, and a status endpoint is not worth a log line per hit.
		slog.Debug("status encoding failed", "err", err)
	}
}

// storeQueue is the live capture-queue depth reported by the status endpoint.
type storeQueue struct {
	Depth     int  `json:"depth"`     // exchanges waiting for the store worker
	Capacity  int  `json:"capacity"`  // the queue's fixed capacity
	Saturated bool `json:"saturated"` // depth has reached capacity: new captures are dropped
}

// queueReporter is the optional Storer capability that reports its queue depth.
// Asserted rather than required of Storer so a Storer that keeps no queue (a
// test double, or a store with no worker) still serves the status endpoint.
type queueReporter interface{ QueueDepth() (depth, capacity int) }

// storeQueue reports the storer queue's depth, or zeros when the storer has no
// queue to report.
func (p *Proxy) storeQueue() storeQueue {
	q, ok := p.store.(queueReporter)
	if !ok {
		return storeQueue{}
	}
	depth, capacity := q.QueueDepth()
	return storeQueue{Depth: depth, Capacity: capacity, Saturated: capacity > 0 && depth >= capacity}
}

// degradedReasons names what is stopping the sidecar from doing its job. Only
// failures on msc's own side of the pipeline count: an upstream 4xx/5xx is the
// LLM provider's answer, forwarded to the agent unchanged, and a rate-limited
// turn is not a degraded sidecar. Proxy errors (the agent got a 502) and save
// errors (memories are being lost) are, as is a saturated queue, which drops
// the next capture outright.
func degradedReasons(snap stats.Snapshot, q storeQueue) []string {
	var reasons []string
	if snap.ProxyErrors > 0 {
		reasons = append(reasons, "proxy transport errors: the agent is receiving 502 responses")
	}
	if snap.SaveErrors > 0 {
		reasons = append(reasons, "delivery errors: captured exchanges are not reaching MuninnDB")
	}
	if snap.Dropped > 0 {
		reasons = append(reasons, "dropped captures: the store queue overflowed")
	}
	if q.Saturated {
		reasons = append(reasons, "store queue is full: new captures are being dropped")
	}
	return reasons
}

// instrument applies the capture/inject pipeline shared by the plain reverse-proxy
// path and the MITM tunnel: if the path is captured, it buffers the body (within
// the size limit), enriches it with recalled memories, and stashes capture
// metadata in the request context. Returns the (possibly rewritten) request and
// whether to proceed forwarding — false means an error response was already
// written. Non-captured requests pass through untouched.
func (p *Proxy) instrument(w http.ResponseWriter, r *http.Request, start time.Time) (*http.Request, bool) {
	id := requestID(r.Context())
	capture := p.shouldCapture(r.URL.Path)
	slog.Debug("request", "request_id", id, "path", r.URL.Path, "capture", capture)
	if !capture {
		return r, true
	}

	var reqBody []byte
	if r.Body != nil {
		var err error
		reqBody, err = io.ReadAll(io.LimitReader(r.Body, maxRequestBodySize+1))
		if err != nil {
			slog.Warn("failed to read request body for capture", "request_id", id, "path", r.URL.Path, "err", err)
			writeJSONError(w, http.StatusInternalServerError, "failed to read request body")
			return r, false
		}
		if int64(len(reqBody)) > maxRequestBodySize {
			slog.Warn("request body exceeds size limit", "request_id", id, "path", r.URL.Path, "limit", maxRequestBodySize)
			writeJSONError(w, http.StatusRequestEntityTooLarge, "request body exceeds proxy size limit")
			return r, false
		}
	}

	// Enrich with recalled memories if injector is enabled.
	forwardBody := reqBody
	if p.injector != nil && len(reqBody) > 0 {
		if enriched, _ := p.injector.Enrich(r.Context(), reqBody); len(enriched) > 0 {
			forwardBody = enriched
		}
	}

	r.Body = io.NopCloser(bytes.NewReader(forwardBody))
	r.ContentLength = int64(len(forwardBody))

	ctx := &captureCtx{
		id:      id,
		start:   start,
		method:  r.Method,
		path:    r.URL.Path,
		reqBody: reqBody, // original body for capture (not enriched)
		agent:   p.agentName,
	}
	return r.WithContext(withCapture(r.Context(), ctx)), true
}

// shouldCapture returns true if the request path matches one of the
// configured CapturePaths (case-insensitive) and none of the ExcludePaths.
// Exclusions are checked first. An empty CapturePaths list means capture
// all (minus exclusions). Case-insensitivity is needed because Gemini API
// key mode uses lowercase paths (generateContent) while OAuth mode uses
// camelCase (streamGenerateContent).
func (p *Proxy) shouldCapture(path string) bool {
	lowerPath := strings.ToLower(path)
	for _, ex := range p.excludePaths {
		if strings.Contains(lowerPath, ex) {
			return false
		}
	}
	if len(p.capturePaths) == 0 {
		return true
	}
	for _, sub := range p.capturePaths {
		if strings.Contains(lowerPath, sub) {
			return true
		}
	}
	return false
}

// rewrite rewrites the request URL to point at the real upstream without
// adding any proxy-specific headers (X-Forwarded-For, etc.), so the
// request reaching the API is identical to what the agent SDK would send
// directly. This keeps the proxy fully transparent.
func (p *Proxy) rewrite(pr *httputil.ProxyRequest) {
	pr.Out.URL.Scheme = p.upstream.Scheme
	pr.Out.URL.Host = p.upstream.Host
	pr.Out.Host = p.upstream.Host

	if p.upstream.Path != "" && p.upstream.Path != "/" {
		pr.Out.URL.Path = singleJoiningSlash(p.upstream.Path, pr.Out.URL.Path)
	}

	slog.Debug("proxying", "method", pr.Out.Method, "url", redactURL(pr.Out.URL))
}

// captureResponse is called after the upstream responds. For non-streaming
// responses it reads the full body, transparently decompresses gzip (serving
// the agent an uncompressed body unless decompression fails or the decompressed
// size exceeds the limit, in which case the compressed body is served unchanged),
// captures the exchange, and re-wraps the body. For SSE/ndjson streams it wraps
// the body in a streamCapture that tees data through while accumulating text
// deltas and tool names from stream events to build a synthetic response for storage.
func (p *Proxy) captureResponse(resp *http.Response) error {
	ctx := captureFromContext(resp.Request.Context())
	if ctx == nil {
		return nil
	}

	// Protocol upgrades: ReverseProxy calls ModifyResponse on a 101 before its
	// upgrade handling, and the body is already the live upgraded connection.
	// Reading it here would block on (and then destroy) the upgraded stream, so
	// skip capture and let the reverse proxy splice the connection natively.
	if resp.StatusCode == http.StatusSwitchingProtocols {
		slog.Debug("skipping capture of protocol upgrade response", "request_id", ctx.id, "path", ctx.path)
		return nil
	}

	contentType := resp.Header.Get("Content-Type")

	// Surface upstream API failures. Without this a 401/429/5xx from the LLM
	// provider is forwarded to the agent and stored, but leaves no trace in
	// msc's own logs — the most common incident question ("why is the agent
	// erroring?") would be unanswerable. Logged at warn (operationally
	// actionable) and counted for the session summary.
	if resp.StatusCode >= 400 {
		slog.Warn("upstream error response",
			"request_id", ctx.id,
			"status", resp.StatusCode,
			"method", ctx.method,
			"path", ctx.path,
			"agent", ctx.agent,
			"duration_ms", p.since(ctx.start).Milliseconds())
		if p.stats != nil {
			p.stats.UpstreamErrors.Add(1)
		}
	}

	// gRPC responses (e.g. agy's cloudcode-pa inference) are length-prefixed
	// protobuf, not JSON — the extractors can't read them and storing the binary
	// would only add noise. Skip capture; the response still forwards untouched.
	if strings.Contains(contentType, "application/grpc") {
		slog.Debug("skipping gRPC response capture (protobuf not decodable)", "request_id", ctx.id, "path", ctx.path)
		return nil
	}

	// Non-gzip content encodings (br, deflate, zstd, …) are opaque to the JSON
	// extractors: the proxy forwards the agent's Accept-Encoding verbatim, so an
	// upstream may return one of these. Only gzip is transparently decoded below;
	// for the rest, storing the compressed bytes would wrap binary as a JSON string
	// and only add noise to the memory store. Skip capture (same reasoning as the
	// gRPC skip); the body still forwards to the agent untouched.
	if enc := resp.Header.Get("Content-Encoding"); enc != "" && !strings.EqualFold(enc, "gzip") {
		slog.Debug("skipping capture of non-gzip encoded response", "request_id", ctx.id, "encoding", enc, "path", ctx.path)
		return nil
	}

	isStreaming := strings.Contains(contentType, "text/event-stream") ||
		strings.Contains(contentType, "ndjson")

	if isStreaming {
		resp.Body = &streamCapture{
			ReadCloser: resp.Body,
			ctx:        ctx,
			store:      p.store,
			stats:      p.stats,
			statusCode: resp.StatusCode,
			clock:      p.clock,
		}
		return nil
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxNonStreamBodySize+1))
	resp.Body.Close()
	if err != nil {
		return err
	}
	if int64(len(body)) > maxNonStreamBodySize {
		slog.Warn("non-streaming response exceeds size limit", "request_id", ctx.id, "path", ctx.path, "limit", maxNonStreamBodySize)
		return fmt.Errorf("response body exceeds %d-byte limit", maxNonStreamBodySize)
	}

	// Transparent gzip decompression for capture. The response is served
	// uncompressed to the agent (simpler and avoids double-compression issues).
	// LimitReader caps decompression to maxDecompressSize to prevent gzip-bomb
	// OOM: if the limit is hit the compressed body is served unchanged.
	if strings.EqualFold(resp.Header.Get("Content-Encoding"), "gzip") {
		gr, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			slog.Warn("failed to decompress gzip response, storing raw", "request_id", ctx.id, "path", ctx.path, "err", err)
		} else {
			decompressed, err := io.ReadAll(io.LimitReader(gr, maxDecompressSize+1))
			gr.Close()
			if err != nil {
				slog.Warn("gzip decompression incomplete, storing raw", "request_id", ctx.id, "path", ctx.path, "err", err)
			} else if int64(len(decompressed)) > maxDecompressSize {
				slog.Warn("gzip response exceeds decompression limit, serving compressed", "request_id", ctx.id, "path", ctx.path, "limit", maxDecompressSize)
			} else {
				body = decompressed
				resp.Header.Del("Content-Encoding")
				resp.Header.Del("Content-Length")
				resp.ContentLength = int64(len(body))
			}
		}
	}

	resp.Body = io.NopCloser(bytes.NewReader(body))

	// Non-streaming: the body is now fully in hand, so this is the whole
	// response time for the turn. The streaming path measures its own at EOF
	// (streamCapture.finalize), where the clock covers the last delta rather
	// than just the first byte.
	if p.stats != nil {
		p.stats.ObserveLatency(p.since(ctx.start).Milliseconds())
	}

	if p.store != nil {
		ex := buildExchange(p.clock, ctx, resp.StatusCode, sanitizeJSON(body))
		p.store.Store(ex)
	}

	return nil
}

func (p *Proxy) errorHandler(w http.ResponseWriter, r *http.Request, err error) {
	// A client (agent) that cancels mid-flight — common when a user interrupts
	// the agent — surfaces here as context.Canceled. That's normal operation,
	// not a proxy fault: log at debug and skip the 502 so it doesn't generate
	// error-level noise that masks real upstream failures.
	if errors.Is(err, context.Canceled) {
		slog.Debug("proxy request canceled by client", "request_id", requestID(r.Context()), "method", r.Method, "path", r.URL.Path)
		return
	}
	slog.Error("proxy error", "request_id", requestID(r.Context()), "err", err, "method", r.Method, "path", r.URL.Path, "agent", p.agentName)
	if p.stats != nil {
		p.stats.ProxyErrors.Add(1)
	}
	writeJSONError(w, http.StatusBadGateway, "upstream request failed")
}

// writeJSONError writes a JSON error response compatible with the common
// subset of LLM API error formats (Anthropic, OpenAI, Gemini all use an
// "error" object with a "message" field). Using JSON avoids secondary parse
// failures in SDK error handlers that expect a JSON body on 4xx/5xx responses.
func writeJSONError(w http.ResponseWriter, statusCode int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	// Best-effort: if Marshal fails we can't do much, but it won't for a
	// plain string message.
	body, _ := json.Marshal(map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    "proxy_error",
		},
	})
	_, _ = w.Write(body)
}

// buildExchange constructs a CapturedExchange from capture context and
// response data. This is the single construction site for exchanges, used by
// both the non-streaming and streaming paths. The bodies are handed over as
// captured: stripping injected context and muninn tool traffic, and deriving
// model/usage, all parse bodies that reach tens of MiB, so they run on the
// store's worker goroutine (see prepareExchange) rather than here.
//
// DurationMs is measured here, at the one point every exchange passes through,
// so both paths time the same interval: arrival to the last response byte. The
// streaming path reaches this after the last delta, so its sample covers the
// whole stream rather than just the first byte.
func buildExchange(clock Clock, ctx *captureCtx, statusCode int, respBody json.RawMessage) *store.CapturedExchange {
	clock = clockOrSystem(clock)
	return &store.CapturedExchange{
		Agent:      ctx.agent,
		RequestID:  ctx.id,
		Path:       ctx.path,
		ReqBody:    ctx.reqBody,
		StatusCode: statusCode,
		RespBody:   respBody,
		DurationMs: clock.Since(ctx.start).Milliseconds(),
	}
}

// preparerSetter is the store capability that lets the proxy install its
// capture-side normalization. Asserted rather than required of Storer so a
// Storer that has no background worker (a test double, say) still works: it
// receives the exchange as captured.
type preparerSetter interface{ SetPreparer(store.Preparer) }

// prepareExchange is the store.Preparer the proxy installs on stores that
// accept one. It strips what must never reach long-term memory (injected
// context markers, MuninnDB's own tool calls and results, their tool
// definitions) from the request and response bodies, wraps a non-JSON payload
// so it stays storable, and fills in the model name and token usage.
func (p *Proxy) prepareExchange(ex *store.CapturedExchange) {
	// Each body is decoded once. cleanRequest/cleanResponse hand back the
	// document they filtered, so the model name, the usage counters, and the
	// two message texts the store writes come out of that single decode
	// instead of four more walks over bodies that reach tens of MiB.
	reqDoc, reqBody := cleanRequest(ex.ReqBody, p.filterPatterns)
	respDoc, respBody := cleanResponse(sanitizeJSON(ex.RespBody), p.filterPatterns)
	ex.ReqBody = reqBody
	ex.RespBody = respBody

	if reqDoc != nil {
		ex.Model, _ = reqDoc["model"].(string)
	}
	if respDoc != nil {
		readUsage(ex, respDoc)
	} else if !json.Valid(respBody) {
		// A captured response body can legitimately be valid JSON that isn't an
		// object — e.g. the synthetic string fallback buildRespBody emits for a
		// stream with no structured final event. That simply carries no usage
		// metadata; only flag genuinely malformed JSON. The validity check is
		// only reached when there is no decoded document, so it never re-scans a
		// body that was just parsed.
		slog.Debug("unparseable response body for token extraction",
			reqid.Field, ex.RequestID, "path", ex.Path, "agent", ex.Agent, "status", ex.StatusCode)
	}

	// Extracted as the capture pipeline sees them; the store strips system
	// reminders and redacts both texts on its own goroutine.
	userText := apiformat.ExtractUserQuery(reqDoc, apiformat.DetectFormat(reqDoc))
	assistantText := apiformat.ExtractAssistantText(respDoc)
	ex.UserText = &userText
	ex.AssistantText = &assistantText
}

// maxTokenCount bounds a usage number taken from an upstream response body. No
// real request comes near it (it is ~1e12 tokens), so anything beyond is a
// provider bug or a hostile body rather than a count.
const maxTokenCount = 1 << 40

// tokenCount converts a usage field from an upstream response body to a token
// count. The value crosses a trust boundary (an arbitrary JSON body from the
// provider or from whatever was MITM'd), and Go's float64→int conversion is
// undefined outside the representable range: 1e30 converts to an arbitrary
// value (MinInt64 on amd64), which would be stored on the exchange and added
// into the session token totals as a huge negative. Absent, non-finite,
// negative, and out-of-range values therefore report 0 — "no usage reported"
// — instead of a garbage count.
func tokenCount(v *float64) int {
	if v == nil {
		return 0
	}
	f := *v
	if math.IsNaN(f) || f <= 0 || f >= maxTokenCount {
		return 0
	}
	return int(f)
}

// readUsage fills the model name and token usage from a decoded response body.
// Handles:
//   - Anthropic: usage.{input_tokens, output_tokens, cache_creation_input_tokens, cache_read_input_tokens}
//   - OpenAI: usage.{prompt_tokens, completion_tokens}
//   - Gemini: usageMetadata.{promptTokenCount, candidatesTokenCount}
//   - Model from the response body or its modelVersion field
//
// The request body's model is already set by the caller, and is kept when
// present; a response never overwrites one the request supplied.
func readUsage(ex *store.CapturedExchange, doc map[string]any) {
	if ex.Model == "" {
		ex.Model, _ = doc["model"].(string)
	}
	if ex.Model == "" {
		ex.Model, _ = doc["modelVersion"].(string)
	}

	// Anthropic / OpenAI: "usage" object.
	if u, ok := doc["usage"].(map[string]any); ok {
		// Input tokens: Anthropic input_tokens or OpenAI prompt_tokens.
		if n, ok := usageNumber(u, "input_tokens"); ok {
			ex.TokensIn = n
		} else if n, ok := usageNumber(u, "prompt_tokens"); ok {
			ex.TokensIn = n
		}
		// Output tokens: Anthropic output_tokens or OpenAI completion_tokens.
		if n, ok := usageNumber(u, "output_tokens"); ok {
			ex.TokensOut = n
		} else if n, ok := usageNumber(u, "completion_tokens"); ok {
			ex.TokensOut = n
		}
		// Anthropic prompt caching tokens.
		ex.CacheWrite, _ = usageNumber(u, "cache_creation_input_tokens")
		ex.CacheRead, _ = usageNumber(u, "cache_read_input_tokens")
	}

	// Gemini: "usageMetadata" object.
	if u, ok := doc["usageMetadata"].(map[string]any); ok {
		if n, ok := usageNumber(u, "promptTokenCount"); ok {
			ex.TokensIn = n
		}
		if n, ok := usageNumber(u, "candidatesTokenCount"); ok {
			ex.TokensOut = n
		}
	}
}

// usageNumber reads one usage counter from a decoded usage object. The second
// result is false when the field is absent, so a provider that omits it leaves
// an already-extracted count alone rather than zeroing it.
func usageNumber(u map[string]any, key string) (int, bool) {
	f, ok := u[key].(float64)
	if !ok {
		return 0, false
	}
	return tokenCount(&f), true
}

// sanitizeJSON ensures data is valid JSON for MuninnDB storage. Non-JSON
// payloads (e.g. plain text error pages) are wrapped as a JSON string.
func sanitizeJSON(data []byte) json.RawMessage {
	if len(data) == 0 {
		return json.RawMessage("null")
	}
	if json.Valid(data) {
		return json.RawMessage(data)
	}
	b, _ := json.Marshal(string(data))
	return json.RawMessage(b)
}

// toLowerSlice returns a new slice with all strings lowercased.
func toLowerSlice(ss []string) []string {
	if ss == nil {
		return nil
	}
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = strings.ToLower(s)
	}
	return out
}

// redactURL returns a loggable form of a URL with every credential carrier
// replaced by "[redacted]": the query string (providers pass keys as query
// params, e.g. Gemini ?key=...), any userinfo (https://user:pass@host, which
// url.String() would otherwise render verbatim), and the fragment (implicit
// OAuth flows return #access_token=...&token_type=...). The result is safe to
// log; the host, scheme, and path — what makes the line diagnosable — are kept.
func redactURL(u *url.URL) string {
	redacted := *u
	if redacted.RawQuery != "" {
		redacted.RawQuery = "[redacted]"
	}
	// A URL with no "//" after the scheme is opaque, and its userinfo stays
	// inside Opaque as literal text (net/url populates User only for the
	// hierarchical form), so it must be redacted there too. The last '@' is
	// the separator, matching how url.Userinfo is derived.
	if i := strings.LastIndex(redacted.Opaque, "@"); i >= 0 {
		redacted.Opaque = "[redacted]@" + redacted.Opaque[i+1:]
	}
	if redacted.User != nil {
		redacted.User = url.User("[redacted]")
	}
	if redacted.Fragment != "" {
		redacted.Fragment = "[redacted]"
	}
	// String() percent-escapes the userinfo and fragment (but not the query),
	// so the marker would render as %5Bredacted%5D there. Undo that for the
	// marker only, so every carrier shows the same literal "[redacted]".
	return strings.ReplaceAll(redacted.String(), "%5Bredacted%5D", "[redacted]")
}

// singleJoiningSlash joins two path segments ensuring exactly one slash between them.
func singleJoiningSlash(a, b string) string {
	aslash := strings.HasSuffix(a, "/")
	bslash := strings.HasPrefix(b, "/")
	switch {
	case aslash && bslash:
		return a + b[1:]
	case !aslash && !bslash:
		return a + "/" + b
	}
	return a + b
}
