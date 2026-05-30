// This file contains the offline evaluation harness for memory injection. It
// measures how well the selection pipeline (selectMemories + withinBudget)
// distinguishes memories worth injecting from noise, given labeled scenarios
// where each candidate carries a gold-standard relevance label.
//
// The harness evaluates the *selection* layer in isolation: candidates arrive
// with simulated recall scores, so results are deterministic and CI-friendly,
// independent of any running MuninnDB. The live layer (eval_live.go) covers
// end-to-end recall quality against a real server.
package inject

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"math"
	"sort"
)

//go:embed testdata/scenarios.json
var defaultScenariosJSON []byte

// EvalMemory is a candidate memory for offline evaluation: a recalled memory
// plus a gold-standard label asserting whether it *should* be injected.
type EvalMemory struct {
	ID       string  `json:"id"`
	Concept  string  `json:"concept"`
	Content  string  `json:"content"`
	Score    float64 `json:"score"`    // simulated recall relevance score in [0,1]
	Relevant bool    `json:"relevant"` // gold label: is this memory genuinely useful here?
}

// EvalScenario is one labeled situation: a set of scored candidates plus the
// selection parameters to evaluate. Budget and MinScore default to the
// production defaults (2048 tokens, 0.5) when left zero.
//
// ShouldInject is the gold answer to "*should this turn inject anything at
// all?*" — false for turns where recall surfaced only noise. It is a pointer so
// an omitted value defaults to true (most turns should inject) rather than the
// JSON zero value false.
type EvalScenario struct {
	Name         string       `json:"name"`
	Desc         string       `json:"desc,omitempty"`
	Budget       int          `json:"budget,omitempty"`
	MinScore     float64      `json:"min_score,omitempty"`
	ShouldInject *bool        `json:"should_inject,omitempty"`
	Candidates   []EvalMemory `json:"candidates"`
}

// shouldInject resolves the gold gate label, defaulting to true when unset.
func (s EvalScenario) shouldInject() bool {
	return s.ShouldInject == nil || *s.ShouldInject
}

// Metrics quantifies the quality of one selection outcome.
//
// Precision/Recall/F1 treat injection as binary classification against the gold
// labels. NDCG measures whether the injected memories are *ordered* by true
// relevance. The token fields measure how much of the injection budget was
// spent on genuinely useful content versus noise.
type Metrics struct {
	Precision      float64 `json:"precision"`       // injected-relevant / injected
	Recall         float64 `json:"recall"`          // injected-relevant / all-relevant
	F1             float64 `json:"f1"`              // harmonic mean of precision and recall
	NDCG           float64 `json:"ndcg"`            // ranking quality of the injected list
	GateAccuracy   float64 `json:"gate_accuracy"`   // 1 if the inject/suppress decision matched the gold label (per-scenario); macro-averaged in aggregate
	NumInjected    int     `json:"num_injected"`    // memories actually injected
	NumRelevant    int     `json:"num_relevant"`    // gold-relevant candidates
	InjectedTokens int     `json:"injected_tokens"` // approx tokens spent on injection
	RelevantTokens int     `json:"relevant_tokens"` // of those, spent on relevant memories
	WastedRatio    float64 `json:"wasted_ratio"`    // irrelevant tokens / injected tokens
}

// EvalResult is the outcome of running selection on one scenario.
type EvalResult struct {
	Scenario     string       `json:"scenario"`
	MinScore     float64      `json:"min_score"`
	Injected     []EvalMemory `json:"injected"`      // survivors, in injection order
	DidInject    bool         `json:"did_inject"`    // whether the turn injected anything
	ShouldInject bool         `json:"should_inject"` // gold gate label
	Metrics      Metrics      `json:"metrics"`
}

// DefaultScenarios returns the built-in labeled scenario corpus embedded at
// build time, shared by the test suite and the msc-eval CLI.
func DefaultScenarios() ([]EvalScenario, error) {
	return ParseScenarios(defaultScenariosJSON)
}

// ParseScenarios decodes a JSON array of scenarios.
func ParseScenarios(data []byte) ([]EvalScenario, error) {
	var scenarios []EvalScenario
	if err := json.Unmarshal(data, &scenarios); err != nil {
		return nil, fmt.Errorf("parse scenarios: %w", err)
	}
	return scenarios, nil
}

// RunScenario runs the production selection pipeline over a scenario's
// candidates and scores the outcome against the gold labels. The pipeline
// mirrors Enrich: sort by score, apply selectMemories (adaptive relevance gate
// + near-duplicate removal), then withinBudget greedy packing.
func RunScenario(s EvalScenario) EvalResult {
	return runScenario(s, resolveMinScore(s), resolveBudget(s))
}

// runScenario is the core evaluation with all parameters resolved. minScore <= 0
// disables the threshold (used by sweeps to measure the always-inject baseline).
func runScenario(s EvalScenario, minScore float64, budget int) EvalResult {
	mems := make([]memory, 0, len(s.Candidates))
	label := make(map[string]bool, len(s.Candidates))
	cand := make(map[string]EvalMemory, len(s.Candidates))
	numRelevant := 0
	for _, c := range s.Candidates {
		mems = append(mems, memory{ID: c.ID, Concept: c.Concept, Content: c.Content, Score: c.Score})
		label[c.ID] = c.Relevant
		cand[c.ID] = c
		if c.Relevant {
			numRelevant++
		}
	}

	// mergeMemories sorts by score; replicate that ordering here so selection
	// sees the same input it would in production without needing an Injector.
	sort.SliceStable(mems, func(i, j int) bool { return mems[i].Score > mems[j].Score })

	injected := withinBudget(selectForInjection(mems, minScore), budget)

	injectedEval := make([]EvalMemory, 0, len(injected))
	relevances := make([]int, 0, len(injected))
	injectedRelevant, injectedTokens, relevantTokens := 0, 0, 0
	for _, m := range injected {
		injectedEval = append(injectedEval, cand[m.ID])
		tok := entryChars(m) / charPerToken
		injectedTokens += tok
		rel := 0
		if label[m.ID] {
			rel = 1
			injectedRelevant++
			relevantTokens += tok
		}
		relevances = append(relevances, rel)
	}

	prec := precision(injectedRelevant, len(injected))
	rec := recall(injectedRelevant, numRelevant)

	// Ranking quality is only meaningful when we injected something. A correctly
	// suppressed turn (nothing should be injected, nothing was) is perfect (1);
	// a wrongly suppressed turn (relevant existed, injected nothing) is 0.
	ndcgVal := ndcg(relevances)
	if len(injected) == 0 {
		if numRelevant == 0 {
			ndcgVal = 1
		} else {
			ndcgVal = 0
		}
	}

	didInject := len(injected) > 0
	gateCorrect := didInject == s.shouldInject()

	return EvalResult{
		Scenario:     s.Name,
		MinScore:     minScore,
		Injected:     injectedEval,
		DidInject:    didInject,
		ShouldInject: s.shouldInject(),
		Metrics: Metrics{
			Precision:      prec,
			Recall:         rec,
			F1:             f1(prec, rec),
			NDCG:           ndcgVal,
			GateAccuracy:   boolToFloat(gateCorrect),
			NumInjected:    len(injected),
			NumRelevant:    numRelevant,
			InjectedTokens: injectedTokens,
			RelevantTokens: relevantTokens,
			WastedRatio:    wastedRatio(relevantTokens, injectedTokens),
		},
	}
}

func boolToFloat(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// AggregateMetrics macro-averages per-scenario metrics (each scenario weighted
// equally) and sums the token counters. Macro-averaging keeps a single
// many-candidate scenario from dominating the headline precision/recall numbers.
func AggregateMetrics(results []EvalResult) Metrics {
	if len(results) == 0 {
		return Metrics{}
	}
	var agg Metrics
	for _, r := range results {
		agg.Precision += r.Metrics.Precision
		agg.Recall += r.Metrics.Recall
		agg.F1 += r.Metrics.F1
		agg.NDCG += r.Metrics.NDCG
		agg.GateAccuracy += r.Metrics.GateAccuracy
		agg.NumInjected += r.Metrics.NumInjected
		agg.NumRelevant += r.Metrics.NumRelevant
		agg.InjectedTokens += r.Metrics.InjectedTokens
		agg.RelevantTokens += r.Metrics.RelevantTokens
	}
	n := float64(len(results))
	agg.Precision /= n
	agg.Recall /= n
	agg.F1 /= n
	agg.NDCG /= n
	agg.GateAccuracy /= n
	agg.WastedRatio = wastedRatio(agg.RelevantTokens, agg.InjectedTokens)
	return agg
}

// SweepPoint is the aggregate outcome of running the whole corpus at one
// MinScore value, used to find and justify the injection threshold. A MinScore
// of 0 disables the threshold (the always-inject baseline). GateAccuracy rises
// as the threshold learns to suppress noise-only turns, then F1/recall fall once
// it starts suppressing genuinely relevant low-score topics.
type SweepPoint struct {
	MinScore float64 `json:"min_score"`
	Metrics  Metrics `json:"metrics"`
}

// SweepMinScore runs every scenario at each MinScore in scores (overriding any
// per-scenario MinScore) and returns the aggregate metrics per threshold. This
// is the primary tool for tuning both *when* and *what* to inject.
func SweepMinScore(scenarios []EvalScenario, scores []float64) []SweepPoint {
	points := make([]SweepPoint, 0, len(scores))
	for _, ms := range scores {
		results := make([]EvalResult, 0, len(scenarios))
		for _, s := range scenarios {
			results = append(results, runScenario(s, ms, resolveBudget(s)))
		}
		points = append(points, SweepPoint{MinScore: ms, Metrics: AggregateMetrics(results)})
	}
	return points
}

func resolveMinScore(s EvalScenario) float64 {
	if s.MinScore <= 0 || s.MinScore > 1 {
		return defaultMinScore
	}
	return s.MinScore
}

func resolveBudget(s EvalScenario) int {
	if s.Budget <= 0 {
		return 2048
	}
	return s.Budget
}

// --- metric primitives ---

// precision is injected-relevant / injected. An empty injection has no false
// positives, so precision is defined as 1.0 (vacuously correct).
func precision(relevant, injected int) float64 {
	if injected == 0 {
		return 1.0
	}
	return float64(relevant) / float64(injected)
}

// recall is injected-relevant / all-relevant. When there is nothing relevant to
// find, recall is defined as 1.0 (vacuously complete).
func recall(relevant, total int) float64 {
	if total == 0 {
		return 1.0
	}
	return float64(relevant) / float64(total)
}

// f1 is the harmonic mean of precision and recall, 0 when either is 0.
func f1(p, r float64) float64 {
	if p+r == 0 {
		return 0
	}
	return 2 * p * r / (p + r)
}

// wastedRatio is the fraction of injected tokens spent on irrelevant memories.
func wastedRatio(relevantTokens, injectedTokens int) float64 {
	if injectedTokens == 0 {
		return 0
	}
	return 1 - float64(relevantTokens)/float64(injectedTokens)
}

// ndcg computes normalized discounted cumulative gain over a ranked list of
// binary relevances (1 = relevant, 0 = not), in the order they were injected.
// It answers: are the more relevant memories ranked higher within what we
// injected? Returns 0 for an empty list or one with no relevant items, and 1.0
// for a perfectly-ordered list.
func ndcg(relevances []int) float64 {
	if len(relevances) == 0 {
		return 0
	}
	actual := dcg(relevances)
	ideal := make([]int, len(relevances))
	copy(ideal, relevances)
	sort.Sort(sort.Reverse(sort.IntSlice(ideal)))
	idcg := dcg(ideal)
	if idcg == 0 {
		return 0
	}
	return actual / idcg
}

// dcg is discounted cumulative gain: each relevance is discounted by log2 of
// its 1-based rank + 1, so relevant items ranked lower contribute less.
func dcg(relevances []int) float64 {
	var sum float64
	for i, rel := range relevances {
		sum += float64(rel) / math.Log2(float64(i+2))
	}
	return sum
}
