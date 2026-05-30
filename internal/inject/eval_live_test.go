package inject

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestParseLiveScenarios(t *testing.T) {
	in := []byte(`[{"name":"a","query":"q","seed":[{"concept":"c","content":"x"}],"expected_concepts":["c"]}]`)
	sc, err := ParseLiveScenarios(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(sc) != 1 || sc[0].Name != "a" || len(sc[0].Seed) != 1 || sc[0].ExpectedConcepts[0] != "c" {
		t.Fatalf("bad parse: %+v", sc)
	}
	if _, err := ParseLiveScenarios([]byte("not json")); err == nil {
		t.Error("expected error on bad json")
	}
}

func TestScoreLive(t *testing.T) {
	s := LiveScenario{
		Name:             "s",
		ExpectedConcepts: []string{"Auth Pattern", "DB Choice"},
	}
	injected := []memory{
		{Concept: "auth pattern", Content: "x"}, // matches (normalized)
		{Concept: "random noise", Content: "y"}, // extra
	}
	r := scoreLive(s, 5, injected)
	if r.Hits != 1 {
		t.Errorf("hits=%d want 1", r.Hits)
	}
	if r.Extra != 1 {
		t.Errorf("extra=%d want 1", r.Extra)
	}
	if r.Recalled != 5 {
		t.Errorf("recalled=%d want 5", r.Recalled)
	}
	if r.HitRate < 0.49 || r.HitRate > 0.51 { // 1 of 2 expected
		t.Errorf("hitrate=%.2f want 0.5", r.HitRate)
	}

	// No expected concepts → vacuous hit rate 1.0.
	if got := scoreLive(LiveScenario{Name: "e"}, 0, nil); got.HitRate != 1.0 {
		t.Errorf("empty expected hitrate=%.2f want 1.0", got.HitRate)
	}
}

func TestRunLive(t *testing.T) {
	var remembered int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var rpc struct {
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		json.Unmarshal(body, &rpc)
		switch rpc.Params.Name {
		case "muninn_remember", "muninn_remember_batch":
			remembered++
			w.Write([]byte(`{"jsonrpc":"2.0","result":{"id":"ok"},"id":1}`))
		case "muninn_recall":
			w.Write(fakeRecallResponse([]memory{
				{ID: "1", Concept: "auth pattern", Content: "use jwt", Score: 0.9, VectorScore: 0.9},
			}))
		default:
			w.Write([]byte(`{"jsonrpc":"2.0","result":{},"id":1}`))
		}
	}))
	defer srv.Close()

	cfg := Config{MCPURL: srv.URL, Vault: "live-test", Timeout: 2 * time.Second}
	scenarios := []LiveScenario{{
		Name:             "auth",
		Query:            "how does auth work",
		Seed:             []SeedMemory{{Concept: "auth pattern", Content: "use jwt"}},
		ExpectedConcepts: []string{"auth pattern"},
	}}
	results, err := RunLive(t.Context(), cfg, scenarios, 0)
	if err != nil {
		t.Fatal(err)
	}
	if remembered == 0 {
		t.Error("expected seed to call remember")
	}
	if len(results) != 1 || results[0].Hits != 1 || results[0].HitRate != 1.0 {
		t.Errorf("expected 1 hit / hitrate 1.0, got %+v", results)
	}
}

func FuzzParseLiveScenarios(f *testing.F) {
	f.Add([]byte(`[{"name":"a","query":"q","expected_concepts":["c"]}]`))
	f.Add([]byte(`[]`))
	f.Add([]byte(`garbage`))
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = ParseLiveScenarios(data)
	})
}
