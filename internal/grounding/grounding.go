// Package grounding provides an LLM answer-grounding rerank: a judge that
// decides which recalled passages actually contain a span answering the query.
// It is the cross-encoder precision step a bi-encoder cosine gate cannot do —
// cosine ranks a same-topic-but-answerless passage as high as the answer-bearing
// one, but a model reading (query, passage) jointly can tell them apart (see
// docs/experiments.md §B2–B4).
//
// Judgments are LISTWISE: one model call grades all candidate passages for a
// query at once, not one call per passage. This is what makes a slow frontier
// judge viable — an inject turn costs one round-trip regardless of how many
// candidates cleared the gate. Two backends: an OpenAI-compatible HTTP model
// (fast local judge, ~1s) and a CLI agent (frontier models claude/codex/grok,
// ~3.5s — now one call/turn, not K).
package grounding

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Grounder grades, in a single call, which of the passages answer the query.
type Grounder interface {
	// Relevant returns a mask parallel to passages: true = keep (contains an
	// answering span), false = drop. On any error it returns all-true (fail-open):
	// a flaky or unavailable judge must never silently drop a real hit, only
	// refine precision when it works. The result always has len(passages) entries.
	Relevant(ctx context.Context, query string, passages []string) []bool
	Label() string
}

// Prompt builds the listwise grading prompt, calibrated for extractive QA: a
// passage counts if it merely contains an answer span (not only if it "directly
// answers"), which avoids over-rejecting long multi-fact passages (§B3).
func Prompt(query string, passages []string) string {
	var sb strings.Builder
	sb.WriteString("You are a retrieval grader for extractive QA. For each numbered passage, decide if it contains a span of text that could serve as a correct answer to the question. Judge each passage independently; surrounding unrelated facts are fine.\n")
	sb.WriteString("Question: " + query + "\n")
	sb.WriteString("Passages:\n")
	for i, p := range passages {
		sb.WriteString("[" + strconv.Itoa(i+1) + "] " + strings.ReplaceAll(p, "\n", " ") + "\n")
	}
	sb.WriteString("Reply with one line per passage in the form \"<number>: yes\" or \"<number>: no\". Output only those lines.")
	return sb.String()
}

var verdictRE = regexp.MustCompile(`(?i)(\d+)\s*[:.)\-]?\s*(yes|no|true|false|relevant|irrelevant)`)

// ParseMask reads "<n>: yes/no" verdicts from model text into a mask of length
// n. Entries with no verdict default to true (fail-open). A bare single "yes"/
// "no" with no numbers applies to a lone passage (n==1).
func ParseMask(s string, n int) []bool {
	mask := make([]bool, n)
	for i := range mask {
		mask[i] = true // fail-open default
	}
	if n == 0 {
		return mask
	}
	matched := false
	for _, m := range verdictRE.FindAllStringSubmatch(s, -1) {
		idx, err := strconv.Atoi(m[1])
		if err != nil || idx < 1 || idx > n {
			continue
		}
		mask[idx-1] = isYes(m[2])
		matched = true
	}
	if !matched && n == 1 {
		// No numbered verdicts; treat the whole reply as a single yes/no.
		mask[0] = parseYesNo(s)
	}
	return mask
}

func isYes(tok string) bool {
	switch strings.ToLower(tok) {
	case "no", "false", "irrelevant":
		return false
	default:
		return true
	}
}

// parseYesNo extracts a single yes/no, scanning from the end (models explain,
// then conclude); ambiguous → true (fail-open).
func parseYesNo(s string) bool {
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

// Filter returns the passages the grounder accepts for the query in one call,
// judging only the first topK (callers pass them pre-sorted by score) and
// dropping the untouched tail beyond topK. A nil grounder or empty input is a
// pass-through.
func Filter(ctx context.Context, g Grounder, query string, passages []string, topK int) []string {
	if g == nil || len(passages) == 0 {
		return passages
	}
	n := topK
	if n <= 0 || n > len(passages) {
		n = len(passages)
	}
	mask := g.Relevant(ctx, query, passages[:n])
	kept := make([]string, 0, n)
	for i := 0; i < n; i++ {
		if i >= len(mask) || mask[i] {
			kept = append(kept, passages[i])
		}
	}
	return kept
}

func allTrue(n int) []bool {
	m := make([]bool, n)
	for i := range m {
		m[i] = true
	}
	return m
}

// --- HTTP (OpenAI-compatible) grounder ---

type httpGrounder struct {
	baseURL, key, model string
	timeout             time.Duration
}

func (g *httpGrounder) Label() string { return "http:" + g.model }

func (g *httpGrounder) Relevant(ctx context.Context, query string, passages []string) []bool {
	if len(passages) == 0 {
		return nil
	}
	body, _ := json.Marshal(map[string]any{
		"model":       g.model,
		"messages":    []map[string]string{{"role": "user", "content": Prompt(query, passages)}},
		"temperature": 0,
		"max_tokens":  8 * len(passages), // a few tokens per verdict line
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return allTrue(len(passages))
	}
	req.Header.Set("Content-Type", "application/json")
	if g.key != "" {
		req.Header.Set("Authorization", "Bearer "+g.key)
	}
	resp, err := (&http.Client{Timeout: g.timeout}).Do(req)
	if err != nil {
		return allTrue(len(passages))
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return allTrue(len(passages))
	}
	var out struct {
		Choices []struct {
			Message struct{ Content string } `json:"message"`
		} `json:"choices"`
	}
	if json.Unmarshal(data, &out) != nil || len(out.Choices) == 0 {
		return allTrue(len(passages))
	}
	return ParseMask(out.Choices[0].Message.Content, len(passages))
}

// --- CLI agent grounder (claude -p / codex exec / grok -p) ---

type cliGrounder struct {
	name    string
	argv    []string
	timeout time.Duration
}

func (g *cliGrounder) Label() string { return "cli:" + g.name }

func (g *cliGrounder) Relevant(ctx context.Context, query string, passages []string) []bool {
	if len(passages) == 0 {
		return nil
	}
	cctx, cancel := context.WithTimeout(ctx, g.timeout)
	defer cancel()
	args := append(append([]string(nil), g.argv[1:]...), Prompt(query, passages))
	cmd := exec.CommandContext(cctx, g.argv[0], args...)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil && stdout.Len() == 0 {
		return allTrue(len(passages))
	}
	// Collect all output (agents may print chatter then the verdict lines).
	var lines []string
	sc := bufio.NewScanner(&stdout)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	return ParseMask(strings.Join(lines, "\n"), len(passages))
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
