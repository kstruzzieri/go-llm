package fingerprint

import (
	"reflect"
	"testing"
	"time"
)

// TestProbeRecipes_FixedVersionsAndBudgets spells every value out rather than
// reusing the table's constants: a change here is a contract change that needs
// a method-version bump.
func TestProbeRecipes_FixedVersionsAndBudgets(t *testing.T) {
	const mib = 1048576
	want := []ProbeRecipe{
		{
			Method: "ollama-show", Adapter: "ollama", Version: 1,
			Capabilities:  []string{"chat", "generate", "insert", "embed", "stream", "tool_call", "thinking"},
			UsualRequests: 1, MaxRequests: 1, MaxOutputTokensPerRequest: 0,
			RequestTimeout: 30 * time.Second, MaxResponseBytes: mib,
		},
		{
			Method: "chat-stream", Adapter: "openai-compat", Version: 1,
			Capabilities:  []string{"chat", "stream"},
			UsualRequests: 1, MaxRequests: 1, MaxOutputTokensPerRequest: 1,
			RequestTimeout: 3 * time.Minute, MaxResponseBytes: mib,
			Message: "Call get_time.",
		},
		{
			Method: "chat-stream-tools", Adapter: "openai-compat", Version: 1,
			Capabilities:  []string{"chat", "stream", "tool_call"},
			UsualRequests: 1, MaxRequests: 2, MaxOutputTokensPerRequest: 2048,
			RequestTimeout: 3 * time.Minute, MaxResponseBytes: mib,
			Message:                 "Call get_time.",
			ToolName:                "get_time",
			ToolDescription:         "Get the current time.",
			ToolParameters:          `{"type":"object","properties":{}}`,
			RequiredToolChoiceFirst: true,
		},
	}
	if len(probeRecipes) != len(want) {
		t.Fatalf("table has %d recipes, want %d", len(probeRecipes), len(want))
	}
	for _, w := range want {
		t.Run(w.Method, func(t *testing.T) {
			got, ok := RecipeFor(w.Method)
			if !ok {
				t.Fatalf("RecipeFor(%q) not found", w.Method)
			}
			if !reflect.DeepEqual(got, w) {
				t.Fatalf("RecipeFor(%q) =\n %+v\nwant\n %+v", w.Method, got, w)
			}
			if v := MethodVersion(w.Method); v != w.Version {
				t.Errorf("MethodVersion(%q) = %d, want %d", w.Method, v, w.Version)
			}

			// The returned value is a copy, including its slice.
			got.Capabilities[0] = "mutated"
			got.Version = 99
			again, _ := RecipeFor(w.Method)
			if !reflect.DeepEqual(again, w) {
				t.Fatalf("mutating a returned recipe changed the table: %+v", again)
			}
		})
	}
}

func TestProbeRecipes_UnknownMethods(t *testing.T) {
	// chat-tools is the non-streamed request shape, never a recipe: an answer
	// through it does not say a streamed tool call works.
	for _, method := range []string{"chat-tools", "nope", ""} {
		if r, ok := RecipeFor(method); ok || !reflect.DeepEqual(r, ProbeRecipe{}) {
			t.Errorf("RecipeFor(%q) = %+v, %v, want the zero recipe and false", method, r, ok)
		}
		if v := MethodVersion(method); v != 0 {
			t.Errorf("MethodVersion(%q) = %d, want 0", method, v)
		}
	}
}
