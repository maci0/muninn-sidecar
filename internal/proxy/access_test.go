package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/maci0/muninn-sidecar/internal/stats"
)

// waitForLogs polls until the buffer holds a line whose msg is want. The turn
// line is written by a deferred handler that runs after the client has already
// been answered, so the wait is what the test needs; the buffer itself is
// mutex-guarded (logbuf_test.go), since polling does not remove the race.
func waitForLogs(t *testing.T, logs *logBuffer, msg string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
			var rec map[string]any
			if json.Unmarshal([]byte(line), &rec) == nil && rec["msg"] == msg {
				return rec
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no %q line was logged within 2s:\n%s", msg, logs.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// turnRecords returns the parsed "turn" lines from a captured log buffer.
func turnRecords(t *testing.T, logs *logBuffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line is not valid JSON: %v (%q)", err, line)
		}
		if rec["msg"] == "turn" {
			out = append(out, rec)
		}
	}
	return out
}

// TestTurnLineReportsOutcome pins the per-turn access line: one line per turn
// carrying the correlation ID, the status it ended with, and how long it took.
// Before this line existed, duration_ms appeared only on the upstream-error
// warning, so a successful turn left no trace of how long it took or that it
// had finished at all.
func TestTurnLineReportsOutcome(t *testing.T) {
	logs := captureLogs(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"id":"msg_1","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer upstream.Close()

	p, err := New(Config{ListenAddr: "127.0.0.1:0", Upstream: upstream.URL, AgentName: "test-agent"})
	if err != nil {
		t.Fatal(err)
	}
	addr, err := p.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer p.Shutdown(t.Context())

	resp, err := http.Post("http://"+addr+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	got := waitForLogs(t, logs, "turn")
	for field, want := range map[string]any{
		"request_id": got["request_id"], // checked below, only presence matters
		"status":     float64(200),
		"method":     "POST",
		"path":       "/v1/messages",
		"agent":      "test-agent",
	} {
		if field == "request_id" {
			if s, _ := got[field].(string); s == "" {
				t.Errorf("turn line has no request_id: %v", got)
			}
			continue
		}
		if got[field] != want {
			t.Errorf("turn line %q = %v, want %v", field, got[field], want)
		}
	}
	if _, ok := got["duration_ms"]; !ok {
		t.Errorf("turn line has no duration_ms: %v", got)
	}
}

// TestTurnLineOnUpstreamError proves the same line reports a failed turn, at
// error level so a 5xx is not buried under the per-stage debug trace.
func TestTurnLineOnUpstreamError(t *testing.T) {
	logs := captureLogs(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer upstream.Close()

	p, err := New(Config{ListenAddr: "127.0.0.1:0", Upstream: upstream.URL, AgentName: "test-agent"})
	if err != nil {
		t.Fatal(err)
	}
	addr, err := p.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer p.Shutdown(t.Context())

	resp, err := http.Post("http://"+addr+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"m","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	got := waitForLogs(t, logs, "turn")
	if got["status"] != float64(500) {
		t.Errorf("turn status = %v, want 500", got["status"])
	}
	if got["level"] != "ERROR" {
		t.Errorf("a 5xx turn logged at level %v, want ERROR", got["level"])
	}
}

// TestRequestRecorderStatus pins the status recorder through the wrapper
// itself, including the implicit 200 of a handler that writes a body without
// calling WriteHeader (how the reverse proxy copies an upstream response) and
// the 200 of a handler that writes nothing at all.
func TestRequestRecorderStatus(t *testing.T) {
	t.Run("implicit 200 on write", func(t *testing.T) {
		rec := newRequestRecorder(httptest.NewRecorder())
		if _, err := rec.Write([]byte("body")); err != nil {
			t.Fatal(err)
		}
		if rec.statusCode() != 200 {
			t.Errorf("status = %d, want 200", rec.statusCode())
		}
		if !rec.committed {
			t.Error("a written body must count as committed")
		}
	})
	t.Run("nothing written", func(t *testing.T) {
		rec := newRequestRecorder(httptest.NewRecorder())
		if rec.statusCode() != 200 {
			t.Errorf("status = %d, want 200", rec.statusCode())
		}
		if rec.committed {
			t.Error("a handler that wrote nothing must not count as committed")
		}
	})
	t.Run("explicit status", func(t *testing.T) {
		rec := newRequestRecorder(httptest.NewRecorder())
		rec.WriteHeader(429)
		rec.Write([]byte("body"))
		if rec.statusCode() != 429 {
			t.Errorf("status = %d, want 429", rec.statusCode())
		}
	})
}

// TestPanicSurfacesStructuredAndProxySurvives pins that a panic escaping the
// request pipeline reaches the structured handler. ErrorLog used to be nil, so
// the stdlib fell back to the log package and wrote the panic as unstructured
// text straight to stderr: a --log-json run got one unparseable line in the
// stream, and the level filter set up in main.go never applied to it.
func TestPanicSurfacesStructuredAndProxySurvives(t *testing.T) {
	logs := captureLogs(t)

	var calls int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			panic("boom")
		}
		w.Write([]byte(`{"id":"msg_2"}`))
	}))
	defer upstream.Close()

	p, err := New(Config{ListenAddr: "127.0.0.1:0", Upstream: upstream.URL, AgentName: "test-agent"})
	if err != nil {
		t.Fatal(err)
	}
	addr, err := p.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer p.Shutdown(t.Context())

	resp, err := http.Post("http://"+addr+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"m","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	// Every line in the buffer parses as JSON: a nil ErrorLog would leave the
	// stdlib's plain-text panic line in here and fail to unmarshal.
	var sawPanic bool
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line is not structured JSON: %v (%q)", err, line)
		}
		if s, _ := rec["msg"].(string); strings.Contains(s, "panic serving") {
			sawPanic = true
			if !strings.Contains(s, "boom") {
				t.Errorf("panic line does not name the panic: %q", s)
			}
		}
	}
	if !sawPanic {
		t.Fatalf("no panic line reached the structured handler:\n%s", logs.String())
	}

	// The proxy is still serving afterwards.
	resp2, err := http.Post("http://"+addr+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"m","messages":[]}`))
	if err != nil {
		t.Fatalf("proxy stopped serving after a panic: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != 200 {
		t.Errorf("turn after a panic = %d, want 200", resp2.StatusCode)
	}
}

// TestUncapturableCounted pins that a response the proxy cannot decode is
// counted, not just logged at debug. Without the counter an upstream answering
// in brotli or gRPC reports the same healthy zero-save counters as one that is
// storing every turn.
func TestUncapturableCounted(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/grpc+proto")
		w.Write([]byte("not json"))
	}))
	defer upstream.Close()

	st := &stats.Stats{}
	p, err := New(Config{
		ListenAddr: "127.0.0.1:0", Upstream: upstream.URL, AgentName: "test-agent", Stats: st,
	})
	if err != nil {
		t.Fatal(err)
	}
	addr, err := p.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer p.Shutdown(t.Context())

	resp, err := http.Post("http://"+addr+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"m","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	// ModifyResponse runs on the client's goroutine before the body is relayed,
	// so the counter is already set by the time the response completes.
	if n := st.Snapshot().Uncapturable; n != 1 {
		t.Errorf("Uncapturable = %d, want 1; an undecodable response is silently lost otherwise", n)
	}
	// The operator sees it on the status endpoint, not only in a debug line.
	if reasons := degradedReasons(st.Snapshot(), storeQueue{}); len(reasons) == 0 {
		t.Error("an undecodable response left the status endpoint reporting healthy")
	}
}

// A provider that refuses most turns is still a working sidecar: the 4xx/5xx
// reached the agent unchanged, so degraded must stay false however high the
// upstream-error ratio climbs.
func TestDegradedIgnoresUpstreamErrors(t *testing.T) {
	st := &stats.Stats{}
	st.Requests.Add(2)
	st.UpstreamErrors.Add(2)
	if reasons := degradedReasons(st.Snapshot(), storeQueue{}); len(reasons) != 0 {
		t.Errorf("degraded_reasons = %v, want none: an upstream 4xx/5xx is the provider's answer", reasons)
	}

	// A failure msc caused itself is the other half of the contract.
	st.ProxyErrors.Add(1)
	if reasons := degradedReasons(st.Snapshot(), storeQueue{}); len(reasons) != 1 {
		t.Errorf("degraded_reasons = %v, want the proxy transport error", reasons)
	}
}
