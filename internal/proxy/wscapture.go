package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/maci0/muninn-sidecar/internal/store"
)

// wsDebug, when MSC_WS_DEBUG is set, makes the parser log the envelope `type`
// (and size) of every decoded WebSocket message. msc captures codex's
// Responses-API WebSocket out of the box; other agents stream over proprietary
// WebSocket protocols (e.g. grok's gateway at wss://grok.com/ws/gw/) whose
// envelope is unknown without observing live traffic. This flag surfaces the
// message shape — not the content — so a new protocol can be mapped. Read once.
var wsDebug = os.Getenv("MSC_WS_DEBUG") != ""

// wsMessageType extracts the JSON `type` field from a decoded WebSocket text
// message for diagnostics, or "" if the message isn't a JSON object with a
// string `type`. Returns only the discriminator field, never message content.
func wsMessageType(msg []byte) string {
	var env struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(msg, &env) != nil {
		return ""
	}
	return env.Type
}

// wsMaxRespText caps the assistant text accumulated for one decoded turn.
const wsMaxRespText = 16 << 10

// wsExchange pairs codex's WebSocket messages into captured exchanges. codex
// frames the OpenAI Responses API over a WebSocket: the client sends
// `{"type":"response.create", ...input...}` (a Responses-format request, which
// the store's existing extractor reads), and the server streams the answer as
// `response.output_text.delta` events terminated by `response.completed`. We
// accumulate the deltas and, on completion, pair the turn's text with the last
// request and store it; the store handles extraction, redaction, and dedup.
type wsExchange struct {
	p        *Proxy
	target   string
	mu       sync.Mutex
	lastReq  []byte          // most recent response.create payload (the request)
	respText strings.Builder // assistant text accumulated from output_text deltas (s->c goroutine only)
}

// onClient handles client→server messages: remember the latest request.
func (e *wsExchange) onClient(_ string, msg []byte) {
	var env struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(msg, &env) != nil || env.Type != "response.create" {
		return
	}
	e.mu.Lock()
	e.lastReq = append([]byte(nil), msg...)
	e.mu.Unlock()
}

// onServer handles server→client events. codex streams the answer as
// `response.output_text.delta` events (the response.completed envelope's output
// is empty), so accumulate the deltas and, on `response.completed`, pair the
// turn's text with the last request and store it. Reasoning-only cycles emit no
// text deltas, so they're naturally skipped. Runs on a single goroutine, so the
// accumulator needs no lock (only lastReq is shared).
func (e *wsExchange) onServer(_ string, msg []byte) {
	var env struct {
		Type  string `json:"type"`
		Delta string `json:"delta"`
	}
	if json.Unmarshal(msg, &env) != nil {
		return
	}
	switch env.Type {
	case "response.output_text.delta":
		// Clamp to the cap instead of testing it: a single delta is appended
		// whole, so the previous form let a long delta overshoot wsMaxRespText by
		// its full length. clampBytes keeps the cap from landing mid-rune, the
		// same guard the SSE path applies to text deltas.
		if e.respText.Len() < wsMaxRespText {
			delta := env.Delta
			if remaining := wsMaxRespText - e.respText.Len(); len(delta) > remaining {
				delta = clampBytes(delta, remaining)
			}
			e.respText.WriteString(delta)
		}
	case "response.completed":
		text := strings.TrimSpace(e.respText.String())
		e.respText.Reset()
		if text == "" {
			return // reasoning-only cycle or no visible answer
		}
		e.mu.Lock()
		req := e.lastReq
		e.mu.Unlock()
		if req == nil {
			return
		}
		// Synthetic response body the store's extractor understands (the user
		// query comes from the Responses-format request; this carries the answer).
		respBody, _ := json.Marshal(map[string]any{
			"content": []map[string]any{{"type": "text", "text": text}},
		})
		e.p.store.Store(&store.CapturedExchange{
			Agent:      e.p.agentName,
			Path:       "/backend-api/codex/responses",
			ReqBody:    req,
			StatusCode: 200,
			RespBody:   respBody,
		})
		slog.Debug("ws capture: stored exchange", "target", e.target, "resp_bytes", len(text))
	}
}

// WebSocket capture for intercepted MITM upgrades. The splice forwards bytes
// verbatim and unconditionally; a copy is fed best-effort to per-direction frame
// parsers. If a parser can't keep up, capture is abandoned for the connection —
// forwarding is never blocked or altered. This lets msc observe (and, with the
// schema mapping, capture) WebSocket-framed exchanges like codex ChatGPT-mode.

// readHeaderBlock reads through the end of an HTTP header block (\r\n\r\n) and
// returns the bytes verbatim — used to forward the backend's 101 handshake.
func readHeaderBlock(r *bufio.Reader) ([]byte, error) {
	var out []byte
	for {
		// ReadSlice (not ReadBytes) so a backend streaming endless bytes with no
		// newline can't grow an unbounded fragment list: it returns ErrBufferFull
		// per buffer-load, letting the cap below fire mid-line.
		line, err := r.ReadSlice('\n')
		out = append(out, line...)
		if err == bufio.ErrBufferFull {
			if len(out) > 64<<10 {
				return out, io.ErrShortBuffer
			}
			continue
		}
		if err != nil {
			return out, err
		}
		if string(line) == "\r\n" || string(line) == "\n" {
			return out, nil
		}
		if len(out) > 64<<10 {
			return out, io.ErrShortBuffer
		}
	}
}

// chanReader presents a stream of []byte chunks (from the splice tap) as an
// io.Reader for the frame parser. Returns io.EOF when the channel is closed.
type chanReader struct {
	ch  <-chan []byte
	cur []byte
}

func (c *chanReader) Read(p []byte) (int, error) {
	for len(c.cur) == 0 {
		chunk, ok := <-c.ch
		if !ok {
			return 0, io.EOF
		}
		c.cur = chunk
	}
	n := copy(p, c.cur)
	c.cur = c.cur[n:]
	return n, nil
}

// spliceCopyTap copies src→dst (forwarding, unconditional and first) while
// feeding a best-effort copy of each chunk to tap. On backpressure (tap full) it
// abandons the tap (closing it once) and keeps forwarding — so capture can never
// stall or break the agent's connection. tap may be nil (forward only). target
// labels the connection for the abandonment log so an operator can tell which
// upstream stopped being captured.
func spliceCopyTap(dst io.Writer, src io.Reader, tap chan []byte, target string) {
	buf := make([]byte, 32*1024)
	tapping := tap != nil
	for {
		n, rerr := src.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				if tapping {
					close(tap)
				}
				return
			}
			if tapping {
				select {
				case tap <- append([]byte(nil), buf[:n]...):
				default: // parser fell behind — abandon capture, keep forwarding
					// Surface the silent capture loss: without this an operator
					// seeing empty WebSocket captures (e.g. codex ChatGPT mode)
					// has no signal that the tap was dropped under load.
					slog.Warn("ws capture: parser fell behind, abandoning capture for connection", "target", target)
					close(tap)
					tapping = false
				}
			}
		}
		if rerr != nil {
			if tapping {
				close(tap)
			}
			return
		}
	}
}

// runWSParser reassembles text messages from a tapped direction and hands each
// to onMessage. Exits on stream end or an unrecoverable decode error (e.g.
// permessage-deflate desync after a dropped chunk) — capture stops, forwarding
// is unaffected.
func runWSParser(dir string, ch <-chan []byte, deflate bool, onMessage func(dir string, msg []byte)) {
	r := bufio.NewReader(&chanReader{ch: ch})
	asm := &wsMessageAssembler{deflate: deflate}
	for {
		f, err := readWSFrame(r)
		if err != nil {
			return
		}
		msg, err := asm.add(f)
		if err != nil {
			slog.Debug("ws capture: decode stopped", "dir", dir, "err", err)
			return
		}
		if msg != nil {
			if wsDebug {
				slog.Info("ws message", "dir", dir, "type", wsMessageType(msg), "bytes", len(msg))
			}
			onMessage(dir, msg)
		}
	}
}

// spliceWithCapture forwards an intercepted WebSocket bidirectionally (verbatim)
// and, when a store is configured, taps both directions to decode text messages.
// The backend's 101 handshake is read+forwarded first so framing starts cleanly
// and permessage-deflate negotiation is detected.
func (p *Proxy) spliceWithCapture(client net.Conn, clientBuf *bufio.Reader, backend net.Conn, target string) {
	backendBuf := bufio.NewReader(backend)
	if err := backend.SetReadDeadline(time.Now().Add(p.upgradeHandshakeTimeout)); err != nil {
		slog.Debug("ws capture: could not set backend handshake deadline", "target", target, "err", err)
		return
	}
	hdr, err := readHeaderBlock(backendBuf)
	if err != nil {
		slog.Debug("ws capture: no backend upgrade response", "target", target, "err", err)
		return
	}
	if _, err := client.Write(hdr); err != nil {
		slog.Debug("ws capture: could not forward upgrade response to client", "target", target, "err", err)
		return
	}
	// The handshake is done; the splice below is a long-lived tunnel whose reads
	// are bounded only by the peers, so the deadline must not leak into it.
	if err := backend.SetReadDeadline(time.Time{}); err != nil {
		slog.Debug("ws capture: could not clear backend handshake deadline", "target", target, "err", err)
		return
	}
	deflate := bytes.Contains(bytes.ToLower(hdr), []byte("permessage-deflate"))

	var c2s, s2c chan []byte
	if p.store != nil {
		c2s = make(chan []byte, 256)
		s2c = make(chan []byte, 256)
		ex := &wsExchange{p: p, target: target}
		go runWSParser("c->s", c2s, deflate, ex.onClient)
		go runWSParser("s->c", s2c, deflate, ex.onServer)
		slog.Debug("ws capture: tapping upgraded tunnel", "target", target, "deflate", deflate)
	}

	// Wait for both directions: returning on the first would close both conns
	// and truncate the peer copy mid-stream (e.g. the WS close handshake). The
	// half-close lets the peer direction drain, so this cannot deadlock.
	done := make(chan struct{}, 2)
	splice := func(dst net.Conn, src io.Reader, tap chan []byte) {
		spliceCopyTap(dst, src, tap, target)
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			cw.CloseWrite()
		}
		done <- struct{}{}
	}
	go splice(backend, clientBuf, c2s) // client → server (masked frames)
	go splice(client, backendBuf, s2c) // server → client (post-101 frames)
	<-done
	<-done
}
