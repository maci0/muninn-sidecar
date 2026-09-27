package mcpclient

import (
	"encoding/json"
	"fmt"
)

// RecallMemories decodes a `muninn_recall` reply into T, the caller's own
// memory record. Every MuninnDB caller in the tree (the injector's recall path
// and the two eval harnesses) reads the same envelope, so the decoding lives
// here next to ContentTexts rather than in a copy per caller.
//
// A recall payload is JSON text inside a content block. Three shapes are
// accepted, in this order per block: `{"memories": [...]}`, `{"results": [...]}`
// (the key some builds use), and a bare array. The first block that yields a
// non-empty list wins. A block that does not parse, or that parses to neither
// list, is skipped rather than fatal: a server may prepend a human-readable
// summary block, and one unreadable block must not abandon the memories in the
// next one.
//
// Every text block being unparsable is a corrupt reply and returns the first
// error; a reply that decodes cleanly but carries no memories is a genuine
// empty result and returns nil.
func RecallMemories[T any](body []byte) ([]T, error) {
	blocks, err := ContentTexts(body)
	if err != nil {
		return nil, fmt.Errorf("parse JSON-RPC response: %w", err)
	}

	var firstErr error
	for _, text := range blocks {
		var envelope struct {
			Memories []T `json:"memories"`
			Results  []T `json:"results"`
		}
		if err := json.Unmarshal([]byte(text), &envelope); err != nil {
			var direct []T
			if err2 := json.Unmarshal([]byte(text), &direct); err2 != nil {
				if firstErr == nil {
					firstErr = fmt.Errorf("parse recall result (struct: %v; array: %w)", err, err2)
				}
				continue
			}
			if len(direct) > 0 {
				return direct, nil
			}
			continue
		}
		if len(envelope.Memories) > 0 {
			return envelope.Memories, nil
		}
		if len(envelope.Results) > 0 {
			return envelope.Results, nil
		}
	}

	if firstErr != nil {
		return nil, firstErr
	}
	return nil, nil
}
