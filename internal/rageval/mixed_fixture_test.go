package rageval

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
)

// The real search verifies the final row before rendering its fixed ID/date.
func TestMixedEvalMemorySearchFinalBody(t *testing.T) {
	var first []agent.Message
	for run := 0; run < 2; run++ {
		got, err := mixedEvalMemorySearch(context.Background(), "fixture-call", "fixture", []mixedEvalMemoryRecord{
			{id: "fixture-final", content: "fixture final body"},
		})
		if err != nil {
			t.Fatalf("run %d: verified search: %v", run, err)
		}
		if len(got) != 2 || !strings.Contains(got[1].Content, "fixture-final · semantic · 2025-07-27 · fixture final body") {
			t.Fatalf("run %d: final ID/date not rendered: %+v", run, got)
		}
		if run == 0 {
			first = got
			continue
		}
		if !reflect.DeepEqual(got, first) {
			t.Fatal("repeated seeding changed the production search chain")
		}
	}
}
