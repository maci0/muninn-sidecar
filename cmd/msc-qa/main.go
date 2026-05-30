// Command msc-qa measures the DOWNSTREAM usefulness of memory injection: does
// putting recalled context in front of the model actually improve its answers?
// It replays SQuAD questions through an OpenAI-compatible chat model under three
// arms and scores answers with SQuAD exact-match / token-F1:
//
//   - none:       question only (baseline)
//   - injected:   question + the gated recall context from a seeded vault
//   - distractor: question + a deliberately irrelevant memory (harm of a false inject)
//
// It also reports answer-coverage (did the injected context even contain the
// answer?) and utilization (did the model's answer overlap the injected text?).
//
// This is the gold-standard metric the proxy-level studies only proxy for. It
// needs a model endpoint; without -model-url it builds the prompts and recall
// but cannot score, and exits with guidance.
//
//	msc-qa -vault msc-squad -model-url http://localhost:1234/v1 -model gpt-4o-mini -n 100
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/maci0/muninn-sidecar/internal/apiformat"
	"github.com/maci0/muninn-sidecar/internal/mcpclient"
)

func newReq(ctx context.Context, url, key string, body []byte) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	return req, nil
}

func doJSON(req *http.Request, timeout time.Duration, out any) error {
	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("model HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return json.Unmarshal(data, out)
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "msc-qa:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		squadFile   = flag.String("squad-file", "/tmp/squad-dev.json", "dataset JSON path (SQuAD or HotpotQA official format)")
		dataset     = flag.String("dataset", "squad", "dataset format: squad | hotpot | generic")
		vault       = flag.String("vault", "msc-squad", "vault to recall from (must be seeded, e.g. by msc-bench)")
		mcpURL      = flag.String("mcp-url", envOr("MUNINN_MCP_URL", "http://127.0.0.1:8750/mcp"), "MuninnDB MCP endpoint")
		token       = flag.String("token", "", "MuninnDB bearer token (default ~/.muninn/mcp.token)")
		modelURL    = flag.String("model-url", "", "OpenAI-compatible base URL (e.g. http://localhost:1234/v1); empty = build-only, no scoring")
		modelKey    = flag.String("model-key", os.Getenv("OPENAI_API_KEY"), "model API key")
		model       = flag.String("model", "gpt-4o-mini", "model name(s); comma-separated to compare several")
		modelCmd    = flag.String("model-cmd", "", "comma-separated reader CLIs (e.g. \"claude -p,codex exec --skip-git-repo-check,grok -p\"); each gets the prompt as a final arg, the last non-empty stdout line is the answer")
		minScore    = flag.Float64("min-score", 0.6, "injection cosine threshold (the gate)")
		n           = flag.Int("n", 100, "number of questions to evaluate")
		maxTokens   = flag.Int("max-tokens", 512, "model max_tokens (raise for thinking models)")
		multiRecall = flag.Bool("multi-recall", false, "split query into entity spans, recall each, merge (multi-hop)")
		timeout     = flag.Duration("timeout", 60*time.Second, "per-call timeout")
		mdFile      = flag.String("md", "", "append a results row per model to this markdown file")
		groundURL   = flag.String("ground-url", "", "add a 4th \"grounded\" arm: LLM answer-grounding filter on the injected context via an OpenAI-compatible URL")
		groundCmd   = flag.String("ground-cmd", "", "add a 4th \"grounded\" arm via a CLI agent grounder (e.g. \"claude -p\")")
		groundMod   = flag.String("ground-model", "qwen2.5:7b-instruct", "grounding model name (for -ground-url)")
		groundKey   = flag.String("ground-key", "", "grounding model API key (for -ground-url)")
		groundTopK  = flag.Int("ground-topk", 5, "ground only the top-K recalled passages per question")
	)
	flag.Parse()

	questions, err := loadDataset(*dataset, *squadFile, *n)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "loaded %d SQuAD questions\n", len(questions))

	mcp := mcpclient.New(*mcpURL, resolveToken(*token), *timeout)
	ctx := context.Background()

	// Optional answer-grounding rerank: a 4th "grounded" arm filters the injected
	// passages by an LLM "does this answer the query?" judgment (model-independent,
	// so computed once with the injected context). Tests whether grounding helps
	// downstream where wrong-paragraph injects hurt (e.g. HotpotQA, experiments §B).
	grd := buildGrounder(*groundCmd, *groundURL, *groundMod, *groundKey, *timeout)

	// Precompute recall context per question ONCE — it is model-independent, so
	// all models reuse it (recall is the expensive MCP path; only model calls
	// repeat per model).
	type prepared struct {
		q                            qaItem
		injected, distract, grounded string
	}
	prep := make([]prepared, len(questions))
	var coverage, groundCalls int
	for i, q := range questions {
		cands := recallCandidates(ctx, mcp, *vault, q.Question, *minScore, *multiRecall)
		inj := strings.Join(cands, "\n")
		dis := recallContext(ctx, mcp, *vault, "unrelated trivia about cooking and weather", *minScore, false)
		grounded := ""
		if grd != nil {
			groundCalls += min(*groundTopK, len(cands))
			grounded = strings.Join(groundFilter(ctx, grd, q.Question, cands, *groundTopK), "\n")
		}
		prep[i] = prepared{q: q, injected: inj, distract: dis, grounded: grounded}
		if containsAnswer(inj, q.Answers) {
			coverage++
		}
	}
	fmt.Fprintf(os.Stderr, "recall context contained the gold answer for %d/%d (%.0f%%)\n",
		coverage, len(questions), 100*float64(coverage)/float64(max(1, len(questions))))
	if grd != nil {
		fmt.Fprintf(os.Stderr, "grounding arm enabled via %s (~%d judge calls)\n", grd.label(), groundCalls)
	}

	// Assemble reader backends: OpenAI-compatible HTTP models (-model-url) and/or
	// CLI agents (-model-cmd). Either source can be empty.
	var readers []answerer
	if *modelURL != "" {
		for _, m := range splitCSV(*model) {
			readers = append(readers, &modelClient{baseURL: strings.TrimRight(*modelURL, "/"), key: *modelKey, model: m, timeout: *timeout, maxTokens: *maxTokens})
		}
	}
	for _, cmd := range splitCSV(*modelCmd) {
		if argv := strings.Fields(cmd); len(argv) > 0 {
			readers = append(readers, &cliClient{name: cmd, argv: argv, timeout: *timeout})
		}
	}

	if len(readers) == 0 {
		fmt.Printf("BUILD-ONLY (no -model-url / -model-cmd): answer-coverage %d/%d. Supply a reader to score arms.\n", coverage, len(questions))
		return nil
	}

	// Arms are dynamic: the grounded arm appears only when a grounder is set.
	armNames := []string{"none", "injected", "distractor"}
	if grd != nil {
		armNames = append(armNames, "grounded")
	}
	fmt.Printf("\n=== msc-qa: downstream answer quality (%d questions) ===\n", len(questions))
	header := fmt.Sprintf("%-26s %12s %12s %12s", "model", "none EM/F1", "inj EM/F1", "dist EM/F1")
	if grd != nil {
		header += fmt.Sprintf(" %12s", "grnd EM/F1")
	}
	fmt.Printf("%s   %s\n", header, "Δinj F1")
	for _, r := range readers {
		agg := make([]armAgg, len(armNames))
		for i, p := range prep {
			ctxBlocks := []string{"", p.injected, p.distract}
			if grd != nil {
				ctxBlocks = append(ctxBlocks, p.grounded)
			}
			for a := range armNames {
				ans, err := r.answer(ctx, p.q.Question, ctxBlocks[a])
				if err != nil {
					fmt.Fprintf(os.Stderr, "  warn: %s q%d arm %s: %v\n", r.label(), i, armNames[a], err)
					ans = ""
				}
				agg[a].add(exactMatch(ans, p.q.Answers), tokenF1(ans, p.q.Answers), containsAnswer(ans, p.q.Answers))
			}
		}
		dInj := agg[1].f1() - agg[0].f1()
		row := fmt.Sprintf("%-26s  %4.2f/%4.2f   %4.2f/%4.2f   %4.2f/%4.2f",
			trunc(r.label(), 26), agg[0].em(), agg[0].f1(), agg[1].em(), agg[1].f1(), agg[2].em(), agg[2].f1())
		if grd != nil {
			row += fmt.Sprintf("   %4.2f/%4.2f", agg[3].em(), agg[3].f1())
		}
		fmt.Printf("%s   %+5.2f\n", row, dInj)
		if grd != nil {
			fmt.Printf("    Δgrounded F1 = %+.2f (vs none), %+.2f (vs injected)\n", agg[3].f1()-agg[0].f1(), agg[3].f1()-agg[1].f1())
		}
		if *mdFile != "" {
			appendMD(*mdFile, r.label(), len(questions), [3]armAgg{agg[0], agg[1], agg[2]})
		}
	}
	return nil
}

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func appendMD(path, model string, n int, agg [3]armAgg) {
	row := fmt.Sprintf("| %s | %d | %.2f/%.2f | %.2f/%.2f | %.2f/%.2f | %+.2f | %+.2f |\n",
		model, n, agg[0].em(), agg[0].f1(), agg[1].em(), agg[1].f1(), agg[2].em(), agg[2].f1(),
		agg[1].f1()-agg[0].f1(), agg[2].f1()-agg[0].f1())
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	f.WriteString(row)
}

type armAgg struct {
	n, emN, ctxN int
	f1Sum        float64
}

func (a *armAgg) add(em, f1 float64, usesCtx bool) {
	a.n++
	a.emN += int(em)
	a.f1Sum += f1
	if usesCtx {
		a.ctxN++
	}
}
func (a *armAgg) em() float64   { return safe(float64(a.emN), float64(a.n)) }
func (a *armAgg) f1() float64   { return safe(a.f1Sum, float64(a.n)) }
func (a *armAgg) util() float64 { return safe(float64(a.ctxN), float64(a.n)) }
func safe(x, y float64) float64 {
	if y == 0 {
		return 0
	}
	return x / y
}

// --- recall (mirrors the proxy's gated selection: cosine >= minScore) ---

// splitQueryQA decomposes a query into sub-queries (full + capitalized entity
// spans) for transparent multi-recall — no LLM call.
func splitQueryQA(q string) []string {
	subs := []string{q}
	seen := map[string]bool{q: true}
	var cur []string
	flush := func() {
		if len(cur) > 0 {
			s := strings.Trim(strings.Join(cur, " "), "?.,")
			if len(s) >= 3 && !seen[s] {
				seen[s] = true
				subs = append(subs, s)
			}
			cur = nil
		}
	}
	for _, w := range strings.Fields(q) {
		r := []rune(w)
		if len(r) > 0 && r[0] >= 'A' && r[0] <= 'Z' {
			cur = append(cur, w)
		} else {
			flush()
		}
	}
	flush()
	return subs
}

func recallContext(ctx context.Context, mcp *mcpclient.Client, vault, query string, minScore float64, multi bool) string {
	return strings.Join(recallCandidates(ctx, mcp, vault, query, minScore, multi), "\n")
}

// recallCandidates returns the gated recall passages (cosine >= minScore),
// highest-scored first, as a slice — so callers can ground-filter before joining.
func recallCandidates(ctx context.Context, mcp *mcpclient.Client, vault, query string, minScore float64, multi bool) []string {
	if multi {
		seen := map[string]bool{}
		var parts []string
		for _, sub := range splitQueryQA(query) {
			for _, c := range recallCandidates(ctx, mcp, vault, sub, minScore, false) {
				if c != "" && !seen[c] {
					seen[c] = true
					parts = append(parts, c)
				}
			}
		}
		return parts
	}
	resp, err := mcp.Call(ctx, "muninn_recall", map[string]any{
		"vault": vault, "context": []string{query}, "limit": 5, "threshold": 0.05, "mode": "semantic",
	})
	if err != nil {
		return nil
	}
	var rpc struct {
		Result struct {
			Content []struct {
				Type, Text string
			} `json:"content"`
		} `json:"result"`
	}
	if json.Unmarshal(resp, &rpc) != nil {
		return nil
	}
	for _, c := range rpc.Result.Content {
		if c.Type != "text" {
			continue
		}
		var inner struct {
			Memories []struct {
				Content     string  `json:"content"`
				VectorScore float64 `json:"vector_score"`
				Score       float64 `json:"score"`
			} `json:"memories"`
		}
		if json.Unmarshal([]byte(c.Text), &inner) != nil {
			return nil
		}
		var parts []string
		for _, m := range inner.Memories {
			rel := m.VectorScore
			if rel == 0 {
				rel = m.Score
			}
			if rel >= minScore { // the gate
				parts = append(parts, m.Content)
			}
		}
		return parts
	}
	return nil
}

// answerer is a reader backend: given a question and an optional injected
// context block, it returns a short-span answer. Both the OpenAI HTTP client and
// the CLI-agent client satisfy it, so the eval loop is backend-agnostic.
type answerer interface {
	answer(ctx context.Context, question, contextBlock string) (string, error)
	label() string
}

// --- model client (OpenAI-compatible chat completions) ---

type modelClient struct {
	baseURL, key, model string
	timeout             time.Duration
	maxTokens           int
}

func (m *modelClient) label() string { return m.model }

// answer queries the model. When contextBlock is non-empty it is injected as a
// SECOND system message wrapped in the real <retrieved-context> markers —
// exactly how the proxy's injectOpenAIContext enriches an OpenAI request — so the
// eval measures the production injection path, not an ad-hoc user-prefix.
func (m *modelClient) answer(ctx context.Context, question, contextBlock string) (string, error) {
	msgs := []map[string]string{
		{"role": "system", "content": "Answer with the shortest exact span that answers the question. If unknown, reply 'unknown'."},
	}
	if contextBlock != "" {
		msgs = append(msgs, map[string]string{
			"role":    "system",
			"content": apiformat.ContextPrefix + "\n" + contextBlock + "\n" + apiformat.ContextSuffix,
		})
	}
	msgs = append(msgs, map[string]string{"role": "user", "content": question})
	body, _ := json.Marshal(map[string]any{
		"model":       m.model,
		"messages":    msgs,
		"temperature": 0,
		"max_tokens":  m.maxTokens,
	})
	req, err := newReq(ctx, m.baseURL+"/chat/completions", m.key, body)
	if err != nil {
		return "", err
	}
	var out struct {
		Choices []struct {
			Message struct{ Content string } `json:"message"`
		} `json:"choices"`
	}
	if err := doJSON(req, m.timeout, &out); err != nil {
		return "", err
	}
	if len(out.Choices) == 0 {
		return "", nil
	}
	return out.Choices[0].Message.Content, nil
}

// --- CLI agent client (claude -p / codex exec / grok -p / any command) ---

type cliClient struct {
	name    string   // display label, e.g. "claude -p"
	argv    []string // command + flags; the prompt is appended as a final arg
	timeout time.Duration
}

func (c *cliClient) label() string { return c.name }

// answer runs the CLI with a single combined prompt (system instruction +
// optional <retrieved-context> block + question) appended as the final argv
// element, then returns the last non-empty stdout line. Agent CLIs print a usage
// footer or streaming chatter; the final span answer is reliably on the last
// content line, so we take that.
func (c *cliClient) answer(ctx context.Context, question, contextBlock string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	prompt := buildCLIPrompt(question, contextBlock)
	args := append(append([]string(nil), c.argv[1:]...), prompt)
	cmd := exec.CommandContext(cctx, c.argv[0], args...)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		// A non-zero exit can still leave a usable answer on stdout (some agents
		// exit non-zero on warnings); prefer any captured line over the error.
		if line := lastNonEmptyLine(stdout.String()); line != "" {
			return line, nil
		}
		return "", err
	}
	return lastNonEmptyLine(stdout.String()), nil
}

// buildCLIPrompt flattens the chat arms into one prompt string for single-shot
// CLI agents, mirroring the HTTP path: the same instruction, the same
// <retrieved-context> markers, then the question.
func buildCLIPrompt(question, contextBlock string) string {
	var sb strings.Builder
	sb.WriteString("Answer with the shortest exact span that answers the question. If unknown, reply 'unknown'. Output only the answer, nothing else.\n")
	if contextBlock != "" {
		sb.WriteString("\n")
		sb.WriteString(apiformat.ContextPrefix + "\n" + contextBlock + "\n" + apiformat.ContextSuffix)
		sb.WriteString("\n")
	}
	sb.WriteString("\nQuestion: " + question + "\nAnswer:")
	return sb.String()
}

// lastNonEmptyLine returns the final non-blank line of s, trimmed.
func lastNonEmptyLine(s string) string {
	lines := strings.Split(s, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if t := strings.TrimSpace(lines[i]); t != "" {
			return t
		}
	}
	return ""
}

// --- SQuAD QA loader ---

type qaItem struct {
	Question string
	Answers  []string
}

func loadSquadQA(path string, n int) ([]qaItem, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read squad: %w", err)
	}
	var sq struct {
		Data []struct {
			Paragraphs []struct {
				QAs []struct {
					Question     string `json:"question"`
					IsImpossible bool   `json:"is_impossible"`
					Answers      []struct {
						Text string `json:"text"`
					} `json:"answers"`
				} `json:"qas"`
			} `json:"paragraphs"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &sq); err != nil {
		return nil, fmt.Errorf("parse squad: %w", err)
	}
	var out []qaItem
	for _, art := range sq.Data {
		for _, p := range art.Paragraphs {
			for _, qa := range p.QAs {
				if qa.IsImpossible || len(qa.Answers) == 0 {
					continue
				}
				golds := make([]string, 0, len(qa.Answers))
				for _, a := range qa.Answers {
					golds = append(golds, a.Text)
				}
				out = append(out, qaItem{Question: qa.Question, Answers: golds})
				if len(out) >= n {
					return out, nil
				}
			}
		}
	}
	return out, nil
}

// loadHotpotQA reads the official HotpotQA array format ([{question, answer,
// context, supporting_facts}]). Multi-hop questions with a single gold answer
// (incl. yes/no). Pairs with `msc-bench -corpus hotpot` seeding.
func loadHotpotQA(path string, n int) ([]qaItem, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read hotpot: %w", err)
	}
	var data []struct {
		Question string `json:"question"`
		Answer   string `json:"answer"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, fmt.Errorf("parse hotpot: %w", err)
	}
	var out []qaItem
	for _, d := range data {
		if d.Question == "" || d.Answer == "" {
			continue
		}
		out = append(out, qaItem{Question: d.Question, Answers: []string{d.Answer}})
		if len(out) >= n {
			break
		}
	}
	return out, nil
}

// loadGenericQA reads a flat [{question, answer}] JSON (e.g. produced by
// `msc-bench -dump-qa`), the format for arbitrary seeded vaults.
func loadGenericQA(path string, n int) ([]qaItem, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read generic qa: %w", err)
	}
	var data []struct {
		Question string `json:"question"`
		Answer   string `json:"answer"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, fmt.Errorf("parse generic qa: %w", err)
	}
	var out []qaItem
	for _, d := range data {
		if d.Question == "" || d.Answer == "" {
			continue
		}
		out = append(out, qaItem{Question: d.Question, Answers: []string{d.Answer}})
		if len(out) >= n {
			break
		}
	}
	return out, nil
}

func loadDataset(dataset, path string, n int) ([]qaItem, error) {
	switch dataset {
	case "hotpot":
		return loadHotpotQA(path, n)
	case "generic":
		return loadGenericQA(path, n)
	default:
		return loadSquadQA(path, n)
	}
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func resolveToken(flagVal string) string {
	if flagVal != "" {
		return flagVal
	}
	if t := os.Getenv("MUNINN_TOKEN"); t != "" {
		return t
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(home, ".muninn", "mcp.token"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}
