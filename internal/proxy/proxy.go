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
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/maci0/muninn-sidecar/internal/mitm"
	"github.com/maci0/muninn-sidecar/internal/store"
)

// Enricher enriches a request body with recalled context from MuninnDB.
// A nil Enricher disables injection. Implemented by *inject.Injector.
type Enricher interface {
	Enrich(ctx context.Context, body []byte) ([]byte, int, error)
}

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
	agentName      string                 // "claude", "gemini", etc. — used for tagging
	store          Storer                 // async MuninnDB writer
	capturePaths   []string               // path substrings to capture; empty = capture all
	excludePaths   []string               // path substrings to exclude from capture (checked first)
	filterPatterns []string               // tool name patterns to strip from stored bodies; empty non-nil = no filtering
	injector       Enricher               // optional memory injector (nil = disabled)
	server         *http.Server           // underlying HTTP server
	reverseProxy   *httputil.ReverseProxy // stdlib reverse proxy with our hooks
	ca             *mitm.CA               // non-nil enables TLS-MITM of CONNECT tunnels
	mitmTransport  *http.Transport        // TLS transport to real upstream hosts (MITM)
	mitmHosts      map[string]bool        // hosts to TLS-terminate; others are blind-tunneled
	mitmAll        bool                   // intercept every CONNECT host (allowlist contained "*")
}

// Config holds the parameters for creating a Proxy.
type Config struct {
	ListenAddr     string   // e.g. "127.0.0.1:0" for random port
	Upstream       string   // real API URL to forward to
	AgentName      string   // agent name for tagging in MuninnDB
	Store          Storer   // MuninnDB writer; nil = discard captures
	CapturePaths   []string // path substrings to capture; empty = capture all
	ExcludePaths   []string // path substrings to exclude from capture (checked first)
	FilterPatterns []string // tool name patterns to strip; nil = defaultFilterPatterns; []string{} = disable all filtering
	Injector       Enricher // optional memory injector; nil = disabled
	CA             *mitm.CA // non-nil enables TLS-MITM: CONNECT tunnels are terminated and intercepted
	MITMHosts      []string // extra hosts to TLS-terminate (besides the upstream host); "*" intercepts all. Others are blind-tunneled untouched.
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

	transport := &http.Transport{
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS13},
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 100,
		IdleConnTimeout:     90 * time.Second,
	}
	// Separate transport for MITM forwarding to arbitrary real hosts. TLS1.2 floor
	// (some upstreams still require it) with normal cert verification of the real
	// server — msc only forges the agent-facing side, never trusts a bad upstream.
	p.mitmTransport = &http.Transport{
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 100,
		IdleConnTimeout:     90 * time.Second,
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
	p.server = &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           p,
		ReadHeaderTimeout: 30 * time.Second,
		ReadTimeout:       5 * time.Minute,
		WriteTimeout:      10 * time.Minute,
		IdleTimeout:       120 * time.Second,
	}

	return p, nil
}

// ListenAddr returns the actual address the proxy is listening on.
func (p *Proxy) ListenAddr() string { return p.listenAddr }

// SetMITMRoots overrides the root CAs used to verify real upstream servers on
// the MITM forward leg. By default the system trust store is used; supply a
// custom pool for environments with a private upstream CA (e.g. a corporate
// egress proxy) or for tests that forward to a self-signed server. No-op when
// MITM is disabled (no CA configured).
func (p *Proxy) SetMITMRoots(pool *x509.CertPool) {
	if p.mitmTransport == nil {
		return
	}
	p.mitmTransport.TLSClientConfig.RootCAs = pool
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
	// MITM mode: an agent that routes via HTTPS_PROXY opens a tunnel with CONNECT.
	// Terminate TLS and intercept the decrypted traffic (the same pipeline).
	if r.Method == http.MethodConnect && p.ca != nil {
		p.handleConnect(w, r)
		return
	}

	r, ok := p.instrument(w, r, time.Now())
	if !ok {
		return // instrument already wrote an error response
	}
	p.reverseProxy.ServeHTTP(w, r)
}

// instrument applies the capture/inject pipeline shared by the plain reverse-proxy
// path and the MITM tunnel: if the path is captured, it buffers the body (within
// the size limit), enriches it with recalled memories, and stashes capture
// metadata in the request context. Returns the (possibly rewritten) request and
// whether to proceed forwarding — false means an error response was already
// written. Non-captured requests pass through untouched.
func (p *Proxy) instrument(w http.ResponseWriter, r *http.Request, start time.Time) (*http.Request, bool) {
	capture := p.shouldCapture(r.URL.Path)
	slog.Debug("request", "path", r.URL.Path, "capture", capture)
	if !capture {
		return r, true
	}

	var reqBody []byte
	if r.Body != nil {
		var err error
		reqBody, err = io.ReadAll(io.LimitReader(r.Body, maxRequestBodySize+1))
		if err != nil {
			slog.Warn("failed to read request body for capture", "path", r.URL.Path, "err", err)
			writeJSONError(w, http.StatusInternalServerError, "failed to read request body")
			return r, false
		}
		if int64(len(reqBody)) > maxRequestBodySize {
			slog.Warn("request body exceeds size limit", "path", r.URL.Path, "limit", maxRequestBodySize)
			writeJSONError(w, http.StatusRequestEntityTooLarge, "request body exceeds proxy size limit")
			return r, false
		}
	}

	// Enrich with recalled memories if injector is enabled.
	forwardBody := reqBody
	if p.injector != nil && len(reqBody) > 0 {
		enriched, _, err := p.injector.Enrich(r.Context(), reqBody)
		if err != nil {
			slog.Warn("inject enrichment failed, using original body", "path", r.URL.Path, "err", err)
		} else if len(enriched) > 0 {
			forwardBody = enriched
		}
	}

	r.Body = io.NopCloser(bytes.NewReader(forwardBody))
	r.ContentLength = int64(len(forwardBody))

	ctx := &captureCtx{
		start:          start,
		method:         r.Method,
		path:           r.URL.Path,
		reqBody:        reqBody, // original body for capture (not enriched)
		agent:          p.agentName,
		filterPatterns: p.filterPatterns,
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

	contentType := resp.Header.Get("Content-Type")
	isStreaming := strings.Contains(contentType, "text/event-stream") ||
		strings.Contains(contentType, "ndjson")

	if isStreaming {
		resp.Body = &streamCapture{
			ReadCloser: resp.Body,
			ctx:        ctx,
			store:      p.store,
			statusCode: resp.StatusCode,
		}
		return nil
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxNonStreamBodySize+1))
	resp.Body.Close()
	if err != nil {
		return err
	}
	if int64(len(body)) > maxNonStreamBodySize {
		slog.Warn("non-streaming response exceeds size limit", "path", ctx.path, "limit", maxNonStreamBodySize)
		return fmt.Errorf("response body exceeds %d-byte limit", maxNonStreamBodySize)
	}

	// Transparent gzip decompression for capture. The response is served
	// uncompressed to the agent (simpler and avoids double-compression issues).
	// LimitReader caps decompression to maxDecompressSize to prevent gzip-bomb
	// OOM: if the limit is hit the compressed body is served unchanged.
	if resp.Header.Get("Content-Encoding") == "gzip" {
		gr, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			slog.Warn("failed to decompress gzip response, storing raw", "path", ctx.path, "err", err)
		} else {
			decompressed, err := io.ReadAll(io.LimitReader(gr, maxDecompressSize+1))
			gr.Close()
			if err != nil {
				slog.Warn("gzip decompression incomplete, storing raw", "path", ctx.path, "err", err)
			} else if int64(len(decompressed)) > maxDecompressSize {
				slog.Warn("gzip response exceeds decompression limit, serving compressed", "path", ctx.path, "limit", maxDecompressSize)
			} else {
				body = decompressed
				resp.Header.Del("Content-Encoding")
				resp.Header.Del("Content-Length")
				resp.ContentLength = int64(len(body))
			}
		}
	}

	resp.Body = io.NopCloser(bytes.NewReader(body))

	if p.store != nil {
		ex := buildExchange(ctx, resp.StatusCode, sanitizeJSON(body))
		p.store.Store(ex)
	}

	return nil
}

func (p *Proxy) errorHandler(w http.ResponseWriter, r *http.Request, err error) {
	slog.Error("proxy error", "err", err, "method", r.Method, "path", r.URL.Path, "agent", p.agentName)
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
// response data. This is the single construction site for exchanges,
// used by both the non-streaming and streaming paths.
func buildExchange(ctx *captureCtx, statusCode int, respBody json.RawMessage) *store.CapturedExchange {
	ex := &store.CapturedExchange{
		Timestamp:  ctx.start,
		Agent:      ctx.agent,
		Method:     ctx.method,
		Path:       ctx.path,
		ReqBody:    cleanRequest(ctx.reqBody, ctx.filterPatterns),
		StatusCode: statusCode,
		RespBody:   cleanResponse(respBody, ctx.filterPatterns),
		DurationMs: time.Since(ctx.start).Milliseconds(),
	}
	extractModelAndTokens(ex)
	return ex
}

// extractModelAndTokens pulls the model name and token usage from the
// request and response JSON. Handles:
//   - Anthropic: usage.{input_tokens, output_tokens, cache_creation_input_tokens, cache_read_input_tokens}
//   - OpenAI: usage.{prompt_tokens, completion_tokens}
//   - Gemini: usageMetadata.{promptTokenCount, candidatesTokenCount}
//   - Model from request body, response body, or response modelVersion field
func extractModelAndTokens(ex *store.CapturedExchange) {
	var reqData map[string]any
	if err := json.Unmarshal(ex.ReqBody, &reqData); err != nil {
		slog.Debug("unparseable request body for token extraction", "path", ex.Path, "err", err)
	} else {
		if m, ok := reqData["model"].(string); ok {
			ex.Model = m
		}
	}

	var respData map[string]any
	if err := json.Unmarshal(ex.RespBody, &respData); err != nil {
		slog.Debug("unparseable response body for token extraction", "path", ex.Path, "err", err)
		return
	}

	// Model: prefer request model, fall back to response model or modelVersion.
	if ex.Model == "" {
		if m, ok := respData["model"].(string); ok {
			ex.Model = m
		}
	}
	if ex.Model == "" {
		if m, ok := respData["modelVersion"].(string); ok {
			ex.Model = m
		}
	}

	// Anthropic / OpenAI: "usage" object.
	if usage, ok := respData["usage"].(map[string]any); ok {
		// Input tokens: Anthropic input_tokens or OpenAI prompt_tokens.
		if v, ok := usage["input_tokens"].(float64); ok {
			ex.TokensIn = int(v)
		} else if v, ok := usage["prompt_tokens"].(float64); ok {
			ex.TokensIn = int(v)
		}
		// Output tokens: Anthropic output_tokens or OpenAI completion_tokens.
		if v, ok := usage["output_tokens"].(float64); ok {
			ex.TokensOut = int(v)
		} else if v, ok := usage["completion_tokens"].(float64); ok {
			ex.TokensOut = int(v)
		}
		// Anthropic prompt caching tokens.
		if v, ok := usage["cache_creation_input_tokens"].(float64); ok {
			ex.CacheWrite = int(v)
		}
		if v, ok := usage["cache_read_input_tokens"].(float64); ok {
			ex.CacheRead = int(v)
		}
	}

	// Gemini: "usageMetadata" object.
	if usage, ok := respData["usageMetadata"].(map[string]any); ok {
		if v, ok := usage["promptTokenCount"].(float64); ok {
			ex.TokensIn = int(v)
		}
		if v, ok := usage["candidatesTokenCount"].(float64); ok {
			ex.TokensOut = int(v)
		}
	}
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

// redactURL returns a URL string with the query string replaced by "[redacted]"
// to avoid leaking API keys that some providers pass as query parameters
// (e.g. Gemini ?key=...).
func redactURL(u *url.URL) string {
	if u.RawQuery == "" {
		return u.String()
	}
	redacted := *u
	redacted.RawQuery = "[redacted]"
	return redacted.String()
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
