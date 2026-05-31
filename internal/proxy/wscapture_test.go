package proxy

import (
	"bufio"
	"bytes"
	"io"
	"strings"
	"sync"
	"testing"

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
	ex := &wsExchange{p: &Proxy{store: rec, agentName: "codex"}, target: "chatgpt.com:443"}

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
		spliceCopyTap(&dst, strings.NewReader("hello world"), tap)
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
		spliceCopyTap(&dst, strings.NewReader("data"), nil)
		if dst.String() != "data" {
			t.Errorf("forwarded %q, want %q", dst.String(), "data")
		}
	})

	t.Run("backpressure abandons tap, keeps forwarding", func(t *testing.T) {
		var dst bytes.Buffer
		tap := make(chan []byte) // unbuffered, never drained → first send hits default
		spliceCopyTap(&dst, strings.NewReader("keep forwarding"), tap)
		if dst.String() != "keep forwarding" {
			t.Errorf("forwarding must continue after tap abandon, got %q", dst.String())
		}
		if _, ok := <-tap; ok {
			t.Error("tap should be closed after backpressure abandon")
		}
	})

	t.Run("write error closes tap and returns", func(t *testing.T) {
		tap := make(chan []byte, 1)
		spliceCopyTap(errWriter{}, strings.NewReader("x"), tap)
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
	runWSParser("s->c", ch, false, func(_ string, msg []byte) {
		got = append(got, string(msg))
	})
	if len(got) != 2 || got[0] != `{"type":"a"}` || got[1] != `{"type":"b"}` {
		t.Fatalf("expected two reassembled messages, got %v", got)
	}
}

func TestRunWSParserDebugLogs(t *testing.T) {
	// With wsDebug on, the parser logs each message's type but still delivers it.
	defer func(prev bool) { wsDebug = prev }(wsDebug)
	wsDebug = true

	ch := make(chan []byte, 1)
	ch <- wsBuildFrame(wsOpText, []byte(`{"type":"gw.message"}`), false, true, false)
	close(ch)

	var n int
	runWSParser("s->c", ch, false, func(_ string, _ []byte) { n++ })
	if n != 1 {
		t.Fatalf("expected one delivered message, got %d", n)
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
	runWSParser("s->c", ch, true, func(_ string, _ []byte) { n++ })
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
