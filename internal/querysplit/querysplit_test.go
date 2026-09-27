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
