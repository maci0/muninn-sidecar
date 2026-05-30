// This file contains the live (end-to-end) evaluation layer. Unlike the offline
// harness in eval.go — which feeds pre-scored candidates through the selection
// pipeline — the live layer seeds a real MuninnDB vault and exercises the full
// recall + selection path, so it measures recall quality (does the embedding
// search surface the right memories?) on top of selection quality.
//
// It has side effects (it writes to a vault) and needs a running MuninnDB, so
// it is never run by the normal test suite — only via the msc-eval CLI's -live
// mode or a test explicitly opted in through an environment flag.
package inject

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

// SeedMemory is a memory to store in the vault before probing.
type SeedMemory struct {
	Concept string `json:"concept"`
	Content string `json:"content"`
}

// LiveScenario seeds a set of memories, then issues a query and asserts which
// concepts should end up injected. Unlike offline scenarios, relevance here is
// expressed as the set of concepts a correct system would surface.
type LiveScenario struct {
	Name             string       `json:"name"`
	Query            string       `json:"query"`
	Seed             []SeedMemory `json:"seed"`
	ExpectedConcepts []string     `json:"expected_concepts"`
}

// LiveResult is the end-to-end outcome for one live scenario.
type LiveResult struct {
	Scenario         string   `json:"scenario"`
	Recalled         int      `json:"recalled"`          // memories returned by recall
	InjectedConcepts []string `json:"injected_concepts"` // concepts that survived selection+budget
	Expected         []string `json:"expected"`
	Hits             int      `json:"hits"`     // expected concepts that were injected
	Extra            int      `json:"extra"`    // injected concepts not in the expected set
	HitRate          float64  `json:"hit_rate"` // hits / len(expected)
}

// ParseLiveScenarios decodes a JSON array of live scenarios.
func ParseLiveScenarios(data []byte) ([]LiveScenario, error) {
	var scenarios []LiveScenario
	if err := json.Unmarshal(data, &scenarios); err != nil {
		return nil, fmt.Errorf("parse live scenarios: %w", err)
	}
	return scenarios, nil
}

// RunLive seeds each scenario's memories into the configured vault, then runs
// the real recall + selection path against the query and scores which expected
// concepts were injected. It uses cfg.RelRatio/cfg.Budget (defaulted in New).
//
// Seeding writes to cfg.Vault — point it at a throwaway eval vault, not a
// working one. A short settle delay lets MuninnDB index the seeded memories
// before the probe; tune via settle.
func RunLive(ctx context.Context, cfg Config, scenarios []LiveScenario, settle time.Duration) ([]LiveResult, error) {
	inj := New(cfg)

	results := make([]LiveResult, 0, len(scenarios))
	for _, s := range scenarios {
		if err := inj.seed(ctx, s.Seed); err != nil {
			return results, fmt.Errorf("scenario %q: seed: %w", s.Name, err)
		}
		if settle > 0 {
			timer := time.NewTimer(settle)
			select {
			case <-ctx.Done():
				timer.Stop()
				return results, ctx.Err()
			case <-timer.C:
			}
		}

		recalled, err := inj.recall(ctx, s.Query)
		if err != nil {
			return results, fmt.Errorf("scenario %q: recall: %w", s.Name, err)
		}
		sort.SliceStable(recalled, func(i, j int) bool { return recalled[i].Score > recalled[j].Score })
		injected := withinBudget(selectForInjection(recalled, inj.minScore), inj.budget)

		results = append(results, scoreLive(s, len(recalled), injected))
	}
	return results, nil
}

// seed stores the scenario's memories in the vault via muninn_remember.
func (inj *Injector) seed(ctx context.Context, seed []SeedMemory) error {
	for _, m := range seed {
		_, err := inj.mcp.Call(ctx, "muninn_remember", map[string]any{
			"vault":   inj.vault,
			"concept": m.Concept,
			"content": m.Content,
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// scoreLive compares the injected concepts against the expected set.
func scoreLive(s LiveScenario, recalled int, injected []memory) LiveResult {
	expected := make(map[string]bool, len(s.ExpectedConcepts))
	for _, c := range s.ExpectedConcepts {
		expected[normalizeConcept(c)] = true
	}

	injectedConcepts := make([]string, 0, len(injected))
	hits, extra := 0, 0
	matched := make(map[string]bool, len(injected))
	for _, m := range injected {
		injectedConcepts = append(injectedConcepts, m.Concept)
		key := normalizeConcept(m.Concept)
		if expected[key] {
			if !matched[key] {
				hits++
				matched[key] = true
			}
		} else {
			extra++
		}
	}

	hitRate := 1.0
	if len(s.ExpectedConcepts) > 0 {
		hitRate = float64(hits) / float64(len(s.ExpectedConcepts))
	}

	return LiveResult{
		Scenario:         s.Name,
		Recalled:         recalled,
		InjectedConcepts: injectedConcepts,
		Expected:         s.ExpectedConcepts,
		Hits:             hits,
		Extra:            extra,
		HitRate:          hitRate,
	}
}
