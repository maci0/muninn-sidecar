package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"unicode/utf8"

	"github.com/maci0/muninn-sidecar/internal/mcpclient"
	"github.com/maci0/muninn-sidecar/internal/stats"
)

// FuzzFormatAndDedup exercises the untrusted ingestion pipeline: a captured
// request/response pair (raw intercepted bytes) flows through system-reminder
// stripping, user/assistant extraction, secret redaction, noise filtering,
// concept building, FNV dedup, and tag construction. Invariants: never panic;
// a produced memory has valid-UTF-8 concept/content and at least the three
// base tags. Redaction is toggled to cover both paths.
func FuzzFormatAndDedup(f *testing.F) {
	f.Add(
		`{"messages":[{"role":"user","content":[{"type":"text","text":"how to sort in go"}]}]}`,
		`{"content":[{"type":"text","text":"use sort.Slice"}],"usage":{"output_tokens":5}}`,
		"claude", 200, true)
	f.Add(
		`{"input":"my key is sk-ant-0000000000000000000000000000000000000000000000000000"}`,
		`{"output":[{"type":"message","content":[{"text":"ok"}]}]}`,
		"gemini", 500, true)
	f.Add(`not json`, ``, "", 0, false)
	f.Add(`{"messages":[]}`, `{}`, "codex", 404, false)

	st := &stats.Stats{}
	s := &MuninnStore{stats: st}

	f.Fuzz(func(t *testing.T, req, resp, agent string, status int, redact bool) {
		s.SetRedaction(redact)

		ex := &CapturedExchange{
			Agent:      agent,
			Path:       "/v1/messages",
			StatusCode: status,
			ReqBody:    json.RawMessage(req),
			RespBody:   json.RawMessage(resp),
		}

		// Fresh ring per iteration: dedup state must not leak across fuzz inputs.
		var dedup dedupWindow
		pending := make(map[uint64]struct{})

		fm := s.formatAndDedup(ex, &dedup, pending)
		if fm == nil {
			return // dropped as empty/noise/dup — a valid outcome.
		}

		// Concept and content derive from JSON string fields and TruncateText,
		// both of which must preserve UTF-8 validity (corruption here would
		// poison every stored memory and recall query).
		if !utf8.ValidString(fm.concept) {
			t.Fatalf("concept is not valid UTF-8: %q", fm.concept)
		}
		if !utf8.ValidString(fm.content) {
			t.Fatalf("content is not valid UTF-8: %q", fm.content)
		}
		// buildTags always emits sidecar + agent + status tags.
		if len(fm.tags) < 3 {
			t.Fatalf("expected >= 3 base tags, got %v", fm.tags)
		}

		// A non-nil memory must not be silently empty: at least one side carried
		// content, so the concept is never blank.
		if fm.concept == "" {
			t.Fatalf("non-nil memory has empty concept (req=%q resp=%q)", req, resp)
		}
	})
}

// FuzzRetryable covers the MCP retry decision: which failures justify another
// attempt at the same logical write. 4xx responses and JSON-RPC error objects
// are permanent rejections and must never be retried (each attempt would
// re-push an exchange the server already refused, delaying the drain deadline);
// transport failures and 5xx are transient. Wrapping must not change the
// verdict, since callers add context on the way up.
func FuzzRetryable(f *testing.F) {
	f.Add("client", "")
	f.Add("rpc", "no such tool")
	f.Add("server", "")
	f.Add("net", "connection reset")
	f.Add("none", "")

	f.Fuzz(func(t *testing.T, class, msg string) {
		// Each class pairs the error with the verdict it must produce, so the
		// expectation cannot drift from the input the way a separate bool seed
		// would under mutation.
		var err error
		var wantRetry bool
		switch class {
		case "client":
			err, wantRetry = &mcpclient.ClientError{Status: 400}, false
		case "rpc":
			err, wantRetry = &mcpclient.RPCError{Code: -32602, Message: msg}, false
		case "server":
			err, wantRetry = &mcpclient.ServerError{Status: 503}, true
		case "net":
			err, wantRetry = errors.New(msg), true
		case "none":
			// A nil error is not a failure to retry against: callTool only
			// consults retryable after a failed attempt.
			err, wantRetry = nil, true
		default:
			t.Skip()
		}
		// Callers add context on the way up, so the verdict must survive wrapping.
		// fmt.Errorf cannot wrap nil, so that case is checked as-is.
		target := err
		if err != nil {
			target = fmt.Errorf("flush batch: %w", err)
		}
		if got := retryable(target); got != wantRetry {
			t.Fatalf("retryable(%T(%q)) = %v, want %v", err, msg, got, wantRetry)
		}
	})
}
