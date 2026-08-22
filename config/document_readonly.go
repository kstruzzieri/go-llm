package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// collisionContext carries the innermost safe subject for a collision:
// providers entry -> {provider, name}; models entry -> {role, name};
// defaults key -> {use_case, key}; top level and nested struct levels
// inherit their enclosing entry's subject.
type collisionContext struct {
	kind      SubjectKind
	subject   string
	entryKind SubjectKind // set on section map levels: what an entry key becomes
}

func (c collisionContext) forEntry(key string) collisionContext {
	if c.entryKind == SubjectNone {
		return c
	}
	return collisionContext{kind: c.entryKind, subject: key}
}

func collisionDiagnostic(c collisionContext) Diagnostic {
	return Diagnostic{
		Code:        CodeDuplicateKeys,
		SubjectKind: c.kind,
		Subject:     sanitizeSubject(c.subject),
	}
}

// sectionEntryKind maps a top-level section tag to the SubjectKind its map
// entries carry. Asserted against configSchema by a test: a new Config
// section must make a conscious choice here.
func sectionEntryKind(field string) SubjectKind {
	switch field {
	case "providers":
		return SubjectProvider
	case "models":
		return SubjectRole
	case "defaults":
		return SubjectUseCase
	default:
		return SubjectNone
	}
}

// detectCollisions token-walks data under the shared schema (spec §5),
// returning the first collision in document order: an exact duplicate key
// at any walker-visited level, or a case-fold alias of a known tag at a
// struct level. Unknown-member subtrees are skipped wholesale.
func detectCollisions(data []byte) (Diagnostic, bool, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	return walkCollisionValue(dec, configSchema(), collisionContext{})
}

// walkCollisionValue consumes one JSON value whose first token has NOT yet
// been read, recursing into objects at schema-walked levels.
func walkCollisionValue(dec *json.Decoder, n *schemaNode, ctx collisionContext) (Diagnostic, bool, error) {
	first, err := dec.Token()
	if err != nil {
		return Diagnostic{}, false, err
	}
	if n == nil {
		return Diagnostic{}, false, skipJSONValue(dec, first)
	}
	delim, ok := first.(json.Delim)
	if !ok {
		return Diagnostic{}, false, nil // scalar under a schema node: nothing to walk
	}
	if delim != '{' {
		return Diagnostic{}, false, skipJSONValue(dec, first)
	}
	return walkCollisionObject(dec, n, ctx)
}

// walkCollisionObject consumes an object body after its opening brace.
func walkCollisionObject(dec *json.Decoder, n *schemaNode, ctx collisionContext) (Diagnostic, bool, error) {
	root := n == configSchema() // memoized: pointer identity marks the root
	seen := map[string]bool{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return Diagnostic{}, false, err
		}
		key, ok := tok.(string)
		if !ok {
			return Diagnostic{}, false, fmt.Errorf("config: collision walk: object key is not a string")
		}

		keyCtx := ctx
		if n.isMap() {
			keyCtx = ctx.forEntry(key)
		}
		if seen[key] {
			return collisionDiagnostic(keyCtx), true, nil
		}
		seen[key] = true

		var child *schemaNode
		childCtx := keyCtx
		switch {
		case n.isStruct():
			var exact bool
			child, exact = n.known[key]
			if !exact {
				for tag := range n.known {
					if strings.EqualFold(key, tag) { // key != tag guaranteed by !exact
						return collisionDiagnostic(ctx), true, nil
					}
				}
			}
			if root {
				childCtx.entryKind = sectionEntryKind(key)
			}
		case n.isMap():
			child = n.elem
		}

		// child may be nil: walkCollisionValue's n==nil branch is the skip path.
		diag, found, err := walkCollisionValue(dec, child, childCtx)
		if err != nil || found {
			return diag, found, err
		}
	}
	if _, err := dec.Token(); err != nil { // closing brace
		return Diagnostic{}, false, err
	}
	return Diagnostic{}, false, nil
}

// skipJSONValue consumes the remainder of a scalar/array/object whose first
// token has already been consumed. Unknown-member subtrees route only here.
func skipJSONValue(dec *json.Decoder, first json.Token) error {
	delim, ok := first.(json.Delim)
	if !ok || (delim != '{' && delim != '[') {
		return nil
	}
	depth := 1
	for depth > 0 {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		switch tok {
		case json.Delim('{'), json.Delim('['):
			depth++
		case json.Delim('}'), json.Delim(']'):
			depth--
		}
	}
	return nil
}

// readOnlyErrLocked returns the standing refusal for a collision-marked
// document. Callers hold d.mu.
func (d *Document) readOnlyErrLocked() error {
	if d.readOnly == nil {
		return nil
	}
	msg := fmt.Errorf("config: document is read-only: duplicate or case-alias keys")
	if d.readOnly.Subject != "" {
		msg = fmt.Errorf("config: document is read-only: duplicate or case-alias keys under %s %q",
			d.readOnly.SubjectKind, d.readOnly.Subject)
	}
	return diagWrap(d.readOnly.Code, d.readOnly.SubjectKind, d.readOnly.Subject, msg)
}

// ReadOnly returns the collision diagnostic by value; false means writable.
// Panels gate edit affordances on it up front instead of discovering
// refusal per-mutation.
func (d *Document) ReadOnly() (Diagnostic, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.readOnly == nil {
		return Diagnostic{}, false
	}
	return *d.readOnly, true
}
