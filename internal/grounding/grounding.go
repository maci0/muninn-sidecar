// Package grounding provides an LLM answer-grounding rerank: a judge that
// decides whether a recalled passage actually contains a span answering the
// query. It is the cross-encoder precision step a bi-encoder cosine gate cannot
// do — cosine ranks a same-topic-but-answerless passage as high as the
// answer-bearing one, but a model reading (query, passage) jointly can tell them
// apart (see docs/experiments.md §B2–B4).
//
// Two backends: an OpenAI-compatible HTTP model (fast local judge, ~1s — viable
// in-flight for harm-prone vaults) and a CLI agent (frontier models claude/codex/
// grok, ~3.5s — offline curation only). The injector and the eval tools share it.
package grounding

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"time"
)

// Grounder judges whether a passage answers a query.
type Grounder interface {
	// Grounded reports whether passage contains a span answering query. On any
	// error it returns true (fail-open): a flaky or unavailable judge must never
	// silently drop a real hit, only ever refine precision when it works.
	Grounded(ctx context.Context, query, passage string) bool
	Label() string
}

// Prompt is the shared judgment prompt, calibrated for extractive QA: it accepts
// a passage that merely contains an answer span (not only ones that "directly
// answer"), which avoids over-rejecting long multi-fact passages (§B3).
func Prompt(query, passage string) string {
	return "You are a retrieval grader for extractive QA. Say yes if the passage contains a span of text that could serve as a correct answer to the question, even if other facts surround it. Say no only if the answer is genuinely not present.\n" +
		"Question: " + query + "\n" +
		"Passage: " + passage + "\n" +
		"Reply with exactly one word: yes or no."
}

// ParseYesNo extracts a yes/no decision from model text, scanning from the end
// (agents explain, then conclude). Ambiguous replies default to true (keep) —
// fail-open, consistent with Grounder.Grounded.
func ParseYesNo(s string) bool {
	fields := strings.FieldsFunc(strings.ToLower(strings.TrimSpace(s)), func(r rune) bool {
		return r < 'a' || r > 'z'
	})
	for i := len(fields) - 1; i >= 0; i-- {
		switch fields[i] {
		case "no", "false", "none", "irrelevant":
			return false
		case "yes", "true", "relevant":
			return true
		}
	}
	return true
}

// Filter returns the passages the grounder accepts for the query, judging only
// the first topK (callers pass them pre-sorted by score). A nil grounder or
// empty input is a pass-through.
func Filter(ctx context.Context, g Grounder, query string, passages []string, topK int) []string {
	if g == nil || len(passages) == 0 {
		return passages
	}
	n := topK
	if n <= 0 || n > len(passages) {
		n = len(passages)
	}
	kept := make([]string, 0, len(passages))
	for i := 0; i < n; i++ {
		if g.Grounded(ctx, query, passages[i]) {
			kept = append(kept, passages[i])
		}
	}
	return kept
}

// --- HTTP (OpenAI-compatible) grounder ---

type httpGrounder struct {
	baseURL, key, model string
	timeout             time.Duration
}

func (g *httpGrounder) Label() string { return "http:" + g.model }

func (g *httpGrounder) Grounded(ctx context.Context, query, passage string) bool {
	body, _ := json.Marshal(map[string]any{
		"model":       g.model,
		"messages":    []map[string]string{{"role": "user", "content": Prompt(query, passage)}},
		"temperature": 0,
		"max_tokens":  4,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return true
	}
	req.Header.Set("Content-Type", "application/json")
	if g.key != "" {
		req.Header.Set("Authorization", "Bearer "+g.key)
	}
	resp, err := (&http.Client{Timeout: g.timeout}).Do(req)
	if err != nil {
		return true
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return true
	}
	var out struct {
		Choices []struct {
			Message struct{ Content string } `json:"message"`
		} `json:"choices"`
	}
	if json.Unmarshal(data, &out) != nil || len(out.Choices) == 0 {
		return true
	}
	return ParseYesNo(out.Choices[0].Message.Content)
}

// --- CLI agent grounder (claude -p / codex exec / grok -p) ---

type cliGrounder struct {
	name    string
	argv    []string
	timeout time.Duration
}

func (g *cliGrounder) Label() string { return "cli:" + g.name }

func (g *cliGrounder) Grounded(ctx context.Context, query, passage string) bool {
	cctx, cancel := context.WithTimeout(ctx, g.timeout)
	defer cancel()
	args := append(append([]string(nil), g.argv[1:]...), Prompt(query, passage))
	cmd := exec.CommandContext(cctx, g.argv[0], args...)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil && stdout.Len() == 0 {
		return true
	}
	last := ""
	sc := bufio.NewScanner(&stdout)
	for sc.Scan() {
		if t := strings.TrimSpace(sc.Text()); t != "" {
			last = t
		}
	}
	return ParseYesNo(last)
}

// New builds the grounder selected by its arguments, or nil if none is set. A
// CLI command takes precedence over an HTTP URL when both are given.
func New(cmd, url, model, key string, timeout time.Duration) Grounder {
	if cmd != "" {
		if argv := strings.Fields(cmd); len(argv) > 0 {
			return &cliGrounder{name: cmd, argv: argv, timeout: timeout}
		}
	}
	if url != "" {
		return &httpGrounder{baseURL: strings.TrimRight(url, "/"), key: key, model: model, timeout: timeout}
	}
	return nil
}
