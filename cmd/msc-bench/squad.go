package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// The SQuAD v1.1/v2.0 JSON shape (only the fields we need).
type squadFile struct {
	Data []squadArticle `json:"data"`
}

type squadArticle struct {
	Title      string      `json:"title"`
	Paragraphs []squadPara `json:"paragraphs"`
}

type squadPara struct {
	Context string    `json:"context"`
	QAs     []squadQA `json:"qas"`
}

type squadQA struct {
	Question     string        `json:"question"`
	IsImpossible bool          `json:"is_impossible"`
	Answers      []squadAnswer `json:"answers"`
}

type squadAnswer struct {
	Text string `json:"text"`
}

// seedChunks appends a paragraph's memories under base: one per sentence when
// chunk is "sentence" (concept base#s, which tests whether finer chunks
// localize the answer better at the cost of more siblings), one per paragraph
// otherwise. Reports how many sentence chunks were seeded, which bounds the
// gold concepts a present probe may name.
func seedChunks(items []item, base, context, chunk string, maxItems int) ([]item, int) {
	if chunk != "sentence" {
		return append(items, item{Concept: base, Content: context}), 0
	}
	seededSents := 0
	for si, s := range splitSentences(context) {
		if len(items) >= maxItems {
			break
		}
		items = append(items, item{Concept: fmt.Sprintf("%s#%d", base, si), Content: s})
		seededSents++
	}
	return items, seededSents
}

// presentProbe returns the paragraph's first answerable question, golded on the
// seeded chunk that holds the answer. ok is false when the paragraph has no
// answerable question, or when sentence chunking left the answer in a sentence
// that maxItems truncation never seeded: an unseeded gold could not be
// retrieved. One probe per paragraph keeps the gold unambiguous.
func (p squadPara) presentProbe(base, chunk string, seededSents int) (probe, bool) {
	for _, qa := range p.QAs {
		if qa.IsImpossible || qa.Question == "" || len(qa.Answers) == 0 {
			continue
		}
		gold := base
		if chunk == "sentence" {
			si := sentenceContaining(p.Context, qa.Answers[0].Text)
			if si < 0 || si >= seededSents {
				continue
			}
			gold = fmt.Sprintf("%s#%d", base, si)
		}
		return probe{Query: qa.Question, Gold: gold, Answer: qa.Answers[0].Text, Present: true}, true
	}
	return probe{}, false
}

// negativeProbe returns the paragraph's first answerable question as a negative
// probe. Whatever paragraph it came from was never seeded, so a correct gate
// must suppress it.
func (p squadPara) negativeProbe() (probe, bool) {
	for _, qa := range p.QAs {
		if qa.IsImpossible || qa.Question == "" {
			continue
		}
		return probe{Query: qa.Question, Gold: "", Present: false}, true
	}
	return probe{}, false
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
	sq, err := loadSquad(path)
	if err != nil {
		return nil, nil, nil, err
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

			base := fmt.Sprintf("%s#%d", slug(art.Title), pi)
			var seededSents int
			items, seededSents = seedChunks(items, base, para.Context, chunk, maxItems)

			if len(present) < nPresent {
				if pr, ok := para.presentProbe(base, chunk, seededSents); ok {
					present = append(present, pr)
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
			if pr, ok := para.negativeProbe(); ok {
				absent = append(absent, pr)
			}
		}
	}
	return items, present, absent, nil
}

// genSquadHardNeg builds a HARD-negative gate instrument: instead of drawing
// negatives from disjoint off-topic articles, it seeds the even-index paragraphs
// of each article and uses the answerable questions of the ODD-index paragraphs
// (same article, never seeded) as negatives. These share heavy topical/lexical
// overlap with seeded memories — the worst case for a cosine gate — yet no seeded
// memory answers them, so a correct gate must still suppress. This is the one
// suppression case the synthetic study cannot model (its noise is a cleanly
// separated low-score cluster). Present probes come from the seeded even paragraphs.
// chunk ("paragraph"|"sentence") controls seed granularity, so the precision
// ceiling can be measured against finer chunks (does localizing the answer
// separate the answer-bearing chunk from same-article siblings?).
func genSquadHardNeg(path string, seedArticles, maxItems, nPresent, nAbsent int, chunk string) ([]item, []probe, []probe, error) {
	sq, err := loadSquad(path)
	if err != nil {
		return nil, nil, nil, err
	}
	if len(sq.Data) < seedArticles {
		return nil, nil, nil, fmt.Errorf("squad file has %d articles, need >= %d", len(sq.Data), seedArticles)
	}

	var items []item
	var present, hardNeg []probe
	seenContent := map[string]bool{}

	for ai := 0; ai < seedArticles && ai < len(sq.Data); ai++ {
		art := sq.Data[ai]
		for pi, para := range art.Paragraphs {
			if para.Context == "" {
				continue
			}
			if pi%2 != 0 {
				if len(hardNeg) >= nAbsent {
					continue
				}
				// Odd paragraph → never seeded; its question is a hard negative.
				if pr, ok := para.negativeProbe(); ok {
					hardNeg = append(hardNeg, pr)
				}
				continue
			}
			// Even paragraph → seed it + a present probe.
			if len(items) >= maxItems || seenContent[para.Context] {
				continue
			}
			seenContent[para.Context] = true
			base := fmt.Sprintf("%s#%d", slug(art.Title), pi)
			var seededSents int
			items, seededSents = seedChunks(items, base, para.Context, chunk, maxItems)
			if len(present) < nPresent {
				if pr, ok := para.presentProbe(base, chunk, seededSents); ok {
					present = append(present, pr)
				}
			}
		}
	}
	return items, present, hardNeg, nil
}

func loadSquad(path string) (squadFile, error) {
	var sq squadFile
	raw, err := os.ReadFile(path)
	if err != nil {
		return sq, fmt.Errorf("read squad file: %w", err)
	}
	if err := json.Unmarshal(raw, &sq); err != nil {
		return sq, fmt.Errorf("parse squad json: %w", err)
	}
	return sq, nil
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
