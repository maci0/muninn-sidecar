// Package proxy implements a transparent reverse proxy that captures
// LLM API traffic for MuninnDB.
package proxy

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/maci0/muninn-sidecar/internal/store"
)

// maxStreamBuf caps the incremental SSE line buffer to prevent OOM.
// Partial lines exceeding this limit are silently dropped.
const maxStreamBuf = 1 << 20 // 1 MiB

// Proxy is a transparent reverse proxy that sits between a coding agent and
// its LLM API upstream. All traffic is forwarded, but only requests matching
// CapturePaths are recorded to MuninnDB (asynchronously). The agent sees the
// proxy as the real API because we override its base-URL env var (e.g.
// ANTHROPIC_BASE_URL) to point here.
//
// Streaming (SSE) responses are handled specially: the body is wrapped so
// chunks flow through to the agent in real-time while the last SSE data line
// is tracked incrementally. On EOF the final data line is stored.
type Proxy struct {
	listenAddr     string                 // resolved after Start() when port is :0
	upstream       *url.URL               // real LLM API (e.g. https://api.anthropic.com)
	agentName      string                 // "claude", "gemini", etc. — used for tagging
	store          *store.MuninnStore     // async MuninnDB writer
	capturePaths   []string               // path substrings to capture; empty = capture all
	filterPatterns []string               // tool name patterns to strip from stored bodies
	server         *http.Server           // underlying HTTP server
	reverseP       *httputil.ReverseProxy // stdlib reverse proxy with our hooks
}

// Config holds the parameters for creating a Proxy.
type Config struct {
	ListenAddr     string             // e.g. "127.0.0.1:0" for random port
	Upstream       string             // real API URL to forward to
	AgentName      string             // agent name for tagging in MuninnDB
	Store          *store.MuninnStore // MuninnDB writer
	CapturePaths   []string           // path substrings to capture; empty = capture all
	FilterPatterns []string           // tool name patterns to strip; nil = defaultFilterPatterns
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
		capturePaths:   cfg.CapturePaths,
		filterPatterns: filterPatterns,
	}

	transport := &http.Transport{
		TLSClientConfig:    &tls.Config{MinVersion: tls.VersionTLS12},
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 100,
		IdleConnTimeout:     90 * time.Second,
	}

	p.reverseP = &httputil.ReverseProxy{
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
	p.server = &http.Server{
		Addr:         cfg.ListenAddr,
		Handler:      p,
		ReadTimeout:  5 * time.Minute,
		WriteTimeout: 10 * time.Minute,
		IdleTimeout:  120 * time.Second,
	}

	return p, nil
}

// ListenAddr returns the actual address the proxy is listening on.
func (p *Proxy) ListenAddr() string { return p.listenAddr }

// Start begins listening. Returns the resolved listen address (with actual
// port if :0 was used). The server runs in a background goroutine.
func (p *Proxy) Start() (string, error) {
	ln, err := net.Listen("tcp", p.listenAddr)
	if err != nil {
		return "", err
	}
	addr := ln.Addr().String()
	p.listenAddr = addr

	go func() {
		if err := p.server.Serve(ln); err != nil && err != http.ErrServerClosed {
			slog.Error("proxy server error", "err", err)
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
	start := time.Now()

	capture := p.shouldCapture(r.URL.Path)
	slog.Debug("request", "path", r.URL.Path, "capture", capture)
	if capture {
		var reqBody []byte
		if r.Body != nil {
			var err error
			reqBody, err = io.ReadAll(r.Body)
			if err != nil {
				slog.Warn("failed to read request body for capture", "path", r.URL.Path, "err", err)
			}
			r.Body = io.NopCloser(bytes.NewReader(reqBody))
		}

		ctx := &captureCtx{
			start:          start,
			method:         r.Method,
			path:           r.URL.Path,
			reqBody:        reqBody,
			agent:          p.agentName,
			filterPatterns: p.filterPatterns,
		}
		r = r.WithContext(withCapture(r.Context(), ctx))
	}

	p.reverseP.ServeHTTP(w, r)
}

// shouldCapture returns true if the request path matches one of the
// configured CapturePaths (case-insensitive). An empty CapturePaths list
// means capture all. Case-insensitivity is needed because Gemini API key
// mode uses lowercase paths (generateContent) while OAuth mode uses
// camelCase (streamGenerateContent).
func (p *Proxy) shouldCapture(path string) bool {
	if len(p.capturePaths) == 0 {
		return true
	}
	lowerPath := strings.ToLower(path)
	for _, sub := range p.capturePaths {
		if strings.Contains(lowerPath, strings.ToLower(sub)) {
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

	slog.Debug("proxying", "method", pr.Out.Method, "url", pr.Out.URL.String())
}

// captureResponse is called after the upstream responds. For non-streaming
// responses it reads the full body, captures the exchange, and re-wraps the
// body. For SSE/ndjson streams it wraps the body in a streamCapture that
// tees data through while tracking the last SSE data line incrementally.
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

	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return err
	}

	// Transparent gzip decompression for capture. The response is served
	// uncompressed to the agent (simpler and avoids double-compression issues).
	if resp.Header.Get("Content-Encoding") == "gzip" {
		gr, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			slog.Warn("failed to decompress gzip response, storing raw", "path", ctx.path, "err", err)
		} else {
			decompressed, err := io.ReadAll(gr)
			gr.Close()
			if err != nil {
				slog.Warn("gzip decompression incomplete, storing raw", "path", ctx.path, "err", err)
			} else {
				body = decompressed
				resp.Header.Del("Content-Encoding")
				resp.Header.Del("Content-Length")
			}
		}
	}

	resp.Body = io.NopCloser(bytes.NewReader(body))

	ex := buildExchange(ctx, resp.StatusCode, sanitizeJSON(body))
	p.store.Store(ex)

	return nil
}

func (p *Proxy) errorHandler(w http.ResponseWriter, r *http.Request, err error) {
	slog.Error("proxy error", "err", err, "path", r.URL.Path)
	http.Error(w, fmt.Sprintf("proxy error: %v", err), http.StatusBadGateway)
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
		ReqBody:    filterMCPTools(sanitizeJSON(ctx.reqBody), ctx.filterPatterns),
		StatusCode: statusCode,
		RespBody:   filterMCPTools(respBody, ctx.filterPatterns),
		DurationMs: time.Since(ctx.start).Milliseconds(),
	}
	extractModelAndTokens(ex)
	return ex
}

// streamCapture wraps a streaming response body (SSE or ndjson). Data flows
// through to the agent via Read() while the last SSE "data:" line is tracked
// incrementally without buffering the entire stream. On EOF the last data
// line is stored as the response summary.
//
// sync.Once ensures the store call happens exactly once even if Read returns
// EOF multiple times (which http.Response.Body contracts allow).
type streamCapture struct {
	io.ReadCloser
	ctx        *captureCtx
	store      *store.MuninnStore
	statusCode int
	once       sync.Once

	// Incremental SSE parsing: we track the last non-[DONE] data line
	// and a line buffer for partial reads, avoiding unbounded memory.
	lineBuf  []byte // partial line carried across Read calls
	lastData string // last complete "data: ..." value seen
	totalLen int    // total bytes seen (for fallback summary)
}

func (sc *streamCapture) Read(p []byte) (int, error) {
	n, err := sc.ReadCloser.Read(p)
	if n > 0 {
		sc.processChunk(p[:n])
	}
	if err == io.EOF {
		sc.once.Do(func() {
			respBody := sc.buildRespBody()
			ex := buildExchange(sc.ctx, sc.statusCode, respBody)
			sc.store.Store(ex)
		})
	}
	return n, err
}

// processChunk scans the chunk for complete "data: ..." lines, updating
// lastData incrementally. Partial lines are carried in lineBuf.
func (sc *streamCapture) processChunk(chunk []byte) {
	sc.totalLen += len(chunk)

	// Prepend any leftover from the previous read.
	data := chunk
	if len(sc.lineBuf) > 0 {
		data = append(sc.lineBuf, chunk...)
		sc.lineBuf = nil
	}

	for len(data) > 0 {
		idx := bytes.IndexByte(data, '\n')
		if idx == -1 {
			// Incomplete line — stash for next Read, but cap to avoid
			// accumulating a huge partial line.
			if len(data) <= maxStreamBuf {
				sc.lineBuf = append(sc.lineBuf[:0], data...)
			} else {
				slog.Debug("SSE line buffer exceeded limit, dropping partial line", "len", len(data))
			}
			break
		}

		line := string(data[:idx])
		data = data[idx+1:]

		if strings.HasPrefix(line, "data: ") {
			d := line[len("data: "):]
			if d != "[DONE]" {
				sc.lastData = d
			}
		}
	}
}

// buildRespBody returns the response body for storage. If we captured a
// "data:" line, use that (the last event typically has usage/stop_reason).
// Otherwise, note the stream size.
func (sc *streamCapture) buildRespBody() json.RawMessage {
	if sc.lastData != "" && json.Valid([]byte(sc.lastData)) {
		return json.RawMessage(sc.lastData)
	}
	if sc.lastData != "" {
		b, _ := json.Marshal(sc.lastData)
		return json.RawMessage(b)
	}
	b, _ := json.Marshal(map[string]any{
		"_stream": true,
		"_bytes":  sc.totalLen,
	})
	return b
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
