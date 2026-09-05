package interceptor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"

	"github.com/kstruzzieri/go-llm/agent"
)

// InvariantRisk is the risk contribution of every invariant block: a policy
// stop, weighted like an exact instruction phrase.
const InvariantRisk = 30

// Invariant is one declarative argument bound for one tool (#439 D2). Tool is
// the registered tool name, Name the stable identifier that becomes the
// Finding.Rule (and so the model-visible block reason), Field the top-level
// JSON argument the check reads. A call whose arguments lack Field is not a
// violation: required-ness is the tool's job.
type Invariant struct {
	Tool, Name, Field string
	Check             Check
}

// Check is the sealed evaluator vocabulary. Tables are composed from the
// shipped value kinds (PathDeny, RemoteScript); NewInvariants rejects
// anything else, pointers included, so a check can neither panic nor
// silently disable enforcement.
type Check interface {
	check(raw json.RawMessage) (detail string, violated bool)
}

// PathDeny matches Pattern against the path in a string field after the
// host's own normalization (see normalizePath). Native cleaning is
// authoritative, so on POSIX a backslash stays a filename character exactly
// as the workspace treats it. A non-string value is not a violation; the
// tool rejects it. NewInvariants keeps its own compiled copy of Pattern.
type PathDeny struct{ Pattern *regexp.Regexp }

func (d PathDeny) check(raw json.RawMessage) (string, bool) {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	clean := normalizePath(s)
	if !d.Pattern.MatchString(clean) {
		return "", false
	}
	return "path " + strconv.Quote(clean) + " matches protected pattern", true
}

// normalizePath renders a path the way the host will open it, as far as a
// lexical check can: filepath.Clean, filepath.ToSlash, on Windows the Win32
// trim of trailing periods and spaces from each component (the OS opens
// ".git." as ".git"; a component made only of periods is a valid name and is
// kept), then a lower-casing of every component. Lower-casing is deliberate:
// on APFS and NTFS ".Git" is ".git", and on a case-sensitive filesystem it
// only over-blocks a distinct spelling. Short (8.3) names and other OS
// aliases are outside a lexical check; the workspace layer is the boundary.
func normalizePath(s string) string {
	clean := filepath.ToSlash(filepath.Clean(s))
	if runtime.GOOS == "windows" {
		parts := strings.Split(clean, "/")
		for i, p := range parts {
			if t := strings.TrimRight(p, ". "); t != "" {
				parts[i] = t
			}
		}
		clean = strings.Join(parts, "/")
	}
	return strings.ToLower(clean)
}

// Default path patterns (#439 D7): repository internals and credential
// directories as a component at any depth for writes; credential directories
// plus the exact basename .env for direct reads.
var (
	protectedPattern  = regexp.MustCompile(`(^|/)\.(git|ssh|gnupg|aws|kube)(/|$)`)
	credentialPattern = regexp.MustCompile(`(^|/)\.(ssh|gnupg|aws|kube)(/|$)|(^|/)\.env$`)
)

// DefaultInvariants returns the shipped table as fresh storage on every call.
// Tool and field names are pinned against the real tool schemas by the
// agent/tools drift test.
func DefaultInvariants() []Invariant {
	protected := PathDeny{Pattern: protectedPattern}
	credential := PathDeny{Pattern: credentialPattern}
	return []Invariant{
		{Tool: "write_file", Name: "protected_path", Field: "path", Check: protected},
		{Tool: "edit_file", Name: "protected_path", Field: "path", Check: protected},
		{Tool: "promote_artifact", Name: "protected_path", Field: "path", Check: protected},
		{Tool: "read_file", Name: "credential_path", Field: "path", Check: credential},
	}
}

// Invariants enforces a table at InspectToolCall with a Block verdict,
// regardless of origin: invariants are policy, not detection. It owns a copy
// of the validated table, so later mutation of the caller's slice changes
// nothing; read-only after construction and safe for concurrent runs.
type Invariants struct {
	byTool map[string][]Invariant
}

var _ agent.Interceptor = Invariants{}

// ambiguousRule is the synthetic block for a guarded field spelled more than
// once; a table may not declare an invariant under this name.
const ambiguousRule = "ambiguous_argument"

// NewInvariants validates and indexes a table: every entry needs a tool, a
// bounded identifier name (the library's rule for model-visible framing)
// other than the reserved ambiguous_argument, a field, and a supported
// concrete check with its data present; (Tool, Name) must be unique. The
// result owns copies of every entry and of every compiled pattern, so the
// caller's later mutation of its table or its regexps cannot change or race
// an installed guard.
func NewInvariants(table []Invariant) (Invariants, error) {
	byTool := make(map[string][]Invariant)
	seen := make(map[[2]string]bool, len(table))
	for i, inv := range table {
		if inv.Tool == "" {
			return Invariants{}, fmt.Errorf("interceptor: invariant %d has no tool", i)
		}
		if !validRuleName(inv.Name) || inv.Name == ambiguousRule {
			return Invariants{}, fmt.Errorf("interceptor: invariant %d (%s) has invalid name %q", i, inv.Tool, inv.Name)
		}
		if inv.Field == "" {
			return Invariants{}, fmt.Errorf("interceptor: invariant %s/%s has no field", inv.Tool, inv.Name)
		}
		owned, err := ownedCheck(inv)
		if err != nil {
			return Invariants{}, err
		}
		inv.Check = owned
		key := [2]string{inv.Tool, inv.Name}
		if seen[key] {
			return Invariants{}, fmt.Errorf("interceptor: duplicate invariant %s/%s", inv.Tool, inv.Name)
		}
		seen[key] = true
		byTool[inv.Tool] = append(byTool[inv.Tool], inv)
	}
	return Invariants{byTool: byTool}, nil
}

// ownedCheck accepts only the shipped value kinds with their data present and
// returns a copy whose data the guard owns.
func ownedCheck(inv Invariant) (Check, error) {
	switch c := inv.Check.(type) {
	case nil:
		return nil, fmt.Errorf("interceptor: invariant %s/%s has no check", inv.Tool, inv.Name)
	case PathDeny:
		if c.Pattern == nil {
			return nil, fmt.Errorf("interceptor: invariant %s/%s has a PathDeny with no pattern", inv.Tool, inv.Name)
		}
		// A compiled Regexp still carries settable state (Longest); an owned
		// recompile of its source keeps the caller's handle out of the guard.
		re, err := regexp.Compile(c.Pattern.String())
		if err != nil {
			return nil, fmt.Errorf("interceptor: invariant %s/%s pattern: %w", inv.Tool, inv.Name, err)
		}
		return PathDeny{Pattern: re}, nil
	default:
		return nil, fmt.Errorf("interceptor: invariant %s/%s has unsupported check kind %T", inv.Tool, inv.Name, inv.Check)
	}
}

// mustInvariants builds the shipped chain's table; the default table is
// compile-time data, so an error here is a programming error.
func mustInvariants(table []Invariant) Invariants {
	iv, err := NewInvariants(table)
	if err != nil {
		panic(err)
	}
	return iv
}

// validRuleName mirrors the library's bounded identifier rule for names that
// reach model-visible framing: [A-Za-z0-9_.:-], 1..64 bytes.
func validRuleName(name string) bool {
	if name == "" || len(name) > 64 {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_', c == '.', c == ':', c == '-':
		default:
			return false
		}
	}
	return true
}

// Name returns "invariants".
func (Invariants) Name() string { return "invariants" }

// InspectInput returns nothing: invariants are about tool arguments.
func (Invariants) InspectInput(context.Context, agent.InputInspection) ([]agent.Finding, error) {
	return nil, nil
}

// InspectOutput returns nothing: model output policy belongs to #438/#437.
func (Invariants) InspectOutput(context.Context, agent.OutputInspection) ([]agent.Finding, error) {
	return nil, nil
}

// InspectToolCall evaluates every invariant declared for the called tool
// against the top-level members of the argument object, read in source order
// with the same case-insensitive name equivalence encoding/json applies to
// struct fields, so the guard sees the member the tool will use. Two or more
// equivalent spellings of a guarded field are blocked as ambiguous_argument
// rather than resolved by any ordering rule. Arguments that are not an
// object, or that lack the field, produce nothing: the tool rejects those.
//
// Parity rests on encoding/json's field lookup: an exact name match first,
// otherwise the first field whose folded name equals the folded key, where
// foldName is documented as equivalent to bytes.EqualFold. strings.EqualFold
// applies the same simple folding, so every key the struct decoder would
// route to the field is matched here, and no other.
func (iv Invariants) InspectToolCall(_ context.Context, call agent.ToolCallInspection) ([]agent.Finding, error) {
	table := iv.byTool[call.Call.Function.Name]
	if len(table) == 0 {
		return nil, nil
	}
	members, ok := topLevelMembers(call.Call.Function.Arguments)
	if !ok {
		// Not an object. Dispatch already required valid JSON, and a struct
		// decode of an array or string fails while null yields a zero struct
		// the tool rejects as missing, so nothing reaches Invoke this way.
		return nil, nil
	}
	tg := toolCallTarget(call.Call.ID)
	var out []agent.Finding
	for _, inv := range table {
		var matched []json.RawMessage
		for _, m := range members {
			if strings.EqualFold(m.key, inv.Field) {
				matched = append(matched, m.value)
			}
		}
		switch len(matched) {
		case 0:
		case 1:
			if detail, violated := inv.Check.check(matched[0]); violated {
				out = append(out, tg.finding(inv.Name, agent.VerdictBlock, InvariantRisk, detail))
			}
		default:
			detail := fmt.Sprintf("argument %q appears %d times under equivalent spellings", inv.Field, len(matched))
			out = append(out, tg.finding(ambiguousRule, agent.VerdictBlock, InvariantRisk, detail))
		}
	}
	return out, nil
}

// member is one top-level object member, value left encoded.
type member struct {
	key   string
	value json.RawMessage
}

// topLevelMembers lists an argument object's members in source order; false
// when the arguments are not a JSON object. Values are not decoded here, so
// an unrelated member of any shape cannot disturb a guarded one.
func topLevelMembers(raw json.RawMessage) ([]member, bool) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		return nil, false
	}
	var out []member
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, false
		}
		key, ok := tok.(string)
		if !ok {
			return nil, false
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return nil, false
		}
		out = append(out, member{key: key, value: value})
	}
	if _, err := dec.Token(); err != nil {
		return nil, false
	}
	return out, true
}
