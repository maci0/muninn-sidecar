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

// grounder answers a single yes/no question: does this passage contain the
// answer to this query? It is the cross-encoder / answer-grounding rerank step
// the bi-encoder cosine gate cannot do (experiments §B2): cosine ranks a
// same-article sibling paragraph as high as the answer-bearing one, but an LLM
// that reads (query, passage) jointly can tell them apart. Two backends mirror
// msc-qa: an OpenAI-compatible HTTP model (fast, for full runs) and a CLI agent
// (frontier models — claude/codex/grok — for transparent in-flight grounding).
type grounder interface {
	// grounded reports whether passage answers query. On error it returns true
	// (fail-open: never suppress a real hit because the judge was unavailable).
	grounded(ctx context.Context, query, passage string) bool
	label() string
}

// groundPrompt is the shared judgment prompt. It demands a bare yes/no so the
// answer is cheap to parse and the judge cannot ramble its budget away.
func groundPrompt(query, passage string) string {
	return "You are a retrieval grader for extractive QA. Say yes if the passage contains a span of text that could serve as a correct answer to the question, even if other facts surround it. Say no only if the answer is genuinely not present.\n" +
		"Question: " + query + "\n" +
		"Passage: " + passage + "\n" +
		"Reply with exactly one word: yes or no."
}

// parseYesNo extracts a yes/no decision from model text. Defaults to true
// (keep) when the reply is ambiguous — fail-open, consistent with grounded().
func parseYesNo(s string) bool {
	s = strings.ToLower(strings.TrimSpace(s))
	// Look at the last word/line first; agents often explain then conclude.
	fields := strings.FieldsFunc(s, func(r rune) bool {
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

// --- HTTP (OpenAI-compatible) grounder ---

type httpGrounder struct {
	baseURL, key, model string
	timeout             time.Duration
}

func (g *httpGrounder) label() string { return "http:" + g.model }

func (g *httpGrounder) grounded(ctx context.Context, query, passage string) bool {
	body, _ := json.Marshal(map[string]any{
		"model": g.model,
		"messages": []map[string]string{
			{"role": "user", "content": groundPrompt(query, passage)},
		},
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
	client := &http.Client{Timeout: g.timeout}
	resp, err := client.Do(req)
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

// --- CLI agent grounder (claude -p / codex exec / grok -p) ---

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
	// Use the last non-empty line: agents print chatter then the verdict.
	last := ""
	sc := bufio.NewScanner(&stdout)
	for sc.Scan() {
		if t := strings.TrimSpace(sc.Text()); t != "" {
			last = t
		}
	}
	return parseYesNo(last)
}

// applyGrounding filters each result's recalled candidates (top-k by cosine) to
// those the grounder accepts, in place. A probe whose every candidate is rejected
// ends up with an empty Recalled set, so the gate suppresses it — which is the
// whole point for same-topic hard negatives. Returns the number of model calls.
func applyGrounding(ctx context.Context, g grounder, results []probeResult, topK int) int {
	calls := 0
	for ri := range results {
		mems := results[ri].Recalled
		if len(mems) == 0 {
			continue
		}
		// Ground only the top-k by cosine — the candidates a gate would consider.
		sortByVecDesc(mems)
		n := topK
		if n <= 0 || n > len(mems) {
			n = len(mems)
		}
		kept := make([]recalledMemory, 0, n)
		for i := 0; i < n; i++ {
			calls++
			if g.grounded(ctx, results[ri].Query, mems[i].Content) {
				kept = append(kept, mems[i])
			}
		}
		results[ri].Recalled = kept
	}
	return calls
}

func sortByVecDesc(mems []recalledMemory) {
	for i := 1; i < len(mems); i++ {
		for j := i; j > 0 && mems[j].VectorScore > mems[j-1].VectorScore; j-- {
			mems[j], mems[j-1] = mems[j-1], mems[j]
		}
	}
}

// buildGrounder constructs the grounder selected by flags, or nil if grounding
// is disabled. The CLI backend takes precedence when both are set.
func buildGrounder(groundCmd, groundURL, groundModel, groundKey string, timeout time.Duration) grounder {
	if groundCmd != "" {
		if argv := strings.Fields(groundCmd); len(argv) > 0 {
			return &cliGrounder{name: groundCmd, argv: argv, timeout: timeout}
		}
	}
	if groundURL != "" {
		return &httpGrounder{baseURL: strings.TrimRight(groundURL, "/"), key: groundKey, model: groundModel, timeout: timeout}
	}
	return nil
}
