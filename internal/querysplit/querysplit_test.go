package querysplit

import (
	"strings"
	"testing"
)

func TestSplit(t *testing.T) {
	subs := Split("Were Scott Derrickson and Ed Wood here?")
	if subs[0] != "Were Scott Derrickson and Ed Wood here?" {
		t.Errorf("first sub must be full query, got %q", subs[0])
	}
	joined := strings.Join(subs, "|")
	for _, want := range []string{"Scott Derrickson", "Ed Wood"} {
		if !strings.Contains(joined, want) {
			t.Errorf("expected entity span %q in %v", want, subs)
		}
	}
}

// TestSplitNonASCIIUppercase pins that entity extraction sees uppercase
// letters outside A-Z. A German sentence-initial noun, a Cyrillic proper noun,
// and a Greek one all carry no ASCII uppercase, so a previous 'A'..'Z' range
// test returned the full query alone and multi-hop recall silently degraded to
// a single recall.
func TestSplitNonASCIIUppercase(t *testing.T) {
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
			subs := Split(tc.query)
			if !strings.Contains(strings.Join(subs, "|"), tc.want) {
				t.Errorf("entity span %q not extracted from %q: %v", tc.want, tc.query, subs)
			}
		})
	}
}

// An entity span ends with the sentence terminator of its own script, not
// necessarily an ASCII one: a span closed by "。" is the same entity as the span
// closed by "?", and keeping the full-width terminator made two different recall
// keys out of one name.
func TestSplitTrimsCJTTrailingPunctuation(t *testing.T) {
	subs := Split("Did Scott Derrickson direct Sinister?。")
	if len(subs) != 3 || subs[1] != "Did Scott Derrickson" || subs[2] != "Sinister" {
		t.Errorf("Split = %q, want the entity spans with the 。 trimmed", subs)
	}
}

// The floor separating a name from a stray initial is a count of characters,
// and a byte count is the wrong unit in both directions: a two-character name
// of plain ASCII is two bytes and was dropped, while a one-character capital
// outside the Basic Multilingual Plane is four bytes and was admitted. Both
// answers are wrong; only the character count gets both right.
func TestSplitEntityFloorCountsCharactersNotBytes(t *testing.T) {
	// U+10400 DESERET CAPITAL LETTER LONG I: one character, four bytes, an
	// initial. The byte floor cleared it as an entity.
	subs := Split("\U00010400 and Ok and Alan")
	if len(subs) != 3 || subs[1] != "Ok" || subs[2] != "Alan" {
		t.Errorf("Split = %q, want the one-character capital dropped and both names kept", subs)
	}
	// A single Latin capital is an initial too, and a longer name is unaffected.
	subs = Split("J Smith met Alan Turing")
	if len(subs) != 3 || subs[1] != "J Smith" || subs[2] != "Alan Turing" {
		t.Errorf("Split = %q, want both multi-character entities kept", subs)
	}
}

// A sub-query is a recall key, and the concept index it is matched against is
// lowercased. Deduplicating on the span as written recalled one entity twice
// over a difference in case no one typed.
func TestSplitDedupesEntityCaseInsensitively(t *testing.T) {
	subs := Split("Paris and Paris")
	if len(subs) != 2 || subs[1] != "Paris" {
		t.Errorf("Split = %q, want the two spellings of one entity collapsed to one recall", subs)
	}
}

func FuzzSplit(f *testing.F) {
	f.Add("Were Scott Derrickson and Ed Wood here?")
	f.Add("")
	f.Fuzz(func(t *testing.T, q string) {
		subs := Split(q)
		if len(subs) == 0 || subs[0] != q {
			t.Fatalf("Split must return full query first, got %v", subs)
		}
	})
}
