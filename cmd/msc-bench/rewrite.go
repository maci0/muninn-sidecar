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

// rewriter turns one (possibly underspecified or multi-hop) query into a small
// set of focused retrieval sub-queries — the recall-side counterpart to the
// answer-grounding rerank. Grounding fixes precision (drop wrong injects); query
// rewrite fixes recall (surface paragraphs a single bi-encoder query misses,
// e.g. the second hop of a multi-hop question). Two backends mirror grounding:
// a fast local HTTP model and a frontier CLI agent.
type rewriter interface {
	// Rewrite returns sub-queries to recall and merge. The original query is
	// always included first, so rewriting can only add recall, never lose the
	// baseline. On any error it returns just the original (fail-safe).
	Rewrite(ctx context.Context, query string, max int) []string
	label() string
}

func rewritePrompt(query string, max int) string {
	return "Decompose this question into the distinct facts a search engine must find to answer it. " +
		"Output up to " + itoa(max) + " short keyword search queries, one per line, no numbering, no prose. " +
		"If the question is already a single lookup, output just one line.\n" +
		"Question: " + query
}

// parseSubqueries reads the model's lines into sub-queries, always prepending
// the original query and de-duplicating, capped at max (counting the original).
func parseSubqueries(original, out string, max int) []string {
	subs := []string{original}
	seen := map[string]bool{strings.ToLower(strings.TrimSpace(original)): true}
	for _, line := range strings.Split(out, "\n") {
		s := strings.TrimSpace(line)
		// Strip common list prefixes ("1.", "-", "*", "•").
		s = strings.TrimLeft(s, "-*•0123456789.) \t")
		if len(s) < 3 {
			continue
		}
		key := strings.ToLower(s)
		if seen[key] {
			continue
		}
		if len(subs) >= max {
			break
		}
		seen[key] = true
		subs = append(subs, s)
	}
	return subs
}

func itoa(n int) string {
	if n <= 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// --- HTTP (OpenAI-compatible) rewriter ---

type httpRewriter struct {
	baseURL, key, model string
	timeout             time.Duration
}

func (r *httpRewriter) label() string { return "http:" + r.model }

func (r *httpRewriter) Rewrite(ctx context.Context, query string, max int) []string {
	body, _ := json.Marshal(map[string]any{
		"model":       r.model,
		"messages":    []map[string]string{{"role": "user", "content": rewritePrompt(query, max)}},
		"temperature": 0,
		"max_tokens":  128,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return []string{query}
	}
	req.Header.Set("Content-Type", "application/json")
	if r.key != "" {
		req.Header.Set("Authorization", "Bearer "+r.key)
	}
	resp, err := (&http.Client{Timeout: r.timeout}).Do(req)
	if err != nil {
		return []string{query}
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return []string{query}
	}
	var out struct {
		Choices []struct {
			Message struct{ Content string } `json:"message"`
		} `json:"choices"`
	}
	if json.Unmarshal(data, &out) != nil || len(out.Choices) == 0 {
		return []string{query}
	}
	return parseSubqueries(query, out.Choices[0].Message.Content, max)
}

// --- CLI agent rewriter (claude -p / codex exec / grok -p) ---

type cliRewriter struct {
	name    string
	argv    []string
	timeout time.Duration
}

func (r *cliRewriter) label() string { return "cli:" + r.name }

func (r *cliRewriter) Rewrite(ctx context.Context, query string, max int) []string {
	cctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	args := append(append([]string(nil), r.argv[1:]...), rewritePrompt(query, max))
	cmd := exec.CommandContext(cctx, r.argv[0], args...)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil && stdout.Len() == 0 {
		return []string{query}
	}
	var lines []string
	sc := bufio.NewScanner(&stdout)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	return parseSubqueries(query, strings.Join(lines, "\n"), max)
}

func buildRewriter(cmd, url, model, key string, timeout time.Duration) rewriter {
	if cmd != "" {
		if argv := strings.Fields(cmd); len(argv) > 0 {
			return &cliRewriter{name: cmd, argv: argv, timeout: timeout}
		}
	}
	if url != "" {
		return &httpRewriter{baseURL: strings.TrimRight(url, "/"), key: key, model: model, timeout: timeout}
	}
	return nil
}
