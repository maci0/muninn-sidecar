package main

import "testing"

func approxf(a, b float64) bool {
	d := a - b
	return d < 0.001 && d > -0.001
}

func TestExactMatch(t *testing.T) {
	if exactMatch("The France", []string{"france"}) != 1 {
		t.Error("article/case-insensitive EM should match")
	}
	if exactMatch("Germany", []string{"France", "french republic"}) != 0 {
		t.Error("non-match should be 0")
	}
}

func TestTokenF1(t *testing.T) {
	// pred "the quick fox", gold "quick brown fox": common {quick,fox}=2;
	// prec 2/2 (after article strip) =1.0? pred tokens after norm: quick fox (2)
	// gold: quick brown fox (3). common 2 -> prec 1.0, rec 0.667 -> F1 0.8
	if got := tokenF1("the quick fox", []string{"quick brown fox"}); !approxf(got, 0.8) {
		t.Errorf("tokenF1=%.3f want 0.8", got)
	}
	if got := tokenF1("paris", []string{"paris"}); !approxf(got, 1) {
		t.Errorf("identical F1 should be 1, got %.3f", got)
	}
	if got := tokenF1("xyz", []string{"abc"}); got != 0 {
		t.Errorf("disjoint F1 should be 0, got %.3f", got)
	}
}

func TestContainsAnswer(t *testing.T) {
	if !containsAnswer("The capital is Paris, France.", []string{"Paris"}) {
		t.Error("should detect answer substring")
	}
	if containsAnswer("nothing relevant here", []string{"Paris"}) {
		t.Error("should not falsely detect")
	}
}

func FuzzScore(f *testing.F) {
	f.Add("the Paris", "Paris")
	f.Fuzz(func(t *testing.T, pred, gold string) {
		golds := []string{gold}
		em := exactMatch(pred, golds)
		f1 := tokenF1(pred, golds)
		_ = containsAnswer(pred, golds)
		_ = normalizeAnswer(pred)
		if em < 0 || em > 1 || f1 < 0 || f1 > 1.0001 {
			t.Fatalf("score out of range em=%v f1=%v", em, f1)
		}
	})
}
