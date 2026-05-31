package proxy

import (
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
