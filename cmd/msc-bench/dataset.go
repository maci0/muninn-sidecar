// This file contains the benchmark's default labeled corpus and the shared
// vocabulary the dataset generators build on: the item/probe record types every
// generator in this package returns, the word banks, and the namespace check that
// rejects corpus sizes whose generator indices would collide.
package main

import (
	"fmt"
	"math/rand"
	"strings"
)

// item is a labeled memory: a unique concept key plus distinctive content.
type item struct {
	Concept string
	Content string
}

// probe is a labeled query: the gold concept it should retrieve (empty if the
// topic is absent, i.e. the query should be suppressed).
type probe struct {
	Query   string
	Gold    string // gold concept; "" => absent (should suppress)
	Answer  string // gold answer span (for QA dump); optional
	Present bool
}

var (
	adjs      = []string{"amber", "cobalt", "crimson", "emerald", "golden", "ivory", "jade", "obsidian", "scarlet", "silver", "teal", "violet", "azure", "bronze", "copper", "indigo", "maroon", "onyx", "pearl", "ruby"}
	creatures = []string{"otter", "falcon", "lynx", "heron", "marmot", "gecko", "badger", "raven", "ferret", "ibis", "newt", "stoat", "tapir", "vole", "wren", "yak", "quail", "shrew", "civet", "dingo"}
	places    = []string{"Velmoor", "Drassil", "Khyber Reach", "Pellwitch", "Greyfen", "Lowmarsh", "Thornvale", "Quillhaven", "Brackwater", "Misthollow", "Caldspire", "Ostgard", "Fenwick Hollow", "Sablecliff", "Dunmere", "Wraithmoor", "Calloway Flats", "Embergrove", "Hollowmere", "Stagholt"}
	traits    = []string{"bioluminescent at dusk", "able to mimic birdsong", "immune to the local frostblight", "known to hoard polished stones", "active only during the spring thaw", "capable of swimming upstream for miles", "famous for its nine-note call", "the last of its migratory line"}
	landmarks = []string{"the Old Salt Bridge", "Cinder Lake", "the Tannery Steps", "Marrow Ridge", "the Glasswind Pass", "Harrow Mill", "the Sunken Orchard", "Pikeman's Wharf"}
)

// checkNamespace rejects corpus sizes whose generator indices leave the modular
// namespace of `space` distinct subjects: the n seeded items (indices 0..n-1)
// must fit without colliding with each other, and the nAbsent absent probes
// (indices n+1, n+1+stride, ...) must not wrap around onto seeded items. The
// error names the actual overflow cause.
func checkNamespace(n, nAbsent, stride, space int, kind string) error {
	if n > space {
		return fmt.Errorf("n=%d exceeds the %d-%s namespace: seeded items would collide with each other", n, space, kind)
	}
	if nAbsent > 0 && n+1+(nAbsent-1)*stride >= space {
		return fmt.Errorf("n=%d with %d absent probes exceeds the %d-%s namespace: absent probes would wrap onto seeded items", n, nAbsent, space, kind)
	}
	return nil
}

// genDataset builds n unique memories from rare adjective+creature+place triples
// and matched present/absent probes. Probes are worded differently from the
// stored content so retrieval tests semantics, not lexical overlap.
func genDataset(rngSeed int64, n, nPresent, nAbsent int) ([]item, []probe, []probe, error) {
	rng := rand.New(rand.NewSource(rngSeed))
	triple := func(i int) (string, string, string) {
		a := adjs[i%len(adjs)]
		c := creatures[(i/len(adjs))%len(creatures)]
		p := places[(i/(len(adjs)*len(creatures)))%len(places)]
		return a, c, p
	}
	if err := checkNamespace(n, nAbsent, 3, len(adjs)*len(creatures)*len(places), "triple"); err != nil {
		return nil, nil, nil, err
	}
	concept := func(a, c, p string) string {
		return strings.ToLower(fmt.Sprintf("%s-%s-%s", a, c, strings.ReplaceAll(p, " ", "")))
	}

	items := make([]item, 0, n)
	for i := 0; i < n; i++ {
		a, c, p := triple(i)
		trait := traits[rng.Intn(len(traits))]
		lm := landmarks[rng.Intn(len(landmarks))]
		count := 12 + rng.Intn(880)
		items = append(items, item{
			Concept: concept(a, c, p),
			Content: fmt.Sprintf("Field note: the %s %s of %s is %s. A census near %s counted roughly %d individuals.",
				a, c, p, trait, lm, count),
		})
	}

	// Present probes: a paraphrased question about a seeded triple.
	present := make([]probe, 0, nPresent)
	for i := 0; i < nPresent && i < n; i++ {
		idx := (i * 7) % n // spread across the corpus
		a, c, p := triple(idx)
		present = append(present, probe{
			Query:   fmt.Sprintf("What have naturalists recorded about the %s %s that lives around %s?", a, c, p),
			Gold:    concept(a, c, p),
			Present: true,
		})
	}

	// Absent probes: triples beyond the seeded range (guaranteed not stored).
	absent := make([]probe, 0, nAbsent)
	for i := 0; i < nAbsent; i++ {
		idx := n + 1 + i*3 // past the seeded indices
		a, c, p := triple(idx)
		absent = append(absent, probe{
			Query:   fmt.Sprintf("What have naturalists recorded about the %s %s that lives around %s?", a, c, p),
			Gold:    "",
			Present: false,
		})
	}
	return items, present, absent, nil
}

// coinNameSpace is the number of distinct names coinName can produce (16 first
// parts x 16 middles x 16 endings); indices beyond it wrap around.
const coinNameSpace = 16 * 16 * 16

// coinName builds a deterministic distinctive pseudo-word from an index, so each
// diverse memory has a unique, embedding-separable subject token.
func coinName(i int) string {
	a := []string{"Zeph", "Korb", "Vael", "Drix", "Mor", "Quil", "Tav", "Bryn", "Sol", "Hesp", "Nyx", "Orin", "Pell", "Rask", "Vund", "Wex"}
	b := []string{"yr", "al", "ix", "on", "eth", "ar", "is", "um", " os", "en", "ic", "ad", "or", "us", "el", "yn"}
	c := []string{"ia", "os", "ane", "ex", "ium", "ara", "oid", "yx", "een", "ova", "ull", "ade", "ish", "orn", "wick", "holt"}
	return a[i%len(a)] + b[(i/len(a))%len(b)] + c[(i/(len(a)*len(b)))%len(c)]
}

// genDiverse builds memories spread across distinct domains with unique
// vocabulary, so embeddings separate them well — a realistic memory store rather
// than the near-identical homogeneous corpus. Each memory has a unique coined
// subject; probes paraphrase a fact about that subject.
func genDiverse(rngSeed int64, n, nPresent, nAbsent int) ([]item, []probe, []probe, error) {
	if err := checkNamespace(n, nAbsent, 3, coinNameSpace, "name"); err != nil {
		return nil, nil, nil, err
	}
	mem := func(i int) item {
		x := coinName(i)
		switch i % 8 {
		case 0:
			return item{"tool-" + x, fmt.Sprintf("The %s build tool caches compiled artifacts under ~/.%s/cache and invalidates them on lockfile changes.", x, strings.ToLower(x))}
		case 1:
			return item{"person-" + x, fmt.Sprintf("%s, the lead engineer on the Halcyon project, insists that every pull request include a rollback plan.", x)}
		case 2:
			return item{"lang-" + x, fmt.Sprintf("In the %s programming language, tail calls are optimized away by the compiler into loops, so deep recursion never overflows the stack.", x)}
		case 3:
			return item{"dish-" + x, fmt.Sprintf("%s stew, a specialty of the coastal town of Marrowport, is traditionally thickened with roasted chestnut flour.", x)}
		case 4:
			return item{"moon-" + x, fmt.Sprintf("The moon %s, orbiting the gas giant Theron, is notable for its methane geysers that erupt on a seven-hour cycle.", x)}
		case 5:
			return item{"proto-" + x, fmt.Sprintf("The %s protocol authenticates clients with rotating ed25519 keys exchanged over a short-lived QUIC channel.", x)}
		case 6:
			return item{"drug-" + x, fmt.Sprintf("The compound %s treats chronic vestibular migraine by blocking the CGRP receptor in the trigeminal pathway.", x)}
		default:
			return item{"treaty-" + x, fmt.Sprintf("The Treaty of %s, signed in 1847, ended the decade-long Saltmarsh border war between the river provinces.", x)}
		}
	}
	probeFor := func(i int) probe {
		x := coinName(i)
		c := mem(i).Concept
		var q string
		switch i % 8 {
		case 0:
			q = fmt.Sprintf("Where does the %s build tool keep its compiled output?", x)
		case 1:
			q = fmt.Sprintf("What does %s require on every pull request?", x)
		case 2:
			q = fmt.Sprintf("How does the %s language avoid stack overflow on deep recursion?", x)
		case 3:
			q = fmt.Sprintf("Which ingredient thickens %s stew?", x)
		case 4:
			q = fmt.Sprintf("What is unusual about the moon %s?", x)
		case 5:
			q = fmt.Sprintf("How does the %s protocol authenticate clients?", x)
		case 6:
			q = fmt.Sprintf("What condition does the compound %s treat?", x)
		default:
			q = fmt.Sprintf("What conflict did the Treaty of %s end?", x)
		}
		return probe{Query: q, Gold: c, Present: true}
	}

	items := make([]item, 0, n)
	for i := 0; i < n; i++ {
		items = append(items, mem(i))
	}
	present := make([]probe, 0, nPresent)
	for i := 0; i < nPresent && i < n; i++ {
		present = append(present, probeFor((i*7)%n))
	}
	absent := make([]probe, 0, nAbsent)
	for i := 0; i < nAbsent; i++ {
		absent = append(absent, probeFor(n+1+i*3)) // coined subjects past the seeded range
	}
	for i := range absent {
		absent[i].Gold = ""
		absent[i].Present = false
	}
	return items, present, absent, nil
}
