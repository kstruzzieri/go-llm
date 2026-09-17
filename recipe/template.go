package recipe

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

const inputMarker = "{{inputs."

// ValidateTemplates validates active input references in a recipe's goal and
// context without requiring values for declared inputs.
func ValidateTemplates(r Recipe) error {
	if err := validate(r); err != nil {
		return fmt.Errorf("recipe: %w", err)
	}

	inputs := make(map[string]string, len(r.Inputs))
	for _, input := range r.Inputs {
		inputs[input.Name] = ""
	}
	if err := scanTemplate(r.Goal, "goal", inputs, nil); err != nil {
		return fmt.Errorf("recipe: %w", err)
	}
	if err := scanTemplate(r.Context, "context", inputs, nil); err != nil {
		return fmt.Errorf("recipe: %w", err)
	}
	return nil
}

// Expand validates and substitutes a recipe's input references. maxBytes
// limits the combined byte length of the expanded goal and context.
func Expand(r Recipe, values map[string]string, maxBytes int) (goal, context string, err error) {
	if err := validate(r); err != nil {
		return "", "", fmt.Errorf("recipe: %w", err)
	}
	if maxBytes <= 0 {
		return "", "", fmt.Errorf("recipe: maxBytes must be positive")
	}

	inputs := make(map[string]string, len(r.Inputs))
	for _, input := range r.Inputs {
		inputs[input.Name] = ""
	}
	for name, value := range values {
		if _, ok := inputs[name]; !ok {
			return "", "", fmt.Errorf("recipe: input %q is not declared", name)
		}
		if !utf8.ValidString(value) {
			return "", "", fmt.Errorf("recipe: input %q value must be valid UTF-8", name)
		}
	}
	for _, input := range r.Inputs {
		value, ok := values[input.Name]
		if !ok {
			if input.Default == nil {
				return "", "", fmt.Errorf("recipe: missing required input %q", input.Name)
			}
			value = *input.Default
		}
		if !utf8.ValidString(value) {
			return "", "", fmt.Errorf("recipe: input %q value must be valid UTF-8", input.Name)
		}
		inputs[input.Name] = value
	}

	remaining := maxBytes
	var goalBuilder, contextBuilder strings.Builder
	write := func(field string, builder *strings.Builder) func(string) error {
		return func(value string) error {
			if len(value) > remaining {
				return fmt.Errorf("%s: expansion exceeds %d bytes", field, maxBytes)
			}
			_, _ = builder.WriteString(value)
			remaining -= len(value)
			return nil
		}
	}
	if err := scanTemplate(r.Goal, "goal", inputs, write("goal", &goalBuilder)); err != nil {
		return "", "", fmt.Errorf("recipe: %w", err)
	}
	if err := scanTemplate(r.Context, "context", inputs, write("context", &contextBuilder)); err != nil {
		return "", "", fmt.Errorf("recipe: %w", err)
	}
	return goalBuilder.String(), contextBuilder.String(), nil
}

func scanTemplate(template, field string, inputs map[string]string, write func(string) error) error {
	position := 0
	for {
		relativeMarker := strings.Index(template[position:], inputMarker)
		if relativeMarker < 0 {
			if write != nil {
				return write(template[position:])
			}
			return nil
		}

		marker := position + relativeMarker
		backslashes := marker
		for backslashes > position && template[backslashes-1] == '\\' {
			backslashes--
		}
		count := marker - backslashes
		if write != nil {
			if err := write(template[position:backslashes]); err != nil {
				return err
			}
			if err := write(template[backslashes : backslashes+count/2]); err != nil {
				return err
			}
		}

		nameStart := marker + len(inputMarker)
		if count%2 == 1 {
			if write != nil {
				if err := write(inputMarker); err != nil {
					return err
				}
			}
			position = nameStart
			continue
		}

		relativeClose := strings.Index(template[nameStart:], "}}")
		if relativeClose < 0 {
			return fmt.Errorf("%s: malformed input reference", field)
		}
		nameEnd := nameStart + relativeClose
		name := template[nameStart:nameEnd]
		if !validName(name, false) {
			return fmt.Errorf("%s: malformed input reference", field)
		}
		value, ok := inputs[name]
		if !ok {
			return fmt.Errorf("%s: input %q is not declared", field, name)
		}
		if write != nil {
			if err := write(value); err != nil {
				return err
			}
		}
		position = nameEnd + 2
	}
}
