package main

import (
	"os"
	"path/filepath"
	"testing"
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
	if envOr("MUNINN_MCP_URL", "d") != "http://x/mcp" || envOr("NOPE_VAR", "d") != "d" {
		t.Error("envOr")
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
			g.items, g.present, g.absent = genDataset(1, 30, 10, 5)
			return
		}(),
		func() (g struct {
			name            string
			items           []item
			present, absent []probe
		}) {
			g.name = "diverse"
			g.items, g.present, g.absent = genDiverse(1, 30, 10, 5)
			return
		}(),
		func() (g struct {
			name            string
			items           []item
			present, absent []probe
		}) {
			g.name = "agentmem"
			g.items, g.present, g.absent = genAgentMem(40, 10)
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
	_, amp, _ := genAgentMem(8, 0)
	for _, p := range amp {
		if p.Answer == "" {
			t.Errorf("agentmem present probe missing answer: %+v", p)
		}
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
	if _, _, _, err := genSquad(filepath.Join(dir, "nope.json"), 1, 1, 1, 1, "paragraph"); err == nil {
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
	if err := writeQA(path, probes); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	s := string(data)
	if !contains(s, "q1") || !contains(s, "a1") || contains(s, "q2") || contains(s, "q3") {
		t.Errorf("writeQA content: %s", s)
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
		_, _, _ = genAgentMem(n, absent)
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
