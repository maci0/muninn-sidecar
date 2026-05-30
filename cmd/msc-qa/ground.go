package main

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

// grounder is the answer-grounding rerank judge: does this passage contain a
// span that answers the query? It is the cross-encoder precision step the cosine
// gate cannot do (experiments §B2/§B3). Mirrors msc-bench: an OpenAI-compatible
// HTTP model (fast, full runs) or a CLI agent (frontier — claude/codex/grok).
type grounder interface {
	grounded(ctx context.Context, query, passage string) bool
	label() string
}

func groundPrompt(query, passage string) string {
	return "You are a retrieval grader for extractive QA. Say yes if the passage contains a span of text that could serve as a correct answer to the question, even if other facts surround it. Say no only if the answer is genuinely not present.\n" +
		"Question: " + query + "\n" +
		"Passage: " + passage + "\n" +
		"Reply with exactly one word: yes or no."
}

// parseYesNo extracts a yes/no decision, defaulting to true (keep) when the
// reply is ambiguous — fail-open, so a flaky judge never silently drops hits.
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

type httpGrounder struct {
	baseURL, key, model string
	timeout             time.Duration
}

func (g *httpGrounder) label() string { return "http:" + g.model }

func (g *httpGrounder) grounded(ctx context.Context, query, passage string) bool {
	body, _ := json.Marshal(map[string]any{
		"model":       g.model,
		"messages":    []map[string]string{{"role": "user", "content": groundPrompt(query, passage)}},
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
	return parseYesNo(out.Choices[0].Message.Content)
}

type cliGrounder struct {
	name    string
	argv    []string
	timeout time.Duration
}

func (g *cliGrounder) label() string { return "cli:" + g.name }

func (g *cliGrounder) grounded(ctx context.Context, query, passage string) bool {
	cctx, cancel := context.WithTimeout(ctx, g.timeout)
	defer cancel()
	args := append(append([]string(nil), g.argv[1:]...), groundPrompt(query, passage))
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
	return parseYesNo(last)
}

func buildGrounder(cmd, url, model, key string, timeout time.Duration) grounder {
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

// groundFilter keeps only the candidate passages the grounder accepts for the
// query (up to topK by input order, which the caller passes pre-sorted by score).
func groundFilter(ctx context.Context, g grounder, query string, cands []string, topK int) []string {
	if g == nil || len(cands) == 0 {
		return cands
	}
	n := topK
	if n <= 0 || n > len(cands) {
		n = len(cands)
	}
	kept := make([]string, 0, n)
	for i := 0; i < n; i++ {
		if g.grounded(ctx, query, cands[i]) {
			kept = append(kept, cands[i])
		}
	}
	return kept
}
