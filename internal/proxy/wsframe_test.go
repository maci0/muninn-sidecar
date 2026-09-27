package proxy

import (
	"bufio"
	"bytes"
	"compress/flate"
	"encoding/binary"
	"strings"
	"testing"
)

// wsBuildFrame assembles a single on-wire frame (test helper).
func wsBuildFrame(opcode byte, payload []byte, masked, fin, rsv1 bool) []byte {
	b0 := opcode
	if fin {
		b0 |= 0x80
	}
	if rsv1 {
		b0 |= 0x40
	}
	out := []byte{b0}
	b1 := byte(0)
	if masked {
		b1 |= 0x80
	}
	n := len(payload)
	switch {
	case n < 126:
		out = append(out, b1|byte(n))
	case n < 65536:
		out = append(out, b1|126)
		var e [2]byte
		binary.BigEndian.PutUint16(e[:], uint16(n))
		out = append(out, e[:]...)
	default:
		out = append(out, b1|127)
		var e [8]byte
		binary.BigEndian.PutUint64(e[:], uint64(n))
		out = append(out, e[:]...)
	}
	pl := append([]byte(nil), payload...)
	if masked {
		key := []byte{0xAA, 0xBB, 0xCC, 0xDD}
		out = append(out, key...)
		for i := range pl {
			pl[i] ^= key[i&3]
		}
	}
	return append(out, pl...)
}

func TestReadWSFrame(t *testing.T) {
	cases := []struct {
		name             string
		payload          string
		masked, fin, rsv bool
	}{
		{"short unmasked", "hello", false, true, false},
		{"short masked (client->server)", "hi there", true, true, false},
		{"rsv1 compressed flag", "x", false, true, true},
		{"medium 200 bytes", strings.Repeat("ab", 100), true, true, false},
		{"not fin (fragment)", "frag", false, false, false},
		{"empty", "", true, true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			raw := wsBuildFrame(wsOpText, []byte(c.payload), c.masked, c.fin, c.rsv)
			f, err := readWSFrame(bytes.NewReader(raw))
			if err != nil {
				t.Fatalf("readWSFrame: %v", err)
			}
			if string(f.payload) != c.payload {
				t.Errorf("payload = %q, want %q (masked=%v)", f.payload, c.payload, c.masked)
			}
			if f.fin != c.fin || f.rsv1 != c.rsv || f.opcode != wsOpText {
				t.Errorf("fin=%v rsv1=%v op=%d, want fin=%v rsv1=%v op=text", f.fin, f.rsv1, f.opcode, c.fin, c.rsv)
			}
		})
	}
}

// wsDeflateMessages compresses messages permessage-deflate-style with context
// takeover (a single flate.Writer keeps its window across Flush boundaries),
// returning each message's on-wire frame payload (sync-flush tail stripped).
func wsDeflateMessages(t *testing.T, msgs []string) [][]byte {
	t.Helper()
	var b bytes.Buffer
	fw, err := flate.NewWriter(&b, flate.DefaultCompression)
	if err != nil {
		t.Fatal(err)
	}
	var payloads [][]byte
	for _, m := range msgs {
		start := b.Len()
		if _, err := fw.Write([]byte(m)); err != nil {
			t.Fatal(err)
		}
		if err := fw.Flush(); err != nil { // emits compressed bytes ending in 00 00 ff ff
			t.Fatal(err)
		}
		chunk := append([]byte(nil), b.Bytes()[start:]...)
		chunk = bytes.TrimSuffix(chunk, wsDeflateTail)
		payloads = append(payloads, chunk)
	}
	return payloads
}

func TestWSInflateContextTakeover(t *testing.T) {
	// Message 2 repeats message 1's text, so correct inflation requires the
	// LZ77 window from message 1 (context takeover). A naive per-message
	// inflater without the carried-over dictionary would corrupt message 2.
	msgs := []string{
		`{"type":"request","content":"deploy the service to us-east-1 now"}`,
		`{"type":"response","content":"deploy the service to us-east-1 now, confirmed"}`,
		`{"type":"done"}`,
	}
	payloads := wsDeflateMessages(t, msgs)

	var infl wsInflater
	for i, p := range payloads {
		got, err := infl.inflate(p)
		if err != nil {
			t.Fatalf("msg %d inflate: %v", i, err)
		}
		if string(got) != msgs[i] {
			t.Fatalf("msg %d = %q, want %q", i, got, msgs[i])
		}
	}
}

func TestWSInflateBombRejected(t *testing.T) {
	// A tiny compressed payload that inflates past wsMaxMessage must be rejected,
	// not buffered — guards against a permessage-deflate decompression bomb that
	// would otherwise exhaust memory while capturing a connection.
	big := strings.Repeat("a", wsMaxMessage+1024)
	payload := wsDeflateMessages(t, []string{big})[0]
	if len(payload) >= wsMaxMessage {
		t.Fatalf("compressed payload not small enough to be a bomb: %d bytes", len(payload))
	}
	var infl wsInflater
	if _, err := infl.inflate(payload); err == nil {
		t.Fatal("expected error for inflated message exceeding max size, got nil")
	}
}

func TestWSAssembler(t *testing.T) {
	// Build a frame stream: a compressed single text message, a ping (skipped),
	// a fragmented uncompressed text message, and a binary message (ignored).
	a := &wsMessageAssembler{deflate: true}

	// 1) compressed single text message.
	comp := wsDeflateMessages(t, []string{"hello world"})[0]
	if got, err := a.add(wsFrame{fin: true, rsv1: true, opcode: wsOpText, payload: comp}); err != nil {
		t.Fatal(err)
	} else if string(got) != "hello world" {
		t.Errorf("compressed text = %q, want 'hello world'", got)
	}

	// 2) control frame is skipped (no message, no state corruption).
	if got, _ := a.add(wsFrame{fin: true, opcode: wsOpPing, payload: []byte("p")}); got != nil {
		t.Errorf("ping should yield no message, got %q", got)
	}

	// 3) fragmented uncompressed text: "foo"+"bar" across two frames.
	if got, _ := a.add(wsFrame{fin: false, opcode: wsOpText, payload: []byte("foo")}); got != nil {
		t.Errorf("non-fin frame should not emit, got %q", got)
	}
	got, err := a.add(wsFrame{fin: true, opcode: wsOpContinuation, payload: []byte("bar")})
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "foobar" {
		t.Errorf("reassembled = %q, want 'foobar'", got)
	}

	// 4) binary message is ignored (not captured as text).
	if got, _ := a.add(wsFrame{fin: true, opcode: wsOpBinary, payload: []byte{0x01, 0x02}}); got != nil {
		t.Errorf("binary should be ignored, got %q", got)
	}
}

// TestWSAssemblerCompressedBinaryAdvancesDict proves a skipped compressed
// binary message still feeds the inflater so context takeover stays in sync:
// a later compressed text message that back-references the binary message's
// plaintext must decode correctly instead of desyncing.
func TestWSAssemblerCompressedBinaryAdvancesDict(t *testing.T) {
	// Two messages share a long substring; the second (text) back-references the
	// first (binary) through the carried-over LZ77 window.
	shared := "deploy the service to us-east-1 immediately and confirm"
	payloads := wsDeflateMessages(t, []string{shared, shared + " done"})

	a := &wsMessageAssembler{deflate: true}
	// Binary compressed message: not captured, but must advance the dictionary.
	got, err := a.add(wsFrame{fin: true, rsv1: true, opcode: wsOpBinary, payload: payloads[0]})
	if err != nil {
		t.Fatalf("compressed binary add: %v", err)
	}
	if got != nil {
		t.Errorf("compressed binary should not be captured, got %q", got)
	}
	// Text compressed message referencing the binary message's plaintext.
	got, err = a.add(wsFrame{fin: true, rsv1: true, opcode: wsOpText, payload: payloads[1]})
	if err != nil {
		t.Fatalf("compressed text add after binary: %v", err)
	}
	if string(got) != shared+" done" {
		t.Errorf("context takeover desynced after skipped binary: got %q, want %q", got, shared+" done")
	}
}

// TestWSAssemblerOversizedCompressedFatal proves an oversized compressed
// message is treated as an unrecoverable desync (its plaintext can't be
// recovered for the window), so add returns an error to stop capture.
func TestWSAssemblerOversizedCompressedFatal(t *testing.T) {
	a := &wsMessageAssembler{deflate: true}
	// A single compressed frame whose declared payload pushes buf past the cap.
	big := make([]byte, wsMaxMessage+1)
	if _, err := a.add(wsFrame{fin: true, rsv1: true, opcode: wsOpText, payload: big}); err == nil {
		t.Fatal("oversized compressed message should return a fatal desync error, got nil")
	}
}

func FuzzWSInflate(f *testing.F) {
	f.Add([]byte{0x00})
	f.Add([]byte("not a deflate stream"))
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, data []byte) {
		// Arbitrary (possibly corrupt) compressed payloads must never panic, and
		// the context-takeover state must survive a second call.
		var w wsInflater
		_, _ = w.inflate(data)
		_, _ = w.inflate(data)
	})
}

func FuzzReadWSFrame(f *testing.F) {
	f.Add(wsBuildFrame(wsOpText, []byte("hi"), true, true, false))
	f.Add([]byte{0x81, 0x00})
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, data []byte) {
		// Must never panic on arbitrary bytes; a returned frame's payload is
		// within bounds.
		fr, err := readWSFrame(bytes.NewReader(data))
		if err == nil && len(fr.payload) > wsMaxMessage {
			t.Fatalf("payload exceeds max: %d", len(fr.payload))
		}
	})
}

// FuzzWSFrameStream drives the capture-side decode loop (readWSFrame feeding
// wsMessageAssembler.add, the pair runWSParser uses) over an arbitrary frame
// stream. Single-frame fuzzing cannot reach the stateful paths: interleaved
// control frames inside a fragmented message, a stray continuation, an opcode
// that restarts a message mid-reassembly, oversized payloads that flip
// overflowed, and the deflate-desync stop. Invariants: never panic, the
// reassembly buffer and every emitted message stay within wsMaxMessage, and a
// deflate connection must not surface a message after add reported a desync
// (the caller stops the stream, so nothing downstream can observe later state).
func FuzzWSFrameStream(f *testing.F) {
	// A fragmented text message with a ping wedged between fragments.
	f.Add(append(append(wsBuildFrame(wsOpText, []byte("he"), true, false, false),
		wsBuildFrame(wsOpPing, []byte("p"), true, false, false)...),
		wsBuildFrame(wsOpContinuation, []byte("llo"), true, true, false)...), false)
	// A binary message followed by a compressed text message (deflate path).
	f.Add(append(wsBuildFrame(wsOpBinary, []byte{0x00}, true, true, false),
		wsBuildFrame(wsOpText, []byte("x"), true, true, true)...), true)
	f.Add(wsBuildFrame(wsOpContinuation, []byte("orphan"), true, true, false), false)
	f.Add(append(wsBuildFrame(wsOpText, []byte("over"), true, false, true),
		wsBuildFrame(wsOpContinuation, []byte("flow"), true, true, true)...), true)
	f.Add([]byte{}, false)
	f.Fuzz(func(t *testing.T, data []byte, deflate bool) {
		asm := &wsMessageAssembler{deflate: deflate}
		r := bufio.NewReader(bytes.NewReader(data))
		for {
			f, err := readWSFrame(r)
			if err != nil {
				return
			}
			msg, err := asm.add(f)
			if err != nil {
				// A desync is terminal for this stream, exactly as runWSParser
				// treats it: stop feeding frames.
				break
			}
			if len(msg) > wsMaxMessage {
				t.Fatalf("emitted message of %d bytes exceeds cap %d", len(msg), wsMaxMessage)
			}
			if len(asm.buf) > wsMaxMessage {
				t.Fatalf("reassembly buffer %d exceeds cap %d", len(asm.buf), wsMaxMessage)
			}
		}
	})
}

// FuzzWSReassemblyRoundTrip asserts the framing contract itself: a text
// message split across any number of continuation frames, masked and carried on
// the wire, must come back out of readWSFrame + add byte-identical, and no
// fragment before the FIN may yield a message. A fuzzer alone would only catch
// panics here; this is the assertion that makes a mis-ordered or truncated
// reassembly a failure rather than silent capture loss.
func FuzzWSReassemblyRoundTrip(f *testing.F) {
	f.Add("hello", uint8(1), false)
	f.Add("", uint8(3), false)
	f.Add("a longer message with masking and multi-byte runes: héllo ✓", uint8(5), false)
	f.Add("x", uint8(0), true)
	f.Fuzz(func(t *testing.T, payload string, parts uint8, masked bool) {
		n := 1 + int(parts)%8
		chunks := make([]string, 0, n)
		for i := 0; i < n; i++ {
			chunks = append(chunks, payload[i*len(payload)/n:(i+1)*len(payload)/n])
		}
		var wire []byte
		for i, c := range chunks {
			opcode := byte(wsOpContinuation)
			if i == 0 {
				opcode = wsOpText
			}
			wire = append(wire, wsBuildFrame(opcode, []byte(c), masked, i == len(chunks)-1, false)...)
		}
		asm := &wsMessageAssembler{}
		r := bufio.NewReader(bytes.NewReader(wire))
		var got []byte
		seen := 0
		for {
			fr, err := readWSFrame(r)
			if err != nil {
				break
			}
			msg, err := asm.add(fr)
			if err != nil {
				t.Fatalf("uncompressed reassembly error: %v", err)
			}
			if msg == nil {
				continue
			}
			seen++
			if seen > 1 {
				t.Fatal("more than one message from a single fragmented message")
			}
			got = msg
		}
		// An empty text message reassembles to no bytes, and add reports that
		// as "no message", so only a non-empty payload owes exactly one.
		want := 0
		if payload != "" {
			want = 1
		}
		if seen != want {
			t.Fatalf("fragmented text message of %d frame(s) produced %d messages, want %d", len(chunks), seen, want)
		}
		if string(got) != payload {
			t.Fatalf("reassembled %q, want %q", got, payload)
		}
	})
}
