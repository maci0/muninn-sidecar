package proxy

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"errors"
	"io"
)

// WebSocket (RFC 6455) frame decoding + permessage-deflate (RFC 7692) inflation,
// used to observe an intercepted MITM upgrade so the agent's WebSocket-framed
// exchange can be captured. This only parses framing and decompresses text
// messages — it never drives forwarding (the splice forwards bytes verbatim and
// feeds a copy here best-effort), so a decode error or backpressure can never
// affect the agent's connection.

// WebSocket opcodes (RFC 6455 §5.2).
const (
	wsOpContinuation = 0x0
	wsOpText         = 0x1
	wsOpBinary       = 0x2
	wsOpClose        = 0x8
	wsOpPing         = 0x9
	wsOpPong         = 0xA
)

// wsMaxMessage caps a reassembled message; larger messages are dropped from
// capture (not from forwarding) to bound memory.
const wsMaxMessage = 8 << 20 // 8 MiB

// wsDeflateTail is the empty-stored-block sync-flush marker that permessage-
// deflate strips from each frame on the wire; the receiver appends it back
// before inflating (RFC 7692 §7.2.2).
var wsDeflateTail = []byte{0x00, 0x00, 0xff, 0xff}

// wsFrame is one parsed frame with its application payload already unmasked.
type wsFrame struct {
	fin     bool
	rsv1    bool // permessage-deflate: set on the first frame of a compressed message
	opcode  byte
	payload []byte
}

// readWSFrame reads a single frame from r. Client→server frames are masked; the
// payload is returned unmasked. It bounds the payload it buffers to wsMaxMessage.
func readWSFrame(r io.Reader) (wsFrame, error) {
	var hdr [2]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return wsFrame{}, err
	}
	f := wsFrame{
		fin:    hdr[0]&0x80 != 0,
		rsv1:   hdr[0]&0x40 != 0,
		opcode: hdr[0] & 0x0f,
	}
	masked := hdr[1]&0x80 != 0
	length := uint64(hdr[1] & 0x7f)
	switch length {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(r, ext[:]); err != nil {
			return wsFrame{}, err
		}
		length = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(r, ext[:]); err != nil {
			return wsFrame{}, err
		}
		length = binary.BigEndian.Uint64(ext[:])
	}
	var mask [4]byte
	if masked {
		if _, err := io.ReadFull(r, mask[:]); err != nil {
			return wsFrame{}, err
		}
	}
	if length > wsMaxMessage {
		return wsFrame{}, errors.New("ws: frame exceeds max message size")
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(r, payload); err != nil {
		return wsFrame{}, err
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i&3]
		}
	}
	f.payload = payload
	return f, nil
}

// wsInflater inflates a permessage-deflate message stream with context takeover:
// each message is inflated using the previous decompressed output as the LZ77
// dictionary (the sliding window), which is exactly what context-takeover means.
type wsInflater struct {
	history []byte // last 32 KiB of decompressed output, used as the next dict
}

// inflate decompresses one message's frame payload (the on-wire bytes, with the
// sync-flush tail already stripped per RFC 7692).
func (w *wsInflater) inflate(payload []byte) ([]byte, error) {
	src := io.MultiReader(bytes.NewReader(payload), bytes.NewReader(wsDeflateTail))
	fr := flate.NewReaderDict(src, w.history)
	out, err := io.ReadAll(fr)
	fr.Close()
	// The appended sync-flush flushes all of the message's output, then the
	// source EOFs without a final deflate block — flate reports ErrUnexpectedEOF
	// (or EOF) but `out` holds the complete message. Only other errors are real.
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return nil, err
	}
	// Advance the sliding window (context takeover).
	w.history = append(w.history, out...)
	if len(w.history) > 32*1024 {
		w.history = w.history[len(w.history)-32*1024:]
	}
	return out, nil
}

// wsMessageAssembler reassembles fragmented frames into complete text messages
// for one direction, inflating permessage-deflate when negotiated. It ignores
// binary, control, and oversized messages. emit is called with each complete
// text message's UTF-8 payload.
type wsMessageAssembler struct {
	deflate bool // permessage-deflate negotiated for this connection
	infl    wsInflater

	inMsg      bool
	msgRSV1    bool // first frame of the current message had RSV1 (compressed)
	msgIsText  bool
	buf        []byte
	overflowed bool
}

// add feeds one frame; returns the complete text message (decompressed) when a
// FIN frame finishes one, else nil. Control frames (ping/pong/close) are skipped.
func (a *wsMessageAssembler) add(f wsFrame) ([]byte, error) {
	switch f.opcode {
	case wsOpPing, wsOpPong, wsOpClose:
		return nil, nil // control frames don't interrupt a fragmented data message
	case wsOpText, wsOpBinary:
		a.inMsg = true
		a.msgRSV1 = f.rsv1
		a.msgIsText = f.opcode == wsOpText
		a.buf = a.buf[:0]
		a.overflowed = false
	case wsOpContinuation:
		if !a.inMsg {
			return nil, nil // stray continuation; ignore
		}
	default:
		return nil, nil // reserved opcode; ignore
	}

	if !a.overflowed {
		if len(a.buf)+len(f.payload) > wsMaxMessage {
			a.overflowed = true
			a.buf = a.buf[:0] // drop partial; too big to capture
		} else {
			a.buf = append(a.buf, f.payload...)
		}
	}

	if !f.fin {
		return nil, nil
	}

	// Message complete.
	done := a.inMsg
	textOnly := a.msgIsText
	compressed := a.msgRSV1
	over := a.overflowed
	data := a.buf
	a.inMsg = false
	if !done || !textOnly || over {
		return nil, nil // only capture complete, non-oversized text messages
	}
	if a.deflate && compressed {
		out, err := a.infl.inflate(data)
		if err != nil {
			return nil, err // deflate desync: caller stops capturing this stream
		}
		return out, nil
	}
	// Uncompressed text (or a non-compressed message on a deflate connection).
	return append([]byte(nil), data...), nil
}
