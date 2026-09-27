// Package querysplit decomposes a question into the sub-queries used for
// multi-recall: the full question plus each capitalized entity span, a no-LLM
// proxy for the "hops" a multi-hop question references.
package querysplit

import (
	"strings"
	"unicode"
)

// minEntityBytes is the shortest entity span worth a second recall: under
// three bytes of joined words is a stray initial or a stop-word fragment, not
// a name.
const minEntityBytes = 3

// trailingPunct is the set of trailing punctuation trimmed off an entity span.
// Beyond ASCII "?.,", the terminators a CJK sentence actually ends with: a
// question written in Japanese or Chinese ends in "。", and keeping it made the
// sub-query ("北京大学。") a different recall key from the same entity written
// without it ("北京大学"), for a difference no one typed.
const trailingPunct = "?.,。、！？；：…!;:"

// Split returns the sub-queries for q: q first, then each maximal run of
// capitalized words, deduped and trimmed of trailing punctuation. Runs shorter
// than minEntityBytes are dropped.
func Split(q string) []string {
	subs := []string{q}
	seen := map[string]bool{q: true}
	var cur []string
	flush := func() {
		if len(cur) == 0 {
			return
		}
		s := strings.Trim(strings.Join(cur, " "), trailingPunct)
		if len(s) >= minEntityBytes && !seen[s] {
			seen[s] = true
			subs = append(subs, s)
		}
		cur = nil
	}
	for _, w := range strings.Fields(q) {
		// unicode.IsUpper, not an 'A'..'Z' range test: a German noun ("Migrations-
		// strategie"), a Turkish or Cyrillic proper noun, and any CJK entity carry
		// no ASCII uppercase at all, so the range test silently extracted no
		// sub-queries for those questions and multi-hop recall collapsed to a
		// single recall of the whole question.
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
