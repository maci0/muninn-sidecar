package proxy

import (
	"bytes"
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
