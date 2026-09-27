package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/maci0/muninn-sidecar/internal/store"
)

// recordStore records exchanges handed to Store (test stub).
type recordStore struct {
	mu  sync.Mutex
	got []*store.CapturedExchange
}

func (r *recordStore) Store(e *store.CapturedExchange) {
	r.mu.Lock()
	r.got = append(r.got, e)
	r.mu.Unlock()
}

func (r *recordStore) all() []*store.CapturedExchange {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*store.CapturedExchange(nil), r.got...)
}

func TestWSExchangeCapture(t *testing.T) {
	rec := &recordStore{}
	ex := &wsExchange{p: &Proxy{store: rec, agentName: "codex"}, target: "chatgpt.com:443", requestID: "req-9"}

	// Request (Responses-format) then streamed answer deltas, then completion.
	ex.onClient("c->s", []byte(`{"type":"response.create","model":"gpt-5","input":[{"type":"message","role":"user","content":"what is 2+2"}]}`))
	ex.onServer("s->c", []byte(`{"type":"response.output_text.delta","delta":"the answer "}`))
	ex.onServer("s->c", []byte(`{"type":"response.output_text.delta","delta":"is 4"}`))
	ex.onServer("s->c", []byte(`{"type":"response.completed"}`))

	got := rec.all()
	if len(got) != 1 {
		t.Fatalf("expected 1 stored exchange, got %d", len(got))
	}
	if !bytes.Contains(got[0].ReqBody, []byte("what is 2+2")) {
		t.Errorf("request body missing user content: %s", got[0].ReqBody)
	}
	if !bytes.Contains(got[0].RespBody, []byte("the answer is 4")) {
		t.Errorf("response body missing accumulated answer: %s", got[0].RespBody)
	}
	if got[0].Agent != "codex" || got[0].StatusCode != 200 {
		t.Errorf("unexpected exchange metadata: %+v", got[0])
	}
	// A WebSocket turn is stored long after its upgrade request returned, with
	// every message on the connection funneled through one tap, so the upgrade's
	// correlation ID is the only thing tying this exchange to a log line.
	if got[0].RequestID != "req-9" {
		t.Errorf("RequestID = %q, want the upgrade request's ID %q", got[0].RequestID, "req-9")
	}
}

func TestWSExchangeResponseTextCapped(t *testing.T) {
	// A single oversized delta must not push the accumulated text past
	// wsMaxRespText; the cap is the bound on what gets stored.
	rec := &recordStore{}
	ex := &wsExchange{p: &Proxy{store: rec, agentName: "codex"}, target: "chatgpt.com:443"}

	ex.onClient("c->s", []byte(`{"type":"response.create","model":"gpt-5","input":[{"type":"message","role":"user","content":"q"}]}`))
	ex.onServer("s->c", []byte(`{"type":"response.output_text.delta","delta":"`+strings.Repeat("@", wsMaxRespText*2)+`"}`))
	ex.onServer("s->c", []byte(`{"type":"response.completed"}`))

	got := rec.all()
	if len(got) != 1 {
		t.Fatalf("expected 1 stored exchange, got %d", len(got))
	}
	if n := strings.Count(string(got[0].RespBody), "@"); n > wsMaxRespText {
		t.Errorf("accumulated text %d bytes exceeds cap %d", n, wsMaxRespText)
	}
}

func TestWSExchangeReasoningOnlySkipped(t *testing.T) {
	rec := &recordStore{}
	ex := &wsExchange{p: &Proxy{store: rec, agentName: "codex"}}

	// A reasoning-only cycle: request + completion with no output_text deltas.
	ex.onClient("c->s", []byte(`{"type":"response.create","input":[{"type":"message","role":"user","content":"hi"}]}`))
	ex.onServer("s->c", []byte(`{"type":"response.output_item.added","item":{"type":"reasoning"}}`))
	ex.onServer("s->c", []byte(`{"type":"response.completed","response":{"output":[]}}`))
	if n := len(rec.all()); n != 0 {
		t.Fatalf("reasoning-only cycle should store nothing, got %d", n)
	}

	// Then the answer cycle stores normally, and the accumulator was reset.
	ex.onServer("s->c", []byte(`{"type":"response.output_text.delta","delta":"answer"}`))
	ex.onServer("s->c", []byte(`{"type":"response.completed"}`))
	got := rec.all()
	if len(got) != 1 || !bytes.Contains(got[0].RespBody, []byte("answer")) {
		t.Fatalf("answer cycle should store 'answer' exactly once, got %d: %+v", len(got), got)
	}
}

func TestWSExchangeCompletionStoredOnce(t *testing.T) {
	rec := &recordStore{}
	ex := &wsExchange{p: &Proxy{store: rec, agentName: "codex"}}

	// One turn, delivered twice: a redelivered response.completed, or a
	// connection that resumes and restreams the tail of the turn. The second
	// delivery finds no request left to pair with, so it stores nothing —
	// without consuming lastReq it would pair the replayed answer with the
	// question of a turn that is already stored.
	ex.onClient("c->s", []byte(`{"type":"response.create","input":[{"type":"message","role":"user","content":"what is 2+2"}]}`))
	ex.onServer("s->c", []byte(`{"type":"response.output_text.delta","delta":"4"}`))
	ex.onServer("s->c", []byte(`{"type":"response.completed"}`))
	ex.onServer("s->c", []byte(`{"type":"response.output_text.delta","delta":"4"}`))
	ex.onServer("s->c", []byte(`{"type":"response.completed"}`))
	if got := rec.all(); len(got) != 1 {
		t.Fatalf("a replayed completion should not store a second exchange, got %d: %+v", len(got), got)
	}

	// The next turn on the same connection still pairs its own request.
	ex.onClient("c->s", []byte(`{"type":"response.create","input":[{"type":"message","role":"user","content":"what is 3+3"}]}`))
	ex.onServer("s->c", []byte(`{"type":"response.output_text.delta","delta":"6"}`))
	ex.onServer("s->c", []byte(`{"type":"response.completed"}`))
	got := rec.all()
	if len(got) != 2 {
		t.Fatalf("expected 2 stored exchanges after the second turn, got %d: %+v", len(got), got)
	}
	if !bytes.Contains(got[1].ReqBody, []byte("3+3")) || !bytes.Contains(got[1].RespBody, []byte("6")) {
		t.Errorf("second turn paired the wrong request/answer: %s / %s", got[1].ReqBody, got[1].RespBody)
	}
}

func TestReadHeaderBlockBoundedMidLine(t *testing.T) {
	// A backend that streams bytes with no '\n' must not grow the accumulator
	// without bound: the 64 KiB cap fires mid-line and returns ErrShortBuffer.
	huge := bytes.Repeat([]byte("x"), 256<<10) // no newline anywhere
	out, err := readHeaderBlock(bufio.NewReader(bytes.NewReader(huge)))
	if err != io.ErrShortBuffer {
		t.Fatalf("expected io.ErrShortBuffer for endless no-newline stream, got %v", err)
	}
	if len(out) > 64<<10+4096 {
		t.Fatalf("accumulator exceeded bound: %d bytes", len(out))
	}
}

func TestReadHeaderBlockReadsBlock(t *testing.T) {
	// A normal 101 handshake block is returned verbatim through the end marker.
	raw := "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nSec-WebSocket-Accept: abc\r\n\r\n"
	out, err := readHeaderBlock(bufio.NewReader(strings.NewReader(raw + "framedata")))
	if err != nil {
		t.Fatalf("readHeaderBlock: %v", err)
	}
	if string(out) != raw {
		t.Errorf("header block = %q, want %q", out, raw)
	}
}

func FuzzReadHeaderBlock(f *testing.F) {
	f.Add([]byte("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\n\r\n"))
	f.Add([]byte("no terminator"))
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, data []byte) {
		// Arbitrary header bytes must never panic and the returned block must not
		// exceed the cap.
		out, _ := readHeaderBlock(bufio.NewReader(bytes.NewReader(data)))
		if len(out) > 64<<10+16 {
			t.Fatalf("header block exceeded cap: %d", len(out))
		}
	})
}

func FuzzWSExchangeMessages(f *testing.F) {
	f.Add(`{"type":"response.create","input":[]}`, `{"type":"response.output_text.delta","delta":"x"}`)
	f.Add(`garbage`, `{"type":"response.completed"}`)
	f.Add(``, ``)
	f.Fuzz(func(t *testing.T, client, server string) {
		// Arbitrary JSON (or non-JSON) on either direction must never panic.
		ex := &wsExchange{p: &Proxy{store: &recordStore{}, agentName: "x"}}
		ex.onClient("c->s", []byte(client))
		ex.onServer("s->c", []byte(server))
		ex.onServer("s->c", []byte(`{"type":"response.completed"}`))
	})
}

type errWriter struct{}

func (errWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestSpliceCopyTap(t *testing.T) {
	t.Run("forwards and taps", func(t *testing.T) {
		var dst bytes.Buffer
		tap := make(chan []byte, 8)
		spliceCopyTap(&dst, strings.NewReader("hello world"), tap, "test", "req-test")
		if dst.String() != "hello world" {
			t.Errorf("forwarded %q, want %q", dst.String(), "hello world")
		}
		var tapped []byte
		for c := range tap { // closed by spliceCopyTap on EOF
			tapped = append(tapped, c...)
		}
		if string(tapped) != "hello world" {
			t.Errorf("tapped %q, want %q", tapped, "hello world")
		}
	})

	t.Run("nil tap forwards only", func(t *testing.T) {
		var dst bytes.Buffer
		spliceCopyTap(&dst, strings.NewReader("data"), nil, "test", "req-test")
		if dst.String() != "data" {
			t.Errorf("forwarded %q, want %q", dst.String(), "data")
		}
	})

	t.Run("backpressure abandons tap, keeps forwarding", func(t *testing.T) {
		var dst bytes.Buffer
		tap := make(chan []byte) // unbuffered, never drained → first send hits default
		spliceCopyTap(&dst, strings.NewReader("keep forwarding"), tap, "test", "req-test")
		if dst.String() != "keep forwarding" {
			t.Errorf("forwarding must continue after tap abandon, got %q", dst.String())
		}
		if _, ok := <-tap; ok {
			t.Error("tap should be closed after backpressure abandon")
		}
	})

	t.Run("write error closes tap and returns", func(t *testing.T) {
		tap := make(chan []byte, 1)
		spliceCopyTap(errWriter{}, strings.NewReader("x"), tap, "test", "req-test")
		if _, ok := <-tap; ok {
			t.Error("tap should be closed after write error")
		}
	})
}

func TestRunWSParser(t *testing.T) {
	// Feed two complete text frames through the channel; the parser must
	// reassemble both and hand each to onMessage, then return on channel close.
	frames := append(
		wsBuildFrame(wsOpText, []byte(`{"type":"a"}`), false, true, false),
		wsBuildFrame(wsOpText, []byte(`{"type":"b"}`), false, true, false)...,
	)
	ch := make(chan []byte, 2)
	ch <- frames
	close(ch)

	var got []string
	runWSParser("req-test", "s->c", ch, false, func(_ string, msg []byte) {
		got = append(got, string(msg))
	})
	if len(got) != 2 || got[0] != `{"type":"a"}` || got[1] != `{"type":"b"}` {
		t.Fatalf("expected two reassembled messages, got %v", got)
	}
}

// TestRunWSParserDebugLogs pins the wsDebug gate: with it on, the parser logs
// one line per reassembled message carrying the direction, the decoded message
// type and the byte count, and still delivers the message. Asserting only the
// delivery (as a bare onMessage counter) would pass whether or not the logging
// exists, so the log records are decoded and checked structurally.
func TestRunWSParserDebugLogs(t *testing.T) {
	defer func(prev bool) { wsDebug = prev }(wsDebug)
	wsDebug = true

	logs := captureLogs(t)

	ch := make(chan []byte, 1)
	ch <- wsBuildFrame(wsOpText, []byte(`{"type":"gw.message"}`), false, true, false)
	close(ch)

	var n int
	runWSParser("req-test", "s->c", ch, false, func(_ string, _ []byte) { n++ })
	if n != 1 {
		t.Fatalf("expected one delivered message, got %d", n)
	}

	var found bool
	for line := range strings.SplitSeq(strings.TrimSpace(logs.String()), "\n") {
		if line == "" {
			continue
		}
		var rec struct {
			Msg   string `json:"msg"`
			ReqID string `json:"request_id"`
			Dir   string `json:"dir"`
			Type  string `json:"type"`
			Bytes int    `json:"bytes"`
		}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line is not valid JSON: %v (%q)", err, line)
		}
		if rec.Msg != "ws message" {
			continue
		}
		found = true
		if rec.ReqID != "req-test" {
			t.Errorf("logged request_id = %q, want %q; a frame line with no correlation ID cannot be tied to a turn", rec.ReqID, "req-test")
		}
		if rec.Dir != "s->c" {
			t.Errorf("logged dir = %q, want %q", rec.Dir, "s->c")
		}
		if rec.Type != "gw.message" {
			t.Errorf("logged type = %q, want %q", rec.Type, "gw.message")
		}
		if rec.Bytes != len(`{"type":"gw.message"}`) {
			t.Errorf("logged bytes = %d, want %d", rec.Bytes, len(`{"type":"gw.message"}`))
		}
	}
	if !found {
		t.Fatalf("wsDebug is on but no %q record was logged; got:\n%s", "ws message", logs.String())
	}
}

// TestRunWSParserDebugOff pins the other side of the gate: with wsDebug off the
// parser must stay silent, so the flag reads the debug level and not merely
// "something got logged".
func TestRunWSParserDebugOff(t *testing.T) {
	defer func(prev bool) { wsDebug = prev }(wsDebug)
	wsDebug = false

	logs := captureLogs(t)

	ch := make(chan []byte, 1)
	ch <- wsBuildFrame(wsOpText, []byte(`{"type":"gw.message"}`), false, true, false)
	close(ch)

	runWSParser("req-test", "s->c", ch, false, func(_ string, _ []byte) {})
	if s := logs.String(); strings.Contains(s, "ws message") {
		t.Errorf("wsDebug is off but the parser logged: %s", s)
	}
}

func TestRunWSParserDecodeErrorStops(t *testing.T) {
	// A compressed (rsv1) frame with a corrupt deflate payload on a deflate
	// connection makes the assembler error; the parser must stop (no delivery),
	// while a following valid frame is never reached.
	bad := wsBuildFrame(wsOpText, []byte{0xde, 0xad, 0xbe, 0xef, 0x00}, false, true, true)
	good := wsBuildFrame(wsOpText, []byte(`{"type":"after"}`), false, true, false)
	ch := make(chan []byte, 1)
	ch <- append(bad, good...)
	close(ch)

	var n int
	runWSParser("req-test", "s->c", ch, true, func(_ string, _ []byte) { n++ })
	if n != 0 {
		t.Fatalf("decode error must stop the parser before any delivery, got %d", n)
	}
}

func TestWSMessageType(t *testing.T) {
	cases := map[string]string{
		`{"type":"response.create","input":[]}`: "response.create",
		`{"type":"gw.message","payload":{}}`:    "gw.message",
		`{"foo":"bar"}`:                         "",
		`{"type":123}`:                          "", // non-string type
		`not json`:                              "",
		``:                                      "",
		`[]`:                                    "",
	}
	for in, want := range cases {
		if got := wsMessageType([]byte(in)); got != want {
			t.Errorf("wsMessageType(%q) = %q, want %q", in, got, want)
		}
	}
}

func FuzzWSMessageType(f *testing.F) {
	f.Add(`{"type":"response.completed"}`)
	f.Add(`{"type":["x"]}`)
	f.Add(`garbage`)
	f.Add(``)
	f.Fuzz(func(t *testing.T, data string) {
		// Arbitrary bytes must never panic; result is only ever the type field.
		_ = wsMessageType([]byte(data))
	})
}

func TestWSExchangeNoRequestNoStore(t *testing.T) {
	rec := &recordStore{}
	ex := &wsExchange{p: &Proxy{store: rec, agentName: "codex"}}
	// Response without any observed request: nothing to pair, store nothing.
	ex.onServer("s->c", []byte(`{"type":"response.output_text.delta","delta":"orphan"}`))
	ex.onServer("s->c", []byte(`{"type":"response.completed"}`))
	if n := len(rec.all()); n != 0 {
		t.Fatalf("response with no request should store nothing, got %d", n)
	}
}

// TestWSExchangeDeltaCapClamped pins that accumulated assistant text stays
// within wsMaxRespText even when a single delta exceeds it, and that the cap
// never lands mid-rune. The previous form appended each delta whole, so one
// oversized delta carried the accumulator past the cap by its full length; the
// multi-byte delta here is the case that would also leave a partial rune, which
// marshals as U+FFFD into stored memory.
//
// The assertions read the stored body, not ex.respText: response.completed
// resets the accumulator, so reading it after the store would always see zero.
// ValidString alone proves nothing here either, since json.Unmarshal repairs
// invalid bytes to U+FFFD, so the rune count and the replacement characters
// are what distinguish a clean clamp from a split rune.
func TestWSExchangeDeltaCapClamped(t *testing.T) {
	rec := &recordStore{}
	ex := &wsExchange{p: &Proxy{store: rec, agentName: "codex"}, target: "t"}

	ex.onClient("c->s", []byte(`{"type":"response.create","model":"gpt-5","input":[{"type":"message","role":"user","content":"q"}]}`))
	// Each delta is 3 bytes per CJK rune, so the cap lands mid-sequence.
	big := strings.Repeat("日", wsMaxRespText/3+100)
	for range 4 {
		ex.onServer("s->c", []byte(`{"type":"response.output_text.delta","delta":`+mustJSON(t, big)+`}`))
	}
	ex.onServer("s->c", []byte(`{"type":"response.completed"}`))

	got := rec.all()
	if len(got) != 1 {
		t.Fatalf("expected 1 stored exchange, got %d", len(got))
	}
	var body struct {
		Content []struct{ Text string } `json:"content"`
	}
	if err := json.Unmarshal(got[0].RespBody, &body); err != nil {
		t.Fatalf("stored response body is not valid JSON: %v", err)
	}
	if len(body.Content) != 1 {
		t.Fatalf("expected 1 content block, got %d", len(body.Content))
	}
	text := body.Content[0].Text
	// The clamp sheds only the trailing bytes that do not complete a rune.
	want := wsMaxRespText - wsMaxRespText%3
	if len(text) != want {
		t.Fatalf("stored %d bytes, want %d (cap %d, %d bytes per rune)",
			len(text), want, wsMaxRespText, len("日"))
	}
	if n := utf8.RuneCountInString(text); n != len(text)/3 {
		t.Errorf("stored %d runes from %d bytes: the cap split a multi-byte sequence", n, len(text))
	}
	if n := strings.Count(text, "�"); n != 0 {
		t.Errorf("stored %d U+FFFD replacement characters, want 0: %q", n, text[len(text)-8:])
	}
	// The cap truncates the tail; the head must survive intact.
	if !strings.HasPrefix(big, text) {
		t.Error("stored text is not a prefix of the deltas it was fed")
	}
}

func mustJSON(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

// TestSpliceWithCaptureBackendHandshakeTimeout pins the bound on waiting for the
// backend's reply to an upgrade request. The dial is already bounded
// (tunnelDialTimeout), but a backend that accepts the connection and then stays
// silent would otherwise pin the hijacked client conn, the backend conn, and the
// serving goroutine forever: readHeaderBlock blocks with no deadline, and the
// function never reaches the splices that would unblock it.
func TestSpliceWithCaptureBackendHandshakeTimeout(t *testing.T) {
	// Backend: accepts the connection and never writes a byte.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			accepted <- nil
			return
		}
		accepted <- c
	}()

	client, clientPeer := net.Pipe()
	defer clientPeer.Close()
	backend, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	held := <-accepted
	if held == nil {
		t.Fatal("backend accept failed")
	}
	defer held.Close()

	p := &Proxy{upgradeHandshakeTimeout: 200 * time.Millisecond}
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.spliceWithCapture(client, bufio.NewReader(client), backend, "silent:443", "req-test")
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("spliceWithCapture blocked on a silent backend: the upgrade handshake read is unbounded")
	}
}
