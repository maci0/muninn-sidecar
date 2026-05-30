package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// squadFile is the SQuAD v1.1/v2.0 JSON shape (only the fields we need).
type squadFile struct {
	Data []struct {
		Title      string `json:"title"`
		Paragraphs []struct {
			Context string `json:"context"`
			QAs     []struct {
				Question     string `json:"question"`
				IsImpossible bool   `json:"is_impossible"`
				Answers      []struct {
					Text string `json:"text"`
				} `json:"answers"`
			} `json:"qas"`
		} `json:"paragraphs"`
	} `json:"data"`
}

// genSquad builds a retrieval/gate test instrument from a real SQuAD file:
//   - The first `seedArticles` articles are seeded: each paragraph becomes one
//     memory (concept = "title#idx", content = the paragraph).
//   - Present probes are answerable questions from those seeded paragraphs; the
//     gold concept is the paragraph the question was written against.
//   - Absent probes are questions from DISJOINT held-out articles whose
//     paragraphs were never seeded — so a correct gate must suppress them. Using
//     whole held-out articles (not held-out paragraphs of seeded articles) keeps
//     the absent set genuinely off-topic, avoiding same-article contamination.
func genSquad(path string, seedArticles, maxItems, nPresent, nAbsent int, chunk string) ([]item, []probe, []probe, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("read squad file: %w", err)
	}
	var sq squadFile
	if err := json.Unmarshal(raw, &sq); err != nil {
		return nil, nil, nil, fmt.Errorf("parse squad json: %w", err)
	}
	if len(sq.Data) < seedArticles+1 {
		return nil, nil, nil, fmt.Errorf("squad file has %d articles, need > %d", len(sq.Data), seedArticles)
	}

	var items []item
	var present, absent []probe
	seenContent := map[string]bool{}

	// Seeded articles → memories + present probes.
	for ai := 0; ai < seedArticles && len(items) < maxItems; ai++ {
		art := sq.Data[ai]
		for pi, para := range art.Paragraphs {
			if len(items) >= maxItems {
				break
			}
			if para.Context == "" || seenContent[para.Context] {
				continue
			}
			seenContent[para.Context] = true

			// Chunk the paragraph into memories. paragraph: one memory per
			// paragraph (concept title#p). sentence: one memory per sentence
			// (concept title#p#s) — finer granularity, tests whether smaller
			// chunks localize the answer better at the cost of more siblings.
			base := fmt.Sprintf("%s#%d", slug(art.Title), pi)
			goldConcept := base
			if chunk == "sentence" {
				sents := splitSentences(para.Context)
				for si, s := range sents {
					if len(items) >= maxItems {
						break
					}
					items = append(items, item{Concept: fmt.Sprintf("%s#%d", base, si), Content: s})
				}
				// Gold = the sentence containing the answer (set per-probe below).
			} else {
				items = append(items, item{Concept: base, Content: para.Context})
			}

			if len(present) < nPresent {
				for _, qa := range para.QAs {
					if qa.IsImpossible || qa.Question == "" || len(qa.Answers) == 0 {
						continue
					}
					gold := goldConcept
					if chunk == "sentence" {
						// Find the sentence index containing the answer text.
						si := sentenceContaining(para.Context, qa.Answers[0].Text)
						if si < 0 {
							continue
						}
						gold = fmt.Sprintf("%s#%d", base, si)
					}
					present = append(present, probe{Query: qa.Question, Gold: gold, Present: true})
					break // one probe per paragraph keeps gold unambiguous
				}
			}
		}
	}

	// Held-out articles → absent probes (their paragraphs are never seeded).
	for ai := seedArticles; ai < len(sq.Data) && len(absent) < nAbsent; ai++ {
		for _, para := range sq.Data[ai].Paragraphs {
			if len(absent) >= nAbsent {
				break
			}
			for _, qa := range para.QAs {
				if qa.IsImpossible || qa.Question == "" {
					continue
				}
				absent = append(absent, probe{Query: qa.Question, Gold: "", Present: false})
				break
			}
		}
	}
	return items, present, absent, nil
}

// splitSentences splits text on sentence-ending punctuation followed by a space.
func splitSentences(text string) []string {
	var out []string
	start := 0
	for i := 0; i < len(text)-1; i++ {
		if (text[i] == '.' || text[i] == '?' || text[i] == '!') && text[i+1] == ' ' {
			s := strings.TrimSpace(text[start : i+1])
			if s != "" {
				out = append(out, s)
			}
			start = i + 1
		}
	}
	if s := strings.TrimSpace(text[start:]); s != "" {
		out = append(out, s)
	}
	return out
}

// sentenceContaining returns the index of the first sentence containing answer,
// or -1.
func sentenceContaining(text, answer string) int {
	answer = strings.TrimSpace(answer)
	if answer == "" {
		return -1
	}
	for i, s := range splitSentences(text) {
		if strings.Contains(s, answer) {
			return i
		}
	}
	return -1
}

func slug(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, " ", "-")
	s = strings.ReplaceAll(s, "/", "-")
	return s
}
