package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/maci0/muninn-sidecar/internal/config"
)

func TestPureHelpers(t *testing.T) {
	if frange(0.0, 0.2, 0.1); len(frange(0, 0.2, 0.1)) != 3 {
		t.Errorf("frange len %d", len(frange(0, 0.2, 0.1)))
	}
	if safeDiv(3, 0) != 0 || safeDiv(1, 4) != 0.25 {
		t.Error("safeDiv")
	}
	if slug("Khyber Reach/X") != "khyber-reach-x" {
		t.Errorf("slug %q", slug("Khyber Reach/X"))
	}
	if articleOf("title#3") != "title" || articleOf("plain") != "plain" {
		t.Error("articleOf")
	}
	if coinName(0) == coinName(1) || coinName(0) == "" {
		t.Error("coinName not unique/nonempty")
	}
	if lexOverlap("a b c", "b c d") < 0.49 || lexOverlap("x", "y") != 0 {
		t.Errorf("lexOverlap")
	}
}

func TestEnvAndToken(t *testing.T) {
	t.Setenv("MUNINN_MCP_URL", "http://x/mcp")
	if config.MCPURL("") != "http://x/mcp" {
		t.Error("MUNINN_MCP_URL not honored")
	}
	if resolveToken("explicit") != "explicit" {
		t.Error("resolveToken flag precedence")
	}
}

func TestSplitSentences(t *testing.T) {
	s := splitSentences("One. Two? Three! Four")
	if len(s) != 4 {
		t.Fatalf("got %d: %v", len(s), s)
	}
	if sentenceContaining("Alpha here. Beta there.", "Beta") != 1 {
		t.Error("sentenceContaining")
	}
	if sentenceContaining("nope", "zzz") != -1 {
		t.Error("sentenceContaining miss")
	}
}

func TestRankHelpers(t *testing.T) {
	mems := []recalledMemory{
		{Concept: "a#1", VectorScore: 0.4},
		{Concept: "b#2", VectorScore: 0.9},
		{Concept: "a#3", VectorScore: 0.7},
	}
	vf := func(m recalledMemory) float64 { return m.VectorScore }
	if rankOf(mems, "b#2", vf) != 0 {
		t.Error("rankOf top")
	}
	if rankOf(mems, "missing", vf) != -1 {
		t.Error("rankOf missing")
	}
	if rankArticleOf(mems, "a#1", vf) != 1 { // first 'a' article by score is a#3 (0.7) at rank 1
		t.Errorf("rankArticleOf got %d", rankArticleOf(mems, "a#1", vf))
	}
	if v, ok := topByField(mems, vf); !ok || v != 0.9 {
		t.Errorf("topByField %v", v)
	}
	if c, ok := topConcept(mems, vf); !ok || c != "b#2" {
		t.Errorf("topConcept %q", c)
	}
}

func TestTransformQuery(t *testing.T) {
	probes := []probe{{Query: "real question"}, {Query: "noise one"}, {Query: "noise two"}}
	if transformQuery(probeOpts{transform: "none"}, probes, 0) != "real question" {
		t.Error("none")
	}
	if got := transformQuery(probeOpts{transform: "distractors", distractN: 1}, probes, 0); got == "real question" {
		t.Error("distractors should prepend")
	}
	if got := transformQuery(probeOpts{transform: "emphasis", distractN: 1}, probes, 0); len(got) <= len("real question") {
		t.Error("emphasis should append distractors")
	}
}

func TestGenerators(t *testing.T) {
	for _, g := range []struct {
		name            string
		items           []item
		present, absent []probe
	}{
		func() (g struct {
			name            string
			items           []item
			present, absent []probe
		}) {
			g.name = "dataset"
			var err error
			g.items, g.present, g.absent, err = genDataset(1, 30, 10, 5)
			if err != nil {
				t.Fatalf("genDataset: %v", err)
			}
			return
		}(),
		func() (g struct {
			name            string
			items           []item
			present, absent []probe
		}) {
			g.name = "diverse"
			var err error
			g.items, g.present, g.absent, err = genDiverse(1, 30, 10, 5)
			if err != nil {
				t.Fatalf("genDiverse: %v", err)
			}
			return
		}(),
		func() (g struct {
			name            string
			items           []item
			present, absent []probe
		}) {
			g.name = "agentmem"
			var err error
			g.items, g.present, g.absent, err = genAgentMem(40, 10)
			if err != nil {
				t.Fatalf("genAgentMem: %v", err)
			}
			return
		}(),
	} {
		if len(g.items) == 0 || len(g.present) == 0 {
			t.Errorf("%s: empty items/present", g.name)
		}
		for _, p := range g.present {
			if p.Query == "" || p.Gold == "" || !p.Present {
				t.Errorf("%s: bad present probe %+v", g.name, p)
			}
		}
		for _, p := range g.absent {
			if p.Present || p.Gold != "" {
				t.Errorf("%s: bad absent probe %+v", g.name, p)
			}
		}
	}
	// genFacts (no args).
	it, pr, ab := genFacts()
	if len(it) == 0 || len(pr) == 0 || len(ab) == 0 {
		t.Error("genFacts empty")
	}
	// agentmem answers populated.
	_, amp, _, err := genAgentMem(8, 0)
	if err != nil {
		t.Fatalf("genAgentMem: %v", err)
	}
	for _, p := range amp {
		if p.Answer == "" {
			t.Errorf("agentmem present probe missing answer: %+v", p)
		}
	}
}

// TestGeneratorNamespaceBounds: absent probes past a generator's modular
// namespace would wrap onto seeded items, so oversized runs must error.
func TestGeneratorNamespaceBounds(t *testing.T) {
	if _, _, _, err := genDataset(1, 8000, 10, 5); err == nil {
		t.Error("genDataset should reject absent probes past the triple namespace")
	}
	if _, _, _, err := genDiverse(1, coinNameSpace, 10, 5); err == nil {
		t.Error("genDiverse should reject absent probes past the coinName namespace")
	}
	if _, _, _, err := genAgentMem(coinNameSpace, 1); err == nil {
		t.Error("genAgentMem should reject absent probes past the coinName namespace")
	}
	// In-range sizing still works, including nAbsent=0 at the namespace edge.
	if _, _, _, err := genAgentMem(coinNameSpace, 0); err != nil {
		t.Errorf("genAgentMem at namespace edge: %v", err)
	}
	// With no absent probes an oversized n must blame seeded-item collisions,
	// not absent probes that do not exist.
	_, _, _, err := genAgentMem(coinNameSpace+1, 0)
	if err == nil {
		t.Fatal("genAgentMem should reject n past the coinName namespace even with 0 absent probes")
	}
	if !contains(err.Error(), "seeded items would collide") || contains(err.Error(), "absent probes") {
		t.Errorf("nAbsent=0 overflow error should blame seeded items: %v", err)
	}
	// With absent probes overflowing past an in-range n, keep the wrap wording.
	_, _, _, err = genAgentMem(coinNameSpace-1, 1)
	if err == nil {
		t.Fatal("genAgentMem should reject absent probes past the namespace")
	}
	if !contains(err.Error(), "absent probes would wrap onto seeded items") {
		t.Errorf("absent overflow error should blame absent probes: %v", err)
	}
}

func TestGenSquadAndHotpotFromFile(t *testing.T) {
	dir := t.TempDir()
	squad := filepath.Join(dir, "squad.json")
	os.WriteFile(squad, []byte(`{"data":[
	  {"title":"A","paragraphs":[{"context":"Paris is the capital of France.","qas":[{"question":"capital of France?","is_impossible":false,"answers":[{"text":"Paris"}]}]}]},
	  {"title":"B","paragraphs":[{"context":"Berlin is in Germany.","qas":[{"question":"where is Berlin?","is_impossible":false,"answers":[{"text":"Germany"}]}]}]}
	]}`), 0o644)
	items, present, absent, err := genSquad(squad, 1, 10, 5, 5, "paragraph")
	if err != nil || len(items) != 1 || len(present) != 1 {
		t.Fatalf("genSquad: err=%v items=%d present=%d", err, len(items), len(present))
	}
	if present[0].Answer != "Paris" {
		t.Errorf("squad present probe should carry the answer span, got %q", present[0].Answer)
	}
	if len(absent) != 1 {
		t.Errorf("expected 1 absent (held-out article B), got %d", len(absent))
	}
	// sentence chunking.
	itS, _, _, err := genSquad(squad, 1, 10, 5, 5, "sentence")
	if err != nil || len(itS) == 0 {
		t.Errorf("genSquad sentence: err=%v items=%d", err, len(itS))
	}

	hp := filepath.Join(dir, "hotpot.json")
	os.WriteFile(hp, []byte(`[
	  {"question":"q1","answer":"yes","context":[["T1",["s1.","s2."]],["T2",["s3."]]],"supporting_facts":[["T1",0]]},
	  {"question":"q2","answer":"no","context":[["T3",["s4."]]],"supporting_facts":[["T3",0]]}
	]`), 0o644)
	hi, hpres, _, err := genHotpot(hp, 1, 10, 5, 5)
	if err != nil || len(hi) == 0 || len(hpres) == 0 {
		t.Fatalf("genHotpot: err=%v items=%d present=%d", err, len(hi), len(hpres))
	}
	if hpres[0].Answer != "yes" {
		t.Errorf("hotpot present probe should carry the answer, got %q", hpres[0].Answer)
	}
	if _, _, _, err := genSquad(filepath.Join(dir, "nope.json"), 1, 1, 1, 1, "paragraph"); err == nil {
		t.Error("expected error on missing file")
	}
}

// TestGenSquadSentenceTruncation: when maxItems cuts a paragraph's sentence
// seeding short, no present probe may point at an unseeded gold sentence.
func TestGenSquadSentenceTruncation(t *testing.T) {
	dir := t.TempDir()
	squad := filepath.Join(dir, "squad.json")
	// Three sentences; the answer lives in the third. maxItems=2 seeds only the
	// first two.
	os.WriteFile(squad, []byte(`{"data":[
	  {"title":"A","paragraphs":[{"context":"First sentence here. Second sentence here. Answer is Zanzibar.","qas":[{"question":"where?","is_impossible":false,"answers":[{"text":"Zanzibar"}]}]}]},
	  {"title":"B","paragraphs":[{"context":"Other.","qas":[{"question":"other?","is_impossible":false,"answers":[{"text":"Other"}]}]}]}
	]}`), 0o644)
	items, present, _, err := genSquad(squad, 1, 2, 5, 5, "sentence")
	if err != nil {
		t.Fatalf("genSquad: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("expected 2 seeded sentences, got %d", len(items))
	}
	if len(present) != 0 {
		t.Errorf("probe with unseeded gold must be skipped, got %+v", present)
	}
	// Untruncated: the probe appears with the answer sentence as gold.
	_, presentFull, _, err := genSquad(squad, 1, 10, 5, 5, "sentence")
	if err != nil || len(presentFull) != 1 || presentFull[0].Gold != "a#0#2" {
		t.Fatalf("full seed: err=%v present=%+v", err, presentFull)
	}
	// Same guard in the hard-negative generator.
	if _, hnPresent, _, err := genSquadHardNeg(squad, 1, 2, 5, 5, "sentence"); err != nil {
		t.Fatalf("genSquadHardNeg: %v", err)
	} else if len(hnPresent) != 0 {
		t.Errorf("hard-neg: probe with unseeded gold must be skipped, got %+v", hnPresent)
	}
}

func TestGenSquadHardNeg(t *testing.T) {
	dir := t.TempDir()
	// One article, 3 paragraphs: even (0,2) seeded → present probes; odd (1) is a
	// same-article hard negative (never seeded).
	squad := filepath.Join(dir, "squad.json")
	os.WriteFile(squad, []byte(`{"data":[
	  {"title":"Newcastle","paragraphs":[
	    {"context":"The Town Moor is a large area of common land in Newcastle.","qas":[{"question":"what is the Town Moor?","is_impossible":false,"answers":[{"text":"common land"}]}]},
	    {"context":"The Hoppings funfair is held on the Town Moor each June.","qas":[{"question":"when is the Hoppings held?","is_impossible":false,"answers":[{"text":"June"}]}]},
	    {"context":"Freemen of the city have grazing rights on the moor.","qas":[{"question":"who has grazing rights?","is_impossible":false,"answers":[{"text":"Freemen"}]}]}
	  ]}
	]}`), 0o644)
	items, present, hardNeg, err := genSquadHardNeg(squad, 1, 10, 5, 5, "paragraph")
	if err != nil {
		t.Fatalf("genSquadHardNeg: %v", err)
	}
	if len(items) != 2 { // paragraphs 0 and 2 seeded
		t.Errorf("expected 2 seeded items (even paragraphs), got %d", len(items))
	}
	if len(present) != 2 {
		t.Errorf("expected 2 present probes, got %d", len(present))
	}
	if len(hardNeg) != 1 { // paragraph 1's question
		t.Fatalf("expected 1 hard-negative probe (odd paragraph), got %d", len(hardNeg))
	}
	if hardNeg[0].Present || hardNeg[0].Gold != "" {
		t.Errorf("hard negative must be Present=false, Gold=\"\": %+v", hardNeg[0])
	}
	if hardNeg[0].Query != "when is the Hoppings held?" {
		t.Errorf("hard negative should come from the odd paragraph, got %q", hardNeg[0].Query)
	}
	// Present probes carry their gold answer span (for QA dumps).
	for _, p := range present {
		if p.Answer == "" {
			t.Errorf("present probe missing answer: %+v", p)
		}
	}
	// Sentence chunking: the two seeded paragraphs (0 and 2) are single sentences
	// each, so still 2 items, but the hard negative is unchanged.
	itS, _, hnS, err := genSquadHardNeg(squad, 1, 10, 5, 5, "sentence")
	if err != nil || len(itS) == 0 {
		t.Fatalf("genSquadHardNeg sentence: err=%v items=%d", err, len(itS))
	}
	if len(hnS) != 1 {
		t.Errorf("sentence chunk: expected 1 hard negative, got %d", len(hnS))
	}
	// Missing file errors.
	if _, _, _, err := genSquadHardNeg(filepath.Join(dir, "nope.json"), 1, 1, 1, 1, "paragraph"); err == nil {
		t.Error("expected error on missing file")
	}
}

func TestWriteQA(t *testing.T) {
	path := filepath.Join(t.TempDir(), "qa.json")
	probes := []probe{
		{Query: "q1", Answer: "a1", Present: true},
		{Query: "q2", Answer: "", Present: true},    // skipped (no answer)
		{Query: "q3", Answer: "a3", Present: false}, // skipped (absent)
	}
	n, err := writeQA(path, probes)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("expected 1 QA pair written, got %d", n)
	}
	data, _ := os.ReadFile(path)
	s := string(data)
	if !contains(s, "q1") || !contains(s, "a1") || contains(s, "q2") || contains(s, "q3") {
		t.Errorf("writeQA content: %s", s)
	}
	// No answer-carrying probes → error, never a silently empty file.
	if _, err := writeQA(path, []probe{{Query: "q", Present: true}}); err == nil {
		t.Error("writeQA should reject probes without answers")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestAnalyzeAndGate(t *testing.T) {
	results := []probeResult{
		{probe: probe{Gold: "g", Present: true}, Recalled: []recalledMemory{{Concept: "g", VectorScore: 0.8}}, RankByVec: 0, RankRerank: 0, RankArtVec: 0, RankByScore: 0, RankArtScore: 0},
		{probe: probe{Gold: "", Present: false}, Recalled: []recalledMemory{{Concept: "x", VectorScore: 0.2}}, RankByVec: -1, RankRerank: -1, RankArtVec: -1, RankByScore: -1, RankArtScore: -1},
	}
	rep := analyze(results)
	if rep.NPresent != 1 || rep.NAbsent != 1 {
		t.Errorf("analyze counts: %+v", rep)
	}
	if rep.RetrievalVec.R1 != 1.0 {
		t.Errorf("present gold at rank0 should give R@1 1.0, got %v", rep.RetrievalVec.R1)
	}
}

// TestHeldOutBest: the in-sample best threshold and the held-out one must be
// allowed to disagree. The tune half (probes whose query hashes even, see
// tuneHalf) is separable at T=0.6 while the full set favors T=0.4, so
// heldOutBest picks 0.6 and scores it on the eval half (odd query hash), where
// the present probe falls below the gate.
func TestHeldOutBest(t *testing.T) {
	vf := func(m recalledMemory) float64 { return m.VectorScore }
	mem := func(v float64) []recalledMemory { return []recalledMemory{{Concept: "g", VectorScore: v}} }
	// Queries chosen so their fnv32a hash parity puts them in the named half.
	if !tuneHalf("tune present") || !tuneHalf("tune absent") {
		t.Fatal("test setup: tune queries must hash into the tune half")
	}
	if tuneHalf("eval present a") || tuneHalf("eval absent a") {
		t.Fatal("test setup: eval queries must hash into the eval half")
	}
	results := []probeResult{
		{probe: probe{Query: "tune present", Gold: "g", Present: true}, Recalled: mem(0.65)},
		{probe: probe{Query: "eval present a", Gold: "g", Present: true}, Recalled: mem(0.45)},
		{probe: probe{Query: "tune absent", Present: false}, Recalled: mem(0.50)},
		{probe: probe{Query: "eval absent a", Present: false}, Recalled: mem(0.30)},
	}
	thresholds := []float64{0.4, 0.6}

	inSample := bestGate(gateSweep(results[:2], results[2:], thresholds, vf))
	if inSample.Threshold != 0.4 {
		t.Fatalf("in-sample best threshold: got %v, want 0.4", inSample.Threshold)
	}
	held := heldOutBest(results, thresholds, vf)
	if held.Threshold != 0.6 {
		t.Errorf("held-out threshold: got %v, want 0.6 (tuned on the tune half)", held.Threshold)
	}
	if held.GateAcc != 0.5 || held.GateF1 != 0 {
		t.Errorf("held-out eval metrics: acc=%v f1=%v, want acc=0.5 f1=0", held.GateAcc, held.GateF1)
	}
	if held.Threshold == inSample.Threshold {
		t.Error("test setup must make in-sample and held-out disagree")
	}
	// Degenerate input must not panic and must stay JSON-encodable (all zeros).
	if p := heldOutBest(nil, thresholds, vf); p.GateAcc != 0 {
		t.Errorf("empty results: got %+v, want zero metrics", p)
	}
}

// TestTuneHalfDecorrelatedFromCategory: the generators assign question category
// by index modulo an even number, so an index-parity split would give each half
// disjoint category sets. The query-hash split must land every agentmem
// category (concept prefix) in both halves.
func TestTuneHalfDecorrelatedFromCategory(t *testing.T) {
	_, present, _, err := genAgentMem(64, 0)
	if err != nil {
		t.Fatalf("genAgentMem: %v", err)
	}
	inTune, inEval := map[string]bool{}, map[string]bool{}
	for _, p := range present {
		cat, _, _ := strings.Cut(p.Gold, "-")
		if tuneHalf(p.Query) {
			inTune[cat] = true
		} else {
			inEval[cat] = true
		}
	}
	for _, cat := range []string{"svc", "mod", "api", "env"} {
		if !inTune[cat] || !inEval[cat] {
			t.Errorf("category %q missing from a half: tune=%v eval=%v", cat, inTune[cat], inEval[cat])
		}
	}
}

// TestGateSweepEmpty: with no probe results (every probe skipped, or -present 0
// -absent 0) the gate must report 0, not NaN, so -json output stays encodable.
func TestGateSweepEmpty(t *testing.T) {
	vf := func(m recalledMemory) float64 { return m.VectorScore }
	pts := gateSweep(nil, nil, []float64{0.5}, vf)
	if len(pts) != 1 || pts[0].GateAcc != 0 {
		t.Fatalf("empty gateSweep must yield acc 0: %+v", pts)
	}
	rep := analyze(nil)
	if err := json.NewEncoder(io.Discard).Encode(rep); err != nil {
		t.Errorf("empty report must JSON-encode: %v", err)
	}
}

func FuzzParseRecall(f *testing.F) {
	f.Add([]byte(`{"result":{"content":[{"type":"text","text":"{\"memories\":[{\"concept\":\"c\",\"score\":0.5,\"vector_score\":0.4}]}"}]}}`))
	f.Add([]byte(`garbage`))
	f.Add([]byte(`{"result":{"content":[]}}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = parseRecall(data)
	})
}

func FuzzTransformQuery(f *testing.F) {
	f.Add("q", 3)
	f.Fuzz(func(t *testing.T, q string, n int) {
		if n < 0 {
			n = -n
		}
		if n > 50 {
			n = 50
		}
		probes := []probe{{Query: q}, {Query: "a"}, {Query: "b"}}
		for _, mode := range []string{"none", "distractors", "emphasis", "repeat-last"} {
			_ = transformQuery(probeOpts{transform: mode, distractN: n}, probes, 0)
		}
	})
}

func FuzzStringHelpers(f *testing.F) {
	f.Add("Khyber Reach/X", "Beta there. Gamma.")
	f.Fuzz(func(t *testing.T, a, b string) {
		_ = slug(a)
		_ = articleOf(a)
		_ = lexOverlap(a, b)
		ss := splitSentences(b)
		_ = sentenceContaining(b, a)
		for _, s := range ss {
			if s == "" {
				t.Fatal("splitSentences returned empty segment")
			}
		}
	})
}

func FuzzGenInts(f *testing.F) {
	f.Add(50, 10)
	f.Fuzz(func(t *testing.T, n, absent int) {
		if n < 0 {
			n = -n
		}
		if n > 500 {
			n = 500
		}
		if absent < 0 {
			absent = -absent
		}
		if absent > 100 {
			absent = 100
		}
		if _, _, _, err := genAgentMem(n, absent); err != nil {
			t.Fatalf("genAgentMem(%d, %d) within namespace: %v", n, absent, err)
		}
		_ = coinName(n)
	})
}

func TestSplitQuery(t *testing.T) {
	subs := splitQuery("Were Scott Derrickson and Ed Wood here?")
	if subs[0] != "Were Scott Derrickson and Ed Wood here?" {
		t.Errorf("first sub must be full query, got %q", subs[0])
	}
	joined := ""
	for _, s := range subs {
		joined += "|" + s
	}
	if !contains(joined, "Scott Derrickson") || !contains(joined, "Ed Wood") {
		t.Errorf("expected entity spans, got %v", subs)
	}
}

func FuzzSplitQuery(f *testing.F) {
	f.Add("Were Scott Derrickson and Ed Wood here?")
	f.Add("")
	f.Fuzz(func(t *testing.T, q string) {
		subs := splitQuery(q)
		if len(subs) == 0 || subs[0] != q {
			t.Fatalf("splitQuery must return full query first, got %v", subs)
		}
	})
}

// TestSplitQueryNonASCIIUppercase pins that entity extraction sees uppercase
// letters outside A-Z. A German sentence-initial noun, a Cyrillic proper noun,
// and a Greek one all carry no ASCII uppercase, so the previous 'A'..'Z' range
// test returned the full query alone and multi-hop recall silently degraded to
// a single recall.
func TestSplitQueryNonASCIIUppercase(t *testing.T) {
	cases := []struct {
		name  string
		query string
		want  string
	}{
		{"german noun", "Welche Migrationsstrategie gilt für Postgres?", "Migrationsstrategie"},
		{"cyrillic", "Кто основал 北京?", "北京"},
		{"greek", "Ποιος έγραψε τον Ωμηρικό;", "Ωμηρικό"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			joined := strings.Join(splitQuery(tc.query), "|")
			if !strings.Contains(joined, tc.want) {
				t.Errorf("entity span %q not extracted from %q: %v", tc.want, tc.query, splitQuery(tc.query))
			}
		})
	}
}
