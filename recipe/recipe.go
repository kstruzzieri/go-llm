package recipe

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	// CurrentVersion is the supported recipe format version.
	CurrentVersion = 1
	// MaxBytes is the largest recipe document Parse accepts.
	MaxBytes = 64 * 1024
)

// Recipe is a reusable prompt bundle.
type Recipe struct {
	Version     int        `json:"version"`
	Name        string     `json:"name"`
	Description string     `json:"description"`
	Goal        string     `json:"goal"`
	Context     string     `json:"context"`
	Inputs      []Input    `json:"inputs"`
	ModelHint   *ModelHint `json:"model_hint"`
}

// Input declares a value used by a recipe. A nil Default requires a caller
// supplied value; a non-nil Default is the exact fallback, including empty.
type Input struct {
	Name        string  `json:"name"`
	Description string  `json:"description"`
	Default     *string `json:"default"`
}

// ModelHint suggests a configured model selection key.
type ModelHint struct {
	Role    string `json:"role"`
	UseCase string `json:"use_case"`
}

// Parse validates and decodes a recipe document up to MaxBytes. It returns a
// zero Recipe on error.
func Parse(data []byte) (Recipe, error) {
	if len(data) > MaxBytes {
		return Recipe{}, fmt.Errorf("recipe: size: exceeds %d bytes", MaxBytes)
	}
	if !utf8.Valid(data) {
		return Recipe{}, fmt.Errorf("recipe: UTF-8: invalid encoding")
	}
	if isEmptyDocument(data) {
		return Recipe{}, fmt.Errorf("recipe: empty document")
	}
	if err := checkJSONKeys(data); err != nil {
		return Recipe{}, err
	}

	var recipe Recipe
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&recipe); err != nil {
		return Recipe{}, fmt.Errorf("recipe: decode: %w", err)
	}
	var trailing any
	err := dec.Decode(&trailing)
	if err == nil {
		return Recipe{}, fmt.Errorf("recipe: decode: extra value")
	}
	if !errors.Is(err, io.EOF) {
		return Recipe{}, fmt.Errorf("recipe: decode trailing data: %w", err)
	}
	if err := validate(recipe); err != nil {
		return Recipe{}, err
	}
	return recipe, nil
}

// Load reads and parses the regular file at path. Symlinks are followed to
// their targets. It returns a zero Recipe on error.
func Load(path string) (Recipe, error) {
	before, err := os.Stat(path)
	if err != nil {
		return Recipe{}, fmt.Errorf("recipe: load %q: stat: %w", path, err)
	}
	if !before.Mode().IsRegular() {
		return Recipe{}, fmt.Errorf("recipe: load %q: target is not a regular file", path)
	}

	f, err := openRecipeFile(path)
	if err != nil {
		return Recipe{}, fmt.Errorf("recipe: load %q: open: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	recipe, err := loadOpened(f, before)
	if err != nil {
		return Recipe{}, fmt.Errorf("recipe: load %q: %w", path, err)
	}
	return recipe, nil
}

func loadOpened(f *os.File, before fs.FileInfo) (Recipe, error) {
	after, err := f.Stat()
	if err != nil {
		return Recipe{}, fmt.Errorf("stat opened file: %w", err)
	}
	if !after.Mode().IsRegular() {
		return Recipe{}, fmt.Errorf("opened target is not a regular file")
	}
	if !os.SameFile(before, after) {
		return Recipe{}, fmt.Errorf("file identity changed while opening")
	}

	data, err := readCapped(f)
	if err != nil {
		return Recipe{}, err
	}
	return Parse(data)
}

func readCapped(r io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, MaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read: %w", err)
	}
	if len(data) > MaxBytes {
		return nil, fmt.Errorf("size: exceeds %d bytes", MaxBytes)
	}
	return data, nil
}

func isEmptyDocument(data []byte) bool {
	for _, b := range data {
		if b != ' ' && b != '\t' && b != '\r' && b != '\n' {
			return false
		}
	}
	return true
}

func checkJSONKeys(data []byte) error {
	type objectState struct {
		keys      map[string]struct{}
		key       string
		expectKey bool
	}

	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil {
		return fmt.Errorf("recipe: decode: %w", err)
	}
	if tok != json.Delim('{') {
		return fmt.Errorf("recipe: root: must be an object")
	}

	stack := []*objectState{{keys: map[string]struct{}{}, expectKey: true}}
	for len(stack) > 0 {
		tok, err = dec.Token()
		if err != nil {
			return fmt.Errorf("recipe: decode: %w", err)
		}
		if delimiter, ok := tok.(json.Delim); ok {
			if top := stack[len(stack)-1]; top != nil && !top.expectKey && (delimiter == '{' || delimiter == '[') {
				switch top.key {
				case "goal", "context", "default":
					// Reject containers before their member names can reach diagnostics.
					return fmt.Errorf("recipe: %s: must be a string", top.key)
				}
			}
			switch delimiter {
			case '{':
				stack = append(stack, &objectState{keys: map[string]struct{}{}, expectKey: true})
			case '[':
				stack = append(stack, nil)
			default:
				stack = stack[:len(stack)-1]
				if len(stack) > 0 && stack[len(stack)-1] != nil {
					stack[len(stack)-1].expectKey = true
				}
			}
			continue
		}

		top := stack[len(stack)-1]
		if top == nil {
			continue
		}
		if !top.expectKey {
			top.expectKey = true
			continue
		}
		key, ok := tok.(string)
		if !ok {
			return fmt.Errorf("recipe: decode: object key is not a string")
		}
		if !knownJSONKey(key) {
			return fmt.Errorf("recipe: key %q: unknown field", key)
		}
		if _, duplicate := top.keys[key]; duplicate {
			return fmt.Errorf("recipe: duplicate key %q", key)
		}
		top.keys[key] = struct{}{}
		top.key = key
		top.expectKey = false
	}
	return nil
}

func knownJSONKey(key string) bool {
	switch key {
	case "version", "name", "description", "goal", "context", "inputs", "model_hint",
		"default", "role", "use_case":
		return true
	default:
		return false
	}
}

func validate(recipe Recipe) error {
	if recipe.Version != CurrentVersion {
		return fmt.Errorf("recipe: version: must be %d", CurrentVersion)
	}
	if !validName(recipe.Name, true) {
		return fmt.Errorf("recipe: name: must match [a-z][a-z0-9_-]*")
	}
	if strings.TrimSpace(recipe.Description) == "" {
		return fmt.Errorf("recipe: description: must be nonblank")
	}
	if strings.TrimSpace(recipe.Goal) == "" {
		return fmt.Errorf("recipe: goal: must be nonblank")
	}
	inputNames := make(map[string]struct{}, len(recipe.Inputs))
	for i, input := range recipe.Inputs {
		if !validName(input.Name, false) {
			return fmt.Errorf("recipe: inputs[%d].name: must match [a-z][a-z0-9_]*", i)
		}
		if _, exists := inputNames[input.Name]; exists {
			return fmt.Errorf("recipe: inputs[%d].name: duplicate %q", i, input.Name)
		}
		inputNames[input.Name] = struct{}{}
	}
	return validateModelHint(recipe.ModelHint)
}

func validName(value string, allowHyphen bool) bool {
	if value == "" || value[0] < 'a' || value[0] > 'z' {
		return false
	}
	for i := 1; i < len(value); i++ {
		c := value[i]
		if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' || allowHyphen && c == '-' {
			continue
		}
		return false
	}
	return true
}

func validateModelHint(hint *ModelHint) error {
	if hint == nil {
		return nil
	}
	if (hint.Role == "") == (hint.UseCase == "") {
		return fmt.Errorf("recipe: model_hint: exactly one of role or use_case is required")
	}
	if hint.Role != "" {
		if strings.TrimSpace(hint.Role) == "" || containsControl(hint.Role) {
			return fmt.Errorf("recipe: model_hint.role: must be nonblank and contain no control characters")
		}
		return nil
	}
	if strings.TrimSpace(hint.UseCase) == "" || containsControl(hint.UseCase) {
		return fmt.Errorf("recipe: model_hint.use_case: must be nonblank and contain no control characters")
	}
	return nil
}

func containsControl(value string) bool {
	return strings.IndexFunc(value, unicode.IsControl) >= 0
}
