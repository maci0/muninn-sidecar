// Package strhash hashes strings without allocating, for the request hot path
// where hash/fnv's digest object and its []byte(string) copy would both escape
// to the heap on every call.
package strhash

// FNV-1a 64-bit parameters, from hash/fnv. The output is byte-identical to
// fnv.New64a, so a value hashed here and a value hashed there compare equal.
const (
	offset64 = 14695981039346656037
	prime64  = 1099511628211
)

// FNV1a returns the FNV-1a 64-bit hash of s.
func FNV1a(s string) uint64 {
	h := uint64(offset64)
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= prime64
	}
	return h
}
