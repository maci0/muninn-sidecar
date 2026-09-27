package main

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
)

// --- SQuAD QA loader ---

type qaItem struct {
	Question string
	Answers  []string
}

func loadSquadQA(path string) ([]qaItem, error) {
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
			}
		}
	}
	return out, nil
}

// loadFlatQA reads a flat [{question, answer}] JSON, keeping every question
// that carries both. label names the source in the error text. Callers
// pass the official HotpotQA array format ({question, answer, context,
// supporting_facts}, multi-hop questions with a single gold answer including
// yes/no, as seeded by `msc-bench -corpus hotpot`) or the dump format
// `msc-bench -dump-qa` produces for an arbitrary seeded vault.
func loadFlatQA(path, label string) ([]qaItem, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", label, err)
	}
	var data []struct {
		Question string `json:"question"`
		Answer   string `json:"answer"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, fmt.Errorf("parse %s: %w", label, err)
	}
	var out []qaItem
	for _, d := range data {
		if d.Question == "" || d.Answer == "" {
			continue
		}
		out = append(out, qaItem{Question: d.Question, Answers: []string{d.Answer}})
	}
	return out, nil
}

// loadDataset loads every eligible question, shuffles deterministically with
// seed, then truncates to n. Datasets are grouped in file order (SQuAD by
// article), so taking the first n unshuffled would cover only 1-2 articles.
func loadDataset(dataset, path string, n int, seed int64) ([]qaItem, error) {
	var qs []qaItem
	var err error
	switch dataset {
	case "hotpot":
		qs, err = loadFlatQA(path, "hotpot")
	case "generic":
		qs, err = loadFlatQA(path, "generic qa")
	default:
		qs, err = loadSquadQA(path)
	}
	if err != nil {
		return nil, err
	}
	rng := rand.New(rand.NewSource(seed))
	rng.Shuffle(len(qs), func(i, j int) { qs[i], qs[j] = qs[j], qs[i] })
	if len(qs) > n {
		qs = qs[:n]
	}
	return qs, nil
}
