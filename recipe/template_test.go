package recipe

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestExpand(t *testing.T) {
	r := Recipe{Version: 1, Name: "review", Description: "Review", Goal: "G={{inputs.x}}", Context: "C={{inputs.x}}", Inputs: []Input{{Name: "x"}}}
	goal, expandedContext, err := Expand(r, map[string]string{"x": "v"}, 100)
	if err != nil || goal != "G=v" || expandedContext != "C=v" {
		t.Fatalf("Expand(recipe, x=v, 100) = %q, %q, %v, want %q, %q, nil", goal, expandedContext, err, "G=v", "C=v")
	}
}

func TestExpandBackslashParity(t *testing.T) {
	tests := []struct {
		name     string
		template string
		want     string
	}{
		{name: "zero", template: `{{inputs.x}}`, want: `VALUE`},
		{name: "one", template: `\{{inputs.x}}`, want: `{{inputs.x}}`},
		{name: "two", template: `\\{{inputs.x}}`, want: `\VALUE`},
		{name: "three", template: `\\\{{inputs.x}}`, want: `\{{inputs.x}}`},
		{name: "four", template: `\\\\{{inputs.x}}`, want: `\\VALUE`},
		{name: "five", template: `\\\\\{{inputs.x}}`, want: `\\{{inputs.x}}`},
		{name: "quoted malformed then active", template: `\{{inputs.bad {{inputs.x}}`, want: `{{inputs.bad VALUE`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := templateRecipe(tt.template, "", Input{Name: "x"})
			goal, expandedContext, err := Expand(r, map[string]string{"x": "VALUE"}, 100)
			if err != nil || goal != tt.want || expandedContext != "" {
				t.Errorf("Expand(%q, x=VALUE, 100) = %q, %q, %v, want %q, empty, nil", tt.template, goal, expandedContext, err, tt.want)
			}
		})
	}
}

func TestExpandLeavesOtherBraceFormsLiteral(t *testing.T) {
	template := `{{ inputs.x }} {{Inputs.x}} \else`
	r := templateRecipe(template, "", Input{Name: "x"})
	goal, expandedContext, err := Expand(r, map[string]string{"x": "VALUE"}, 100)
	if err != nil || goal != template || expandedContext != "" {
		t.Errorf("Expand(%q, x=VALUE, 100) = %q, %q, %v, want unchanged, empty, nil", template, goal, expandedContext, err)
	}
}

func TestExpandDoesNotRescanValuesOrDefaults(t *testing.T) {
	t.Run("supplied value", func(t *testing.T) {
		r := templateRecipe(`{{inputs.x}}|{{inputs.x}}`, "", Input{Name: "x"}, Input{Name: "y"})
		goal, expandedContext, err := Expand(r, map[string]string{"x": "{{inputs.y}}", "y": "SECRET"}, 100)
		if err != nil || goal != "{{inputs.y}}|{{inputs.y}}" || expandedContext != "" {
			t.Errorf("Expand(repeated x, placeholder value, 100) = %q, %q, %v, want %q, empty, nil", goal, expandedContext, err, "{{inputs.y}}|{{inputs.y}}")
		}
	})

	t.Run("default and whitespace", func(t *testing.T) {
		defaultValue := `{{inputs.y}}; $(whoami)`
		r := templateRecipe(" \t{{inputs.x}}\n", "\n{{inputs.x}} ", Input{Name: "x", Default: &defaultValue}, Input{Name: "y", Default: templateStringPointer("SECRET")})
		goal, expandedContext, err := Expand(r, nil, 100)
		if err != nil || goal != " \t{{inputs.y}}; $(whoami)\n" || expandedContext != "\n{{inputs.y}}; $(whoami) " {
			t.Errorf("Expand(whitespace/default recipe, nil, 100) = %q, %q, %v, want literal default with preserved whitespace", goal, expandedContext, err)
		}
	})
}

func TestValidateTemplatesAndExpandRejectReservedSyntax(t *testing.T) {
	templates := []struct {
		name     string
		template string
	}{
		{name: "missing name and close", template: `{{inputs.`},
		{name: "empty name", template: `{{inputs.}}`},
		{name: "uppercase name", template: `{{inputs.X}}`},
		{name: "hyphenated name", template: `{{inputs.x-y}}`},
		{name: "space after name", template: `{{inputs.x }}`},
		{name: "one closing brace", template: `{{inputs.x}`},
		{name: "undeclared reference", template: `{{inputs.y}}`},
	}
	for _, field := range []string{"goal", "context"} {
		for _, tt := range templates {
			t.Run(field+"/"+tt.name, func(t *testing.T) {
				goalTemplate, contextTemplate := "valid", ""
				if field == "goal" {
					goalTemplate = tt.template
				} else {
					contextTemplate = tt.template
				}
				r := templateRecipe(goalTemplate, contextTemplate, Input{Name: "x"})
				if err := ValidateTemplates(r); err == nil || !strings.Contains(err.Error(), field) {
					t.Errorf("ValidateTemplates(%s=%q) error = %v, want diagnostic naming %s", field, tt.template, err, field)
				}
				goal, expandedContext, err := Expand(r, map[string]string{"x": "VALUE"}, 100)
				if err == nil || !strings.Contains(err.Error(), field) {
					t.Errorf("Expand(%s=%q, x=VALUE, 100) error = %v, want diagnostic naming %s", field, tt.template, err, field)
				}
				if goal != "" || expandedContext != "" {
					t.Errorf("Expand(%s=%q, x=VALUE, 100) = %q, %q on error, want two empty strings", field, tt.template, goal, expandedContext)
				}
			})
		}
	}
}

func TestValidateTemplatesDoesNotRequireValues(t *testing.T) {
	r := templateRecipe(`{{inputs.x}}`, `{{inputs.x}}`, Input{Name: "x"})
	if err := ValidateTemplates(r); err != nil {
		t.Errorf("ValidateTemplates(required referenced input) error = %v, want nil", err)
	}
}

func TestExpandBindings(t *testing.T) {
	tests := []struct {
		name     string
		input    Input
		values   map[string]string
		goal     string
		wantGoal string
		wantErr  string
	}{
		{name: "nil default is required", input: Input{Name: "x"}, goal: `{{inputs.x}}`, wantErr: `recipe: missing required input "x"`},
		{name: "empty default is present", input: Input{Name: "x", Default: templateStringPointer("")}, goal: `before{{inputs.x}}after`, wantGoal: "beforeafter"},
		{name: "explicit empty is present", input: Input{Name: "x"}, values: map[string]string{"x": ""}, goal: `before{{inputs.x}}after`, wantGoal: "beforeafter"},
		{name: "supplied value overrides default", input: Input{Name: "x", Default: templateStringPointer("default")}, values: map[string]string{"x": "supplied"}, goal: `{{inputs.x}}`, wantGoal: "supplied"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := templateRecipe(tt.goal, "", tt.input)
			goal, expandedContext, err := Expand(r, tt.values, 100)
			if tt.wantErr != "" {
				if err == nil || err.Error() != tt.wantErr {
					t.Errorf("Expand(%s, values, 100) error = %v, want %q", tt.name, err, tt.wantErr)
				}
				if goal != "" || expandedContext != "" {
					t.Errorf("Expand(%s, values, 100) = %q, %q on error, want two empty strings", tt.name, goal, expandedContext)
				}
				return
			}
			if err != nil || goal != tt.wantGoal || expandedContext != "" {
				t.Errorf("Expand(%s, values, 100) = %q, %q, %v, want %q, empty, nil", tt.name, goal, expandedContext, err, tt.wantGoal)
			}
		})
	}
}

func TestExpandRejectsUnusedRequiredInput(t *testing.T) {
	r := templateRecipe("literal", "", Input{Name: "unused"})
	goal, expandedContext, err := Expand(r, nil, 100)
	if err == nil || !strings.Contains(err.Error(), "unused") {
		t.Errorf("Expand(unused required input, nil, 100) error = %v, want diagnostic naming unused", err)
	}
	if goal != "" || expandedContext != "" {
		t.Errorf("Expand(unused required input, nil, 100) = %q, %q on error, want two empty strings", goal, expandedContext)
	}
}

func TestExpandRejectsUnknownValue(t *testing.T) {
	r := templateRecipe("literal", "")
	goal, expandedContext, err := Expand(r, map[string]string{"unknown": "value"}, 100)
	if err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Errorf("Expand(no inputs, unknown=value, 100) error = %v, want diagnostic naming unknown", err)
	}
	if goal != "" || expandedContext != "" {
		t.Errorf("Expand(no inputs, unknown=value, 100) = %q, %q on error, want two empty strings", goal, expandedContext)
	}
}

func TestExpandRejectsInvalidUTF8Values(t *testing.T) {
	tests := []struct {
		name   string
		input  Input
		values map[string]string
	}{
		{name: "supplied", input: Input{Name: "x"}, values: map[string]string{"x": string([]byte{0xff})}},
		{name: "default", input: Input{Name: "x", Default: templateStringPointer(string([]byte{0xff}))}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := templateRecipe(`{{inputs.x}}`, "", tt.input)
			goal, expandedContext, err := Expand(r, tt.values, 100)
			if err == nil || !strings.Contains(err.Error(), "x") || !strings.Contains(err.Error(), "UTF-8") {
				t.Errorf("Expand(%s invalid UTF-8 x, 100) error = %v, want UTF-8 diagnostic naming x", tt.name, err)
			}
			if goal != "" || expandedContext != "" {
				t.Errorf("Expand(%s invalid UTF-8 x, 100) = %q, %q on error, want two empty strings", tt.name, goal, expandedContext)
			}
		})
	}
}

func TestTemplateAPIsRejectInvalidMetadata(t *testing.T) {
	r := templateRecipe("valid", "")
	r.Name = "Invalid"
	if err := ValidateTemplates(r); err == nil || !strings.Contains(err.Error(), "name") {
		t.Errorf("ValidateTemplates(invalid name) error = %v, want name diagnostic", err)
	}
	goal, expandedContext, err := Expand(r, nil, 100)
	if err == nil || !strings.Contains(err.Error(), "name") {
		t.Errorf("Expand(invalid name, nil, 100) error = %v, want name diagnostic", err)
	}
	if goal != "" || expandedContext != "" {
		t.Errorf("Expand(invalid name, nil, 100) = %q, %q on error, want two empty strings", goal, expandedContext)
	}
}

func TestExpandRejectsNonpositiveLimits(t *testing.T) {
	for _, tt := range []struct {
		name  string
		limit int
	}{
		{name: "zero", limit: 0},
		{name: "negative", limit: -1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := templateRecipe("valid", "")
			goal, expandedContext, err := Expand(r, nil, tt.limit)
			if err == nil || !strings.Contains(err.Error(), "positive") {
				t.Errorf("Expand(valid recipe, nil, %d) error = %v, want positive-limit diagnostic", tt.limit, err)
			}
			if goal != "" || expandedContext != "" {
				t.Errorf("Expand(valid recipe, nil, %d) = %q, %q on error, want two empty strings", tt.limit, goal, expandedContext)
			}
		})
	}
}

func TestExpandByteLimit(t *testing.T) {
	t.Run("combined exact limit", func(t *testing.T) {
		r := templateRecipe("abc", "de")
		goal, expandedContext, err := Expand(r, nil, 5)
		if err != nil || goal != "abc" || expandedContext != "de" {
			t.Errorf("Expand(abc/de, nil, 5) = %q, %q, %v, want abc, de, nil", goal, expandedContext, err)
		}
	})

	t.Run("combined one byte over", func(t *testing.T) {
		r := templateRecipe("abc", "de")
		goal, expandedContext, err := Expand(r, nil, 4)
		if err == nil || !strings.Contains(err.Error(), "4") {
			t.Errorf("Expand(abc/de, nil, 4) error = %v, want overflow diagnostic naming limit 4", err)
		}
		if goal != "" || expandedContext != "" {
			t.Errorf("Expand(abc/de, nil, 4) = %q, %q on error, want two empty strings", goal, expandedContext)
		}
	})

	t.Run("repeated value growth", func(t *testing.T) {
		r := templateRecipe(`{{inputs.x}}{{inputs.x}}`, "", Input{Name: "x"})
		goal, expandedContext, err := Expand(r, map[string]string{"x": "abc"}, 6)
		if err != nil || goal != "abcabc" || expandedContext != "" {
			t.Errorf("Expand(repeated abc, 6) = %q, %q, %v, want abcabc, empty, nil", goal, expandedContext, err)
		}
		goal, expandedContext, err = Expand(r, map[string]string{"x": "abc"}, 5)
		if err == nil || !strings.Contains(err.Error(), "5") {
			t.Errorf("Expand(repeated abc, 5) error = %v, want overflow diagnostic naming limit 5", err)
		}
		if goal != "" || expandedContext != "" {
			t.Errorf("Expand(repeated abc, 5) = %q, %q on error, want two empty strings", goal, expandedContext)
		}
	})

	t.Run("multibyte bytes", func(t *testing.T) {
		r := templateRecipe("é", "𐐷")
		goal, expandedContext, err := Expand(r, nil, 6)
		if err != nil || goal != "é" || expandedContext != "𐐷" {
			t.Errorf("Expand(é/𐐷, nil, 6) = %q, %q, %v, want é, 𐐷, nil", goal, expandedContext, err)
		}
		goal, expandedContext, err = Expand(r, nil, 5)
		if err == nil || !strings.Contains(err.Error(), "5") {
			t.Errorf("Expand(é/𐐷, nil, 5) error = %v, want overflow diagnostic naming limit 5", err)
		}
		if goal != "" || expandedContext != "" {
			t.Errorf("Expand(é/𐐷, nil, 5) = %q, %q on error, want two empty strings", goal, expandedContext)
		}
	})
}

func FuzzExpand(f *testing.F) {
	seeds := []struct {
		goal, context, value, defaultValue string
		supplied, hasDefault               bool
		limit                              uint8
	}{
		{goal: `{{inputs.x}}`, value: "VALUE", supplied: true, limit: 63},
		{goal: `\{{inputs.x}}`, value: "VALUE", supplied: true, limit: 63},
		{goal: `\\{{inputs.x}}`, value: "VALUE", supplied: true, limit: 63},
		{goal: `\\\{{inputs.x}}`, value: "VALUE", supplied: true, limit: 63},
		{goal: `{{inputs.`, value: "VALUE", supplied: true, limit: 63},
		{goal: `{{inputs.x}}`, defaultValue: "", hasDefault: true, limit: 63},
		{goal: `{{inputs.x}}`, value: string([]byte{0xff}), supplied: true, limit: 63},
		{goal: "é", context: "𐐷", defaultValue: "", hasDefault: true, limit: 5},
		{goal: `{{inputs.x}}|{{inputs.x}}`, value: `{{inputs.y}}`, supplied: true, limit: 63},
	}
	for _, seed := range seeds {
		f.Add(seed.goal, seed.context, seed.value, seed.defaultValue, seed.supplied, seed.hasDefault, seed.limit)
	}

	f.Fuzz(func(t *testing.T, goalTemplate, contextTemplate, value, defaultValue string, supplied, hasDefault bool, limitSeed uint8) {
		limit := 1 + int(limitSeed%64)
		input := Input{Name: "x"}
		if hasDefault {
			input.Default = &defaultValue
		}
		r := templateRecipe(goalTemplate, contextTemplate, input, Input{Name: "y", Default: templateStringPointer("SECRET")})
		values := map[string]string{}
		if supplied {
			values["x"] = value
		}
		goal, expandedContext, err := Expand(r, values, limit)
		if err != nil {
			if goal != "" || expandedContext != "" {
				t.Fatalf("Expand(fuzz recipe, values, %d) = %q, %q on error %v, want two empty strings", limit, goal, expandedContext, err)
			}
		} else if len(goal)+len(expandedContext) > limit {
			t.Fatalf("Expand(fuzz recipe, values, %d) output bytes = %d, want <= %d", limit, len(goal)+len(expandedContext), limit)
		}

		fixed := templateRecipe(`{{inputs.x}}|{{inputs.x}}`, "", Input{Name: "x"}, Input{Name: "y"})
		fixedValues := map[string]string{"x": value, "y": "SECRET"}
		fixedGoal, fixedContext, fixedErr := Expand(fixed, fixedValues, limit)
		if fixedErr != nil {
			if fixedGoal != "" || fixedContext != "" {
				t.Fatalf("Expand(fixed repeated recipe, values, %d) = %q, %q on error %v, want two empty strings", limit, fixedGoal, fixedContext, fixedErr)
			}
			return
		}
		if len(fixedGoal)+len(fixedContext) > limit {
			t.Fatalf("Expand(fixed repeated recipe, values, %d) output bytes = %d, want <= %d", limit, len(fixedGoal)+len(fixedContext), limit)
		}
		if utf8.ValidString(value) && len(value)*2+1 <= limit {
			want := value + "|" + value
			if fixedGoal != want || fixedContext != "" {
				t.Fatalf("Expand(fixed repeated recipe, x=%q, %d) = %q, %q, want %q, empty", value, limit, fixedGoal, fixedContext, want)
			}
		}
	})
}

func templateRecipe(goal, context string, inputs ...Input) Recipe {
	return Recipe{
		Version:     1,
		Name:        "review",
		Description: "Review",
		Goal:        goal,
		Context:     context,
		Inputs:      inputs,
	}
}

func templateStringPointer(value string) *string {
	return &value
}
