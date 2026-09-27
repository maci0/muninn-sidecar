// Package querysplit decomposes a question into the sub-queries used for
// multi-recall: the full question plus each capitalized entity span, a no-LLM
// proxy for the "hops" a multi-hop question references. The signal is case, so
// it only fires in scripts that have case; see the note in Split.
package querysplit

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// minEntityRunes is the shortest entity span worth a second recall, counted in
// characters. One character is a stray initial, two or more is a name, in every
// script. The floor used to be counted in bytes, which is a different unit than
// the one the words it was filtering are made of: a single CJK character is
// three bytes and so cleared a floor written to reject stray ASCII initials,
// while the two-character entity the floor is meant to admit and reject
// correctly was measured on the wrong axis. A byte floor is also 64× more
// permissive for Cyrillic, Greek and Devanagari than for Latin.
const minEntityRunes = 2

// trailingPunct is the set of trailing punctuation trimmed off an entity span.
// Beyond ASCII "?.,", the terminators a CJK sentence actually ends with: a
// question written in Japanese or Chinese ends in "。", and keeping it made the
// sub-query ("北京大学。") a different recall key from the same entity written
// without it ("北京大学"), for a difference no one typed.
const trailingPunct = "?.,。、！？；：…!;:"

// Split returns the sub-queries for q: q first, then each maximal run of
// capitalized words, deduped and trimmed of trailing punctuation. Runs shorter
// than minEntityRunes are dropped.
//
// The dedup set is keyed on the lowercased span, because a sub-query is a
// recall key and the concept index it is matched against is lowercased too.
// Keying it on the span as written recalled "Paris" and "paris" as two
// sub-queries of one question, doubling the recall for one entity.
func Split(q string) []string {
	subs := []string{q}
	seen := map[string]bool{strings.ToLower(q): true}
	var cur []string
	flush := func() {
		if len(cur) == 0 {
			return
		}
		s := strings.Trim(strings.Join(cur, " "), trailingPunct)
		key := strings.ToLower(s)
		if utf8.RuneCountInString(s) >= minEntityRunes && !seen[key] {
			seen[key] = true
			subs = append(subs, s)
		}
		cur = nil
	}
	for _, w := range strings.Fields(q) {
		// unicode.IsUpper, not an 'A'..'Z' range test: a German noun ("Migrations-
		// strategie"), a Turkish proper noun and a Cyrillic or Greek one carry no
		// ASCII uppercase at all, so the range test silently extracted no
		// sub-queries for those questions and multi-hop recall collapsed to a
		// single recall of the whole question.
		//
		// This does not help Chinese or Japanese, which have no case to test: a
		// Han span is not upper, so those questions still yield the whole question
		// and no entity spans. Recognizing them needs a signal other than case
		// (a name dictionary, or fixed-length n-grams), which is a different
		// design and not a change to make here.
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
