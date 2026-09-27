package redact

import (
	"strings"
	"testing"
)

// benchMessage is a representative 4 KiB assistant turn: the size the store
// worker redacts per captured exchange (apiformat.TruncateText caps at 4000).
var benchMessage = func() string {
	base := "we migrated the ingestion pipeline from the legacy batch job to the streaming " +
		"consumer, the retry budget is three attempts with exponential backoff, and the " +
		"dead letter queue lives in the ops vault under prod-eu. the oncall rotation " +
		"changed last quarter and the runbook now documents the rollback procedure for " +
		"each of the downstream consumers that depend on the topic layout. "
	return strings.Repeat(base, 4000/len(base)+1)[:4000]
}()

// BenchmarkSecretsMessage measures redaction of a full captured assistant turn.
func BenchmarkSecretsMessage(b *testing.B) {
	b.SetBytes(int64(len(benchMessage)))
	b.ReportAllocs()
	for b.Loop() {
		Secrets(benchMessage)
	}
}

// BenchmarkSecretsQuery measures redaction of a recall query (up to 2000 runes).
func BenchmarkSecretsQuery(b *testing.B) {
	q := benchMessage[:2000]
	b.SetBytes(int64(len(q)))
	b.ReportAllocs()
	for b.Loop() {
		Secrets(q)
	}
}
