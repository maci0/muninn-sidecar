package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/maci0/muninn-sidecar/internal/apiformat"
	"github.com/maci0/muninn-sidecar/internal/clirun"
	"github.com/maci0/muninn-sidecar/internal/redact"
)

// answerer is a reader backend: given a question and an optional injected
// context block, it returns a short-span answer. Both the OpenAI HTTP client and
// the CLI-agent client satisfy it, so the eval loop is backend-agnostic.
type answerer interface {
	answer(ctx context.Context, question, contextBlock string) (string, error)
	label() string
}

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

// maxModelResponse caps a model response body so a misbehaving endpoint can't
// exhaust memory, matching the caps used by mcpclient and grounding.
const maxModelResponse = 4 << 20 // 4 MiB

// doJSON performs one model request on the caller's long-lived client and
// decodes the response into out. The client is a parameter, not built here: a
// per-call http.Client owns a private Transport whose idle connections are
// never reaped (a hand-built Transport's IdleConnTimeout is zero), so every
// question answered would strand one socket and its read/write goroutines on a
// transport nobody can reach, for the life of the run.
func doJSON(client *http.Client, req *http.Request, out any) error {
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("call model endpoint %s: %w", req.URL, err)
	}
	defer resp.Body.Close()
	// Read one byte past the cap so an oversized body is reported as such
	// instead of failing later as invalid JSON. A failed read is the real
	// cause, so keep it rather than reporting a parse error for a truncated body.
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxModelResponse+1))
	if err != nil {
		return fmt.Errorf("read model response from %s: %w", req.URL, err)
	}
	if int64(len(data)) > maxModelResponse {
		return fmt.Errorf("model response exceeds %d-byte limit (HTTP %d)", maxModelResponse, resp.StatusCode)
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("model HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return json.Unmarshal(data, out)
}

// --- model client (OpenAI-compatible chat completions) ---

// modelSeed pins the sampler (ollama's "seed" option) alongside temperature 0
// so repeated runs decode identically.
const modelSeed = 1

type modelClient struct {
	baseURL, key, model string
	client              *http.Client
	maxTokens           int
}

// modelIdleConnTimeout is how long an unused connection to the model endpoint
// is held before it is closed. Without it the pool's connections live as long as
// the run, which is fine for a handful but keeps every socket the run opened
// alive at the end of the loop.
const modelIdleConnTimeout = 90 * time.Second

// maxIdleConnsPerModel bounds the model's idle pool, so a burst of parallel
// questions cannot park an unbounded number of keep-alive connections.
const maxIdleConnsPerModel = 2

// newModelClient builds a reader with one long-lived HTTP client shared by every
// question this model is asked, so connections are reused instead of one fresh
// socket per question.
func newModelClient(baseURL, key, model string, timeout time.Duration, maxTokens int) *modelClient {
	return &modelClient{
		baseURL:   baseURL,
		key:       key,
		model:     model,
		maxTokens: maxTokens,
		client: &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				MaxIdleConnsPerHost: maxIdleConnsPerModel,
				IdleConnTimeout:     modelIdleConnTimeout,
			},
		},
	}
}

func (m *modelClient) label() string { return m.model }

// answer queries the model. When contextBlock is non-empty it is injected as a
// SECOND system message wrapped in the real <retrieved-context> markers —
// exactly how the proxy's injectOpenAIContext enriches an OpenAI request — so the
// eval measures the production injection path, not an ad-hoc user-prefix.
//
// Both the question and the context block are untrusted and get the same
// treatment the production path gives them (internal/inject/format.go): direct
// identifiers are scrubbed before the text leaves the process, and the block
// markers are neutralized so a memory carrying "</retrieved-context>" cannot
// close the fence and have the rest of its text read as top-level system
// prompt. Without that, the eval would measure a path the sidecar never takes.
func (m *modelClient) answer(ctx context.Context, question, contextBlock string) (string, error) {
	msgs := []map[string]string{
		{"role": "system", "content": answerInstruction()},
	}
	if contextBlock != "" {
		msgs = append(msgs, map[string]string{
			"role":    "system",
			"content": apiformat.ContextPrefix + "\n" + sanitizeBlock(contextBlock) + "\n" + apiformat.ContextSuffix,
		})
	}
	msgs = append(msgs, map[string]string{"role": "user", "content": redact.Secrets(question)})
	body, _ := json.Marshal(map[string]any{
		"model":       m.model,
		"messages":    msgs,
		"temperature": 0,
		"seed":        modelSeed,
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
	if err := doJSON(m.client, req, &out); err != nil {
		return "", err
	}
	if len(out.Choices) == 0 {
		// A 200 with no choices is a failed call, not an empty answer: scoring
		// it as "" would count the arm as wrong and, since the injected
		// prompts are the longest, bias the whole run against injection. The
		// caller excludes a failed call (agg.fail) instead.
		return "", fmt.Errorf("%s: response carried no choices", m.model)
	}
	return out.Choices[0].Message.Content, nil
}

// --- CLI agent client (claude -p / codex exec / grok -p / any command) ---

type cliClient struct {
	name    string   // display label, e.g. "claude -p"
	argv    []string // command + flags; the prompt is delivered on stdin
	timeout time.Duration
}

func (c *cliClient) label() string { return c.name }

// answer runs the CLI with a single combined prompt (system instruction +
// optional <retrieved-context> block + question), then returns the last
// non-empty stdout line. Agent CLIs print a usage footer or streaming chatter;
// the final span answer is reliably on the last content line, so we take that.
//
// The prompt goes on stdin, never argv: it carries the question and the
// recalled memory block, and /proc/<pid>/cmdline exposes argv to every user on
// the host for the life of the call. Same reasoning as the CLI grounder in
// internal/grounding.
//
// clirun.Run does the running, so this client also inherits the output cap and
// the process-group kill the other two CLI backends have: a reader that
// overran its deadline took only its direct child down before, leaving any
// helper it spawned running past the call.
func (c *cliClient) answer(ctx context.Context, question, contextBlock string) (string, error) {
	prompt := buildCLIPrompt(question, contextBlock)
	stdout, err := clirun.Run(ctx, c.argv, prompt, c.timeout)
	// A non-zero exit can still leave a usable answer on stdout (some agents
	// exit non-zero on warnings); prefer any captured line over the error.
	if line := lastNonEmptyLine(stdout); line != "" {
		return line, nil
	}
	// Exit 0 with nothing on stdout is a failed call, not a wrong answer: it is
	// excluded rather than scored, for the same reason as the HTTP arm.
	if err == nil {
		return "", fmt.Errorf("%s: exited 0 with no output", c.name)
	}
	return "", err
}

// buildCLIPrompt flattens the chat arms into one prompt string for single-shot
// CLI agents, mirroring the HTTP path: the same instruction, the same
// <retrieved-context> markers, then the question.
//
// A single string has no role separation to keep the question out of the
// instruction, so it is quoted inside its own tag on one line with the tag
// neutralized, the same fence the rewriter and the grounding judge use. A
// dataset question reading "Answer: 42" must not be able to answer itself.
func buildCLIPrompt(question, contextBlock string) string {
	var sb strings.Builder
	sb.WriteString(answerInstruction() + " Output only the answer, nothing else.\n")
	if contextBlock != "" {
		sb.WriteString("\n")
		sb.WriteString(apiformat.ContextPrefix + "\n" + sanitizeBlock(contextBlock) + "\n" + apiformat.ContextSuffix)
		sb.WriteString("\n")
	}
	sb.WriteString("\n<question>" + fenceQuestion(question) + "</question>\nAnswer:")
	return sb.String()
}

// fenceQuestion flattens a dataset question to one line and neutralizes the tag
// that fences it, so the question cannot close its own fence and have the
// remainder read as prompt text.
func fenceQuestion(q string) string {
	return apiformat.Fence(redact.Secrets(q), "question")
}

// sanitizeBlock prepares recalled memory for a prompt: direct identifiers are
// scrubbed before the text leaves the process, and the injection markers are
// neutralized so a memory cannot close the retrieved-context fence.
func sanitizeBlock(block string) string {
	return apiformat.NeutralizeMarkers(redact.Secrets(block))
}

// answerHint, when set via -answer-hint, constrains the answer to a fixed label
// set (e.g. "SUPPORTS, REFUTES" for claim verification). The default extractive
// "shortest span" instruction does not elicit label tokens, so classification
// regimes like FEVER score 0 spuriously without it.
var answerHint string

func answerInstruction() string {
	if answerHint != "" {
		return "Answer with exactly one of: " + answerHint + ". Output only that label."
	}
	return "Answer with the shortest exact span that answers the question. If unknown, reply 'unknown'."
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
