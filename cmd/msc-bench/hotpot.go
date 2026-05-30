package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// HotpotQA is a multi-hop QA dataset: each question requires combining facts
// from two different Wikipedia paragraphs. It is the fair test for whether the
// `deep` recall mode (graph traversal across linked memories) earns its keep —
// single-hop SQuAD found deep adds only noise. genHotpot seeds the supporting
// paragraphs of the first `seedExamples` questions as memories and probes with
// those multi-hop questions; absent probes come from disjoint later questions.
//
// Public HotpotQA mirrors are flaky; if the file is missing this returns a clear
// error. Fetch: hotpot_dev_distractor_v1.json from the HotpotQA project page.
func genHotpot(path string, seedExamples, maxItems, nPresent, nAbsent int) ([]item, []probe, []probe, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("read hotpot file (download hotpot_dev_distractor_v1.json): %w", err)
	}
	// context: [[title, [sentences...]], ...]; supporting_facts: [[title, idx], ...]
	var data []struct {
		Question        string              `json:"question"`
		Answer          string              `json:"answer"`
		Context         [][]json.RawMessage `json:"context"`
		SupportingFacts [][]json.RawMessage `json:"supporting_facts"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, nil, nil, fmt.Errorf("parse hotpot json: %w", err)
	}
	if len(data) < seedExamples+1 {
		return nil, nil, nil, fmt.Errorf("hotpot file has %d examples, need > %d", len(data), seedExamples)
	}

	titleSents := func(ctx [][]json.RawMessage) map[string]string {
		out := map[string]string{}
		for _, pair := range ctx {
			if len(pair) != 2 {
				continue
			}
			var title string
			var sents []string
			_ = json.Unmarshal(pair[0], &title)
			_ = json.Unmarshal(pair[1], &sents)
			if title != "" {
				out[slug(title)] = strings.Join(sents, " ")
			}
		}
		return out
	}
	firstSupport := func(sf [][]json.RawMessage) string {
		if len(sf) == 0 || len(sf[0]) == 0 {
			return ""
		}
		var title string
		_ = json.Unmarshal(sf[0][0], &title)
		return slug(title)
	}

	var items []item
	var present, absent []probe
	seen := map[string]bool{}

	for i := 0; i < seedExamples && len(items) < maxItems; i++ {
		ex := data[i]
		for concept, content := range titleSents(ex.Context) {
			if content == "" || seen[concept] {
				continue
			}
			seen[concept] = true
			items = append(items, item{Concept: concept, Content: content})
		}
		if len(present) < nPresent && ex.Question != "" {
			if gold := firstSupport(ex.SupportingFacts); gold != "" {
				present = append(present, probe{Query: ex.Question, Gold: gold, Present: true})
			}
		}
	}
	// Absent probes from disjoint later examples (their paragraphs not seeded).
	for i := seedExamples; i < len(data) && len(absent) < nAbsent; i++ {
		if data[i].Question != "" {
			absent = append(absent, probe{Query: data[i].Question, Gold: "", Present: false})
		}
	}
	return items, present, absent, nil
}
