package mcpclient

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// AdmissionError identifies a blocked alias without exposing transport errors,
// credentials or unvalidated remote prose. Reason is a bounded diagnostic code.
type AdmissionError struct {
	Alias           string
	Reason          string
	PinnedDigest    string
	CandidateDigest string
	Diff            CatalogDiff
	cause           error
}

func (e *AdmissionError) Error() string {
	text := fmt.Sprintf("server %q: %s", e.Alias, e.Reason)
	if e.PinnedDigest != "" {
		text += "; pinned " + e.PinnedDigest
	}
	if e.CandidateDigest != "" {
		text += "; candidate " + e.CandidateDigest
		if diff := e.Diff.String(); diff != "" {
			text += "; " + diff
		}
		text += fmt.Sprintf("; review with golem mcp inspect, then run golem mcp approve with the same -root and server arguments, explicit alias=%s, and -digest %s", e.Alias, e.CandidateDigest)
	}
	if e.Reason == "pin_durability" {
		text += "; published bytes may already be present; durability is unconfirmed"
	}
	return text
}

// Unwrap permits cancellation/conflict checks without displaying the cause.
func (e *AdmissionError) Unwrap() error { return e.cause }

func admissionFailure(alias, reason string, cause error) *AdmissionError {
	var previous *AdmissionError
	if errors.As(cause, &previous) {
		reason = previous.Reason
	}
	switch {
	case errors.Is(cause, errPinDurability):
		reason = "pin_durability"
	case errors.Is(cause, context.Canceled), errors.Is(cause, context.DeadlineExceeded):
		reason = "canceled"
	case errors.Is(cause, errPinMissing):
		reason = "pin_missing"
	case errors.Is(cause, errPinMismatch):
		reason = "catalog_changed"
	case errors.Is(cause, errPinRevisionConflict):
		reason = "pin_conflict"
	case errors.Is(cause, errPinContention):
		reason = "pin_contention"
	}
	return &AdmissionError{Alias: alias, Reason: reason, cause: cause}
}

// CatalogChange names a changed tool and the definition fields that changed.
type CatalogChange struct {
	Name   string
	Fields []string
}

// CatalogDiff contains only validated names and fixed field labels, sorted by name.
type CatalogDiff struct {
	Added, Removed []string
	Changed        []CatalogChange
}

func (d CatalogDiff) String() string {
	var parts []string
	if len(d.Added) > 0 {
		parts = append(parts, "added: "+strings.Join(d.Added, ", "))
	}
	if len(d.Removed) > 0 {
		parts = append(parts, "removed: "+strings.Join(d.Removed, ", "))
	}
	if len(d.Changed) > 0 {
		names := make([]string, 0, len(d.Changed))
		for _, c := range d.Changed {
			names = append(names, c.Name+" ("+strings.Join(c.Fields, ", ")+")")
		}
		parts = append(parts, "changed: "+strings.Join(names, ", "))
	}
	return strings.Join(parts, "; ")
}
func diffCatalogs(old, next toolCatalog) CatalogDiff {
	before, after := make(map[string]catalogEntry), make(map[string]catalogEntry)
	for _, e := range old.entries {
		before[e.Name] = e
	}
	for _, e := range next.entries {
		after[e.Name] = e
	}
	var diff CatalogDiff
	for _, e := range next.entries {
		prior, ok := before[e.Name]
		if !ok {
			diff.Added = append(diff.Added, e.Name)
			continue
		}
		var fields []string
		if prior.Description != e.Description {
			fields = append(fields, "description")
		}
		if !bytes.Equal(prior.InputSchema, e.InputSchema) {
			fields = append(fields, "inputSchema")
		}
		if len(fields) > 0 {
			diff.Changed = append(diff.Changed, CatalogChange{Name: e.Name, Fields: fields})
		}
	}
	for _, e := range old.entries {
		if _, ok := after[e.Name]; !ok {
			diff.Removed = append(diff.Removed, e.Name)
		}
	}
	return diff
}
func catalogNames(c toolCatalog) []string {
	names := make([]string, len(c.entries))
	for i, e := range c.entries {
		names[i] = e.Name
	}
	return names
}

// Inspection describes a fetched candidate and the prior pin. String includes
// quoted complete definitions for operator review; it never includes endpoints.
type Inspection struct {
	Alias             string
	PinPath           string
	PinnedDigest      string // empty means absent, distinct from a pinned empty catalog
	CandidateDigest   string
	Diff              CatalogDiff
	pinned, candidate toolCatalog
}

func (i *Inspection) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "server %q\npin file: %s\npinned: %s\ncandidate: %s\n", i.Alias, strconv.QuoteToGraphic(i.PinPath), i.PinnedDigest, i.CandidateDigest)
	if diff := i.Diff.String(); diff != "" {
		fmt.Fprintln(&b, diff)
	}
	for _, set := range []struct {
		label   string
		catalog toolCatalog
	}{{"pinned", i.pinned}, {"candidate", i.candidate}} {
		for _, e := range set.catalog.entries {
			fmt.Fprintf(&b, "%s %s\n  description: %s\n  inputSchema: %s\n", set.label, e.Name, strconv.QuoteToGraphic(e.Description), strconv.QuoteToGraphic(string(e.InputSchema)))
		}
	}
	return b.String()
}
func inspection(s *PinStore, alias string, prior, candidate toolCatalog) *Inspection {
	return &Inspection{Alias: alias, PinPath: filepath.Join(s.dir, pinKey(alias)+".json"), PinnedDigest: prior.digest(), CandidateDigest: candidate.digest(), Diff: diffCatalogs(prior, candidate), pinned: prior, candidate: candidate}
}

var digestRE = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

func validateTrustConfig(servers []Server, pins *PinStore) error {
	if pins == nil || pins.workspace == "" || pins.dir == "" {
		return errors.New("mcpclient: initialized pin store required")
	}
	seen := make(map[string]bool, len(servers))
	for _, s := range servers {
		if !validAlias(s.Alias) {
			return errors.New("mcpclient: invalid server alias")
		}
		if seen[s.Alias] {
			return fmt.Errorf("mcpclient: duplicate server alias %q", s.Alias)
		}
		seen[s.Alias] = true
		if _, err := s.transport(); err != nil {
			return admissionFailure(s.Alias, "invalid_config", err)
		}
	}
	return nil
}

// Inspect fetches and validates a complete catalog without creating a pin or
// invoking a tool. Callers must supply an explicitly selected stable alias.
func Inspect(ctx context.Context, impl Implementation, server Server, pins *PinStore) (*Inspection, error) {
	return inspectOrApprove(ctx, impl, server, pins, "")
}

// Approve re-fetches the catalog and replaces the captured pin revision only if
// its internally computed digest exactly equals the operator's supplied digest.
func Approve(ctx context.Context, impl Implementation, server Server, pins *PinStore, digest string) (*Inspection, error) {
	if !digestRE.MatchString(digest) {
		return nil, errors.New("mcpclient: digest must be sha256: followed by 64 lowercase hex digits")
	}
	return inspectOrApprove(ctx, impl, server, pins, digest)
}
func inspectOrApprove(ctx context.Context, impl Implementation, server Server, pins *PinStore, digest string) (result *Inspection, err error) {
	if err = validateTrustConfig([]Server{server}, pins); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	prior, revision, err := pins.capturePin(ctx, server.Alias)
	if err != nil {
		return nil, admissionFailure(server.Alias, "pin_unavailable", err)
	}
	session, _, candidate, _, err := discover(ctx, impl, server)
	if err != nil {
		return nil, err
	}
	// Close discovery before publication so failed cleanup cannot look like an
	// uncommitted approval to the operator after we have changed the pin.
	if err = session.Close(); err != nil {
		return nil, admissionFailure(server.Alias, "unavailable", err)
	}
	if err = ctx.Err(); err != nil {
		return nil, admissionFailure(server.Alias, "canceled", err)
	}
	result = inspection(pins, server.Alias, prior, candidate)
	if digest != "" {
		if candidate.digest() != digest {
			failure := admissionFailure(server.Alias, "digest_mismatch", nil)
			failure.PinnedDigest = prior.digest()
			failure.CandidateDigest = candidate.digest()
			failure.Diff = result.Diff
			return nil, failure
		}
		if err = pins.replacePin(ctx, server.Alias, revision, candidate); err != nil {
			return nil, admissionFailure(server.Alias, "pin_unavailable", err)
		}
	}
	return result, nil
}
