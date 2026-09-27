package mcpclient

import (
	"encoding/json"
	"reflect"
	"testing"
)

type recallRecord struct {
	ID      string  `json:"id"`
	Concept string  `json:"concept"`
	Score   float64 `json:"score"`
}

// recallBody wraps the given text blocks in a JSON-RPC result envelope.
func recallBody(t *testing.T, texts ...string) []byte {
	t.Helper()
	blocks := make([]map[string]string, len(texts))
	for i, text := range texts {
		blocks[i] = map[string]string{"type": "text", "text": text}
	}
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"result":  map[string]any{"content": blocks},
		"id":      1,
	})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// TestRecallMemoriesFormats pins every payload shape the shared decoder accepts
// and the caller's own record type reaching it, since three packages decode
// through this one function.
func TestRecallMemoriesFormats(t *testing.T) {
	cases := []struct {
		name  string
		inner string
		want  []recallRecord
		err   bool
	}{
		{
			name:  "memories field",
			inner: `{"memories":[{"id":"m1","concept":"c","score":0.5}]}`,
			want:  []recallRecord{{ID: "m1", Concept: "c", Score: 0.5}},
		},
		{
			name:  "results field",
			inner: `{"results":[{"id":"r1","concept":"c","score":0.9}]}`,
			want:  []recallRecord{{ID: "r1", Concept: "c", Score: 0.9}},
		},
		{
			name:  "memories wins over results",
			inner: `{"memories":[{"id":"m1"}],"results":[{"id":"r1"}]}`,
			want:  []recallRecord{{ID: "m1"}},
		},
		{
			name:  "direct array",
			inner: `[{"id":"d1","concept":"c","score":0.4}]`,
			want:  []recallRecord{{ID: "d1", Concept: "c", Score: 0.4}},
		},
		{
			name:  "empty object",
			inner: `{}`,
			want:  nil,
		},
		{
			name:  "empty memories array",
			inner: `{"memories":[]}`,
			want:  nil,
		},
		{
			name:  "malformed payload",
			inner: `{not json`,
			err:   true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := RecallMemories[recallRecord](recallBody(t, tc.inner))
			if tc.err {
				if err == nil {
					t.Fatalf("expected a parse error, got %d records", len(got))
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("RecallMemories = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestRecallMemoriesSkipsSummaryBlock pins the case a per-caller copy of this
// decoder got wrong: a server that prepends a human-readable block must not
// cost the caller the memories in the block after it.
func TestRecallMemoriesSkipsSummaryBlock(t *testing.T) {
	summary := "Recalled 3 memories from vault msc-qa."
	garbage := "not json at all"
	payload := `{"memories":[{"id":"m1","concept":"c","score":0.7}]}`

	got, err := RecallMemories[recallRecord](recallBody(t, summary, garbage, payload))
	if err != nil {
		t.Fatal(err)
	}
	want := []recallRecord{{ID: "m1", Concept: "c", Score: 0.7}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("RecallMemories = %+v, want %+v", got, want)
	}
}

// TestRecallMemoriesAllBlocksUnparsable separates a corrupt reply (an error the
// caller can report) from a reply that decodes and carries nothing.
func TestRecallMemoriesAllBlocksUnparsable(t *testing.T) {
	if _, err := RecallMemories[recallRecord](recallBody(t, "{not json")); err == nil {
		t.Fatal("expected an error when no block parses")
	}
	if _, err := RecallMemories[recallRecord](recallBody(t, `{"memories":[]}`)); err != nil {
		t.Fatalf("an empty result is not an error: %v", err)
	}
}
