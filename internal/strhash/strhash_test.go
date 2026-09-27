package strhash

import (
	"hash/fnv"
	"testing"
)

// FNV1a must agree with hash/fnv byte for byte, so a hash written before the
// switch to this package still matches one written after it.
func TestFNV1aMatchesStdlib(t *testing.T) {
	for _, s := range []string{
		"",
		"a",
		"the quick brown fox",
		"User:\nsummarize this diff\n\nAssistant:\ndone",
		"héllo wörld ☃",
		string(make([]byte, 4096)),
	} {
		h := fnv.New64a()
		h.Write([]byte(s))
		if got, want := FNV1a(s), h.Sum64(); got != want {
			t.Errorf("FNV1a(%d bytes) = %d, hash/fnv = %d", len(s), got, want)
		}
	}
}
