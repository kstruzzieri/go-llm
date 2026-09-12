package recipe_test

import (
	_ "embed"
	"reflect"
	"testing"

	"github.com/kstruzzieri/go-llm/recipe"
)

//go:embed testdata/review-change.recipe.json
var embeddedReviewChange []byte

func TestParseEmbeddedRecipe(t *testing.T) {
	defaultValue := "correctness"
	want := recipe.Recipe{
		Version:     1,
		Name:        "review-change",
		Description: "Review a change for actionable defects.",
		Goal:        "Review {{inputs.target}}. Focus on {{inputs.focus}}.",
		Context:     "Report defects with file references and explain their impact.",
		Inputs: []recipe.Input{
			{Name: "target", Description: "Change or path to review"},
			{Name: "focus", Description: "Review emphasis", Default: &defaultValue},
		},
		ModelHint: &recipe.ModelHint{UseCase: "code-review"},
	}

	got, err := recipe.Parse(embeddedReviewChange)
	if err != nil {
		t.Fatalf("Parse(embedded review-change recipe) error = %v, want nil", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Parse(embedded review-change recipe) = %#v, want %#v", got, want)
	}
}
