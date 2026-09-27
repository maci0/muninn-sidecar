package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/maci0/muninn-sidecar/internal/mcpclient"
)

// --- seeding ---

func seedCorpus(ctx context.Context, c *mcpclient.Client, vault string, items []item) error {
	const batchSize = 25
	for start := 0; start < len(items); start += batchSize {
		end := start + batchSize
		if end > len(items) {
			end = len(items)
		}
		mems := make([]map[string]any, 0, end-start)
		for _, it := range items[start:end] {
			mems = append(mems, map[string]any{
				"concept": it.Concept,
				"content": it.Content,
				"summary": it.Content,
				"type":    "reference",
				// Content-addressed, like the live store: rerunning -seed over
				// the same corpus updates those memories instead of writing a
				// second copy of each one.
				"dedup_key": mcpclient.DedupKey(vault, it.Concept, it.Content),
			})
		}
		if _, err := c.Call(ctx, "muninn_remember_batch", map[string]any{
			"vault":    vault,
			"memories": mems,
		}); err != nil {
			return fmt.Errorf("seed batch [%d:%d]: %w", start, end, err)
		}
		fmt.Fprintf(os.Stderr, "  seeded %d/%d\n", end, len(items))
	}
	return nil
}

// --- recall ---

type recalledMemory struct {
	Concept     string  `json:"concept"`
	Content     string  `json:"content"`
	Score       float64 `json:"score"`
	VectorScore float64 `json:"vector_score"`
}

// recallMems issues one recall and parses the memories.
func recallMems(ctx context.Context, c *mcpclient.Client, vault, query string, limit int, mode string) ([]recalledMemory, error) {
	args := map[string]any{"vault": vault, "context": []string{query}, "limit": limit, "threshold": 0.05}
	if mode != "" {
		args["mode"] = mode
	}
	resp, err := c.Call(ctx, "muninn_recall", args)
	if err != nil {
		return nil, err
	}
	return parseRecall(resp)
}

// splitQuery decomposes a query into sub-queries for multi-recall: the full
// query plus each capitalized entity span (a no-LLM proxy for the "hops" a
// multi-hop question references). Deduped, full query first.
func splitQuery(q string) []string {
	subs := []string{q}
	seen := map[string]bool{q: true}
	words := strings.Fields(q)
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
	for _, w := range words {
		// unicode.IsUpper, not an 'A'..'Z' range test: a German noun ("Migrations-
		// strategie"), a Turkish or Cyrillic proper noun, and any CJK entity carry
		// no ASCII uppercase at all, so the range test silently extracted no
		// sub-queries for those queries and multi-hop recall collapsed to a single
		// recall of the whole question.
		r := []rune(w)
		if len(r) > 0 && unicode.IsUpper(r[0]) {
			cur = append(cur, w)
		} else {
			flush()
		}
	}
	flush()
	return subs
}

// recallMerged does a single recall, or (multi) splits the query, recalls each
// sub-query, and merges by best vector_score per concept — a transparent,
// no-LLM way to surface both hops of a multi-hop question.
func recallMerged(ctx context.Context, c *mcpclient.Client, vault, query string, limit int, mode string, multi bool) ([]recalledMemory, error) {
	if !multi {
		return recallMems(ctx, c, vault, query, limit, mode)
	}
	return recallSubqueries(ctx, c, vault, splitQuery(query), limit, mode), nil
}

// recallSubqueries recalls each sub-query and merges by best vector_score per
// concept — shared by the no-LLM entity split and the LLM query-rewrite path.
func recallSubqueries(ctx context.Context, c *mcpclient.Client, vault string, subs []string, limit int, mode string) []recalledMemory {
	best := map[string]recalledMemory{}
	for _, sub := range subs {
		ms, err := recallMems(ctx, c, vault, sub, limit, mode)
		if err != nil {
			continue
		}
		for _, m := range ms {
			if cur, ok := best[m.Concept]; !ok || m.VectorScore > cur.VectorScore {
				best[m.Concept] = m
			}
		}
	}
	out := make([]recalledMemory, 0, len(best))
	for _, m := range best {
		out = append(out, m)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].VectorScore > out[j].VectorScore })
	return out
}

func parseRecall(body []byte) ([]recalledMemory, error) {
	var rpc struct {
		Result struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &rpc); err != nil {
		return nil, err
	}
	for _, ct := range rpc.Result.Content {
		if ct.Type != "text" {
			continue
		}
		var inner struct {
			Memories []recalledMemory `json:"memories"`
		}
		if err := json.Unmarshal([]byte(ct.Text), &inner); err != nil {
			return nil, err
		}
		return inner.Memories, nil
	}
	return nil, nil
}

// --- probing ---

// probeOpts configures query construction and reranking for an experiment run.
type probeOpts struct {
	mode         string
	transform    string   // none | distractors | emphasis | repeat-last
	distractN    int      // prior unrelated turns to prepend
	rerank       string   // none | lexical
	rerankLambda float64  // lexical rerank weight
	multiRecall  bool     // split the query into entity spans, recall each, merge
	rewriter     rewriter // optional LLM query rewrite/decomposition before recall
	rewriteN     int      // max sub-queries (incl. original) from the rewriter
}

// lexOverlap is the word-set Jaccard of two strings (cheap lexical similarity).
func lexOverlap(a, b string) float64 {
	wa := strings.Fields(strings.ToLower(a))
	wb := strings.Fields(strings.ToLower(b))
	if len(wa) == 0 || len(wb) == 0 {
		return 0
	}
	set := make(map[string]struct{}, len(wa))
	for _, w := range wa {
		set[w] = struct{}{}
	}
	inter := 0
	seen := make(map[string]struct{}, len(wb))
	for _, w := range wb {
		if _, dup := seen[w]; dup {
			continue
		}
		seen[w] = struct{}{}
		if _, ok := set[w]; ok {
			inter++
		}
	}
	union := len(set) + len(seen) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

// transformQuery builds the query actually sent, simulating how the proxy would
// construct it from conversation context.
func transformQuery(opt probeOpts, probes []probe, i int) string {
	q := probes[i].Query
	switch opt.transform {
	case "distractors":
		// Prepend N other probes' queries as prior unrelated turns, then the real
		// question last — tests whether multi-turn concatenation dilutes recall.
		var b strings.Builder
		for d := 1; d <= opt.distractN; d++ {
			j := (i + d*37) % len(probes)
			b.WriteString(probes[j].Query)
			b.WriteString("\n")
		}
		b.WriteString(q)
		return b.String()
	case "emphasis":
		// Latest turn FIRST, then prior unrelated turns — mirrors the proxy's
		// recency-emphasis query construction; should recover the retrieval that
		// plain "distractors" loses.
		var b strings.Builder
		b.WriteString(q)
		for d := 1; d <= opt.distractN; d++ {
			j := (i + d*37) % len(probes)
			b.WriteString("\n")
			b.WriteString(probes[j].Query)
		}
		return b.String()
	case "repeat-last":
		return q + "\n" + q
	default:
		return q
	}
}

// probeResult records, for one probe, the ranked recall output (by score order
// as MuninnDB returns it) and the rank of the gold concept.
type probeResult struct {
	probe
	Recalled []recalledMemory `json:"recalled"`
	// rankByScore / rankByVec: 0-based rank of gold in the result list ordered
	// by score / vector_score; -1 if gold absent from results.
	RankByScore int `json:"rank_by_score"`
	RankByVec   int `json:"rank_by_vec"`
	RankRerank  int `json:"rank_rerank"` // rank under the configured rerank (== vec when rerank=none)
	// Article-level ranks: rank of the first recalled memory from the SAME
	// source article as gold (concept prefix before '#'). For corpora without
	// '#' in concepts this equals the exact rank. Captures topic-level retrieval,
	// which is what injection needs — a sibling paragraph is usually also useful.
	RankArtScore int `json:"rank_art_score"`
	RankArtVec   int `json:"rank_art_vec"`
}

func runProbes(ctx context.Context, c *mcpclient.Client, vault string, probes []probe, limit int, opt probeOpts) ([]probeResult, error) {
	out := make([]probeResult, 0, len(probes))
	var totalDur time.Duration
	var timed int
	for i, pr := range probes {
		query := transformQuery(opt, probes, i)
		t0 := time.Now()
		var mems []recalledMemory
		var err error
		if opt.rewriter != nil {
			// LLM query rewrite/decomposition → recall each sub-query, merge.
			subs := opt.rewriter.Rewrite(ctx, query, opt.rewriteN)
			mems = recallSubqueries(ctx, c, vault, subs, limit, opt.mode)
		} else {
			mems, err = recallMerged(ctx, c, vault, query, limit, opt.mode, opt.multiRecall)
		}
		totalDur += time.Since(t0)
		timed++
		if err != nil {
			// Skip transient failures rather than aborting the whole run; a
			// dropped probe just doesn't contribute to the metrics.
			fmt.Fprintf(os.Stderr, "  warn: probe %d skipped: %v\n", i, err)
			continue
		}
		// Optional lexical rerank field: vector + lambda * token-overlap(query, content).
		vecField := func(m recalledMemory) float64 { return m.VectorScore }
		rerankField := vecField
		if opt.rerank == "lexical" {
			rerankField = func(m recalledMemory) float64 {
				return m.VectorScore + opt.rerankLambda*lexOverlap(pr.Query, m.Content)
			}
		}
		out = append(out, probeResult{
			probe:        pr,
			Recalled:     mems,
			RankByScore:  rankOf(mems, pr.Gold, func(m recalledMemory) float64 { return m.Score }),
			RankByVec:    rankOf(mems, pr.Gold, vecField),
			RankRerank:   rankOf(mems, pr.Gold, rerankField),
			RankArtScore: rankArticleOf(mems, pr.Gold, func(m recalledMemory) float64 { return m.Score }),
			RankArtVec:   rankArticleOf(mems, pr.Gold, vecField),
		})
		if (i+1)%25 == 0 {
			fmt.Fprintf(os.Stderr, "  probed %d/%d\n", i+1, len(probes))
		}
	}
	if timed > 0 {
		fmt.Fprintf(os.Stderr, "recall latency: avg %.1fms over %d calls\n",
			float64(totalDur.Microseconds())/float64(timed)/1000, timed)
	}
	return out, nil
}
