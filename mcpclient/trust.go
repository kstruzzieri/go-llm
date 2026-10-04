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
	// Names lists validated names relevant to Reason (for selection_missing,
	// the selected tools absent from the admitted catalog). Never remote prose.
	Names []string
	// ConnectionChanges lists fixed labels for a changed connection identity
	// (connection_changed). Never values, paths or fingerprints.
	ConnectionChanges []string
	cause             error
}

// reviewReasons are refusals an operator resolves with inspect and approve.
var reviewReasons = map[string]bool{"pin_missing": true, "connection_missing": true, "connection_changed": true}

func (e *AdmissionError) Error() string {
	text := fmt.Sprintf("server %q: %s", e.Alias, e.Reason)
	if len(e.Names) > 0 {
		text += ": " + strings.Join(e.Names, ", ")
	}
	if len(e.ConnectionChanges) > 0 {
		text += "; connection fields: " + strings.Join(e.ConnectionChanges, ", ")
	}
	if e.PinnedDigest != "" {
		text += "; pinned " + e.PinnedDigest
	}
	if e.CandidateDigest != "" {
		text += "; candidate " + e.CandidateDigest
		if diff := e.Diff.String(); diff != "" {
			text += "; " + diff
		}
	}
	// Approval needs the connection fingerprint, which only inspect shows, so
	// the hint never offers a ready-made digest.
	if e.CandidateDigest != "" || reviewReasons[e.Reason] {
		text += fmt.Sprintf("; review with golem mcp inspect using the same -root, server and -mcp-env arguments and explicit alias=%s, then golem mcp approve with the -digest and -connection it prints", e.Alias)
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
	var changed *connectionChangedError
	isChanged := errors.As(cause, &changed)
	switch {
	case errors.Is(cause, errPinDurability):
		reason = "pin_durability"
	case errors.Is(cause, errRedirectRefused):
		reason = "redirect_refused"
	case errors.Is(cause, errDestinationRefused):
		reason = "destination_refused"
	case errors.Is(cause, context.Canceled), errors.Is(cause, context.DeadlineExceeded):
		reason = "canceled"
	case errors.Is(cause, errPinMissing):
		reason = "pin_missing"
	case errors.Is(cause, errConnectionMissing):
		reason = "connection_missing"
	case isChanged:
		reason = "connection_changed"
	case errors.Is(cause, errPinMismatch):
		reason = "catalog_changed"
	case errors.Is(cause, errPinRevisionConflict):
		reason = "pin_conflict"
	case errors.Is(cause, errPinContention):
		reason = "pin_contention"
	}
	failure := &AdmissionError{Alias: alias, Reason: reason, cause: cause}
	if isChanged {
		failure.ConnectionChanges = append([]string(nil), changed.labels...)
	}
	return failure
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

func validateTrustConfig(servers []Server, pins *PinStore, policy envPolicy) error {
	if pins == nil || pins.workspace == "" || pins.dir == "" || pins.connKey == nil {
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
		if err := validateSelection(s); err != nil {
			// Selection diagnostics contain only positions and fixed text.
			return fmt.Errorf("%w: %s", admissionFailure(s.Alias, "invalid_config", err), err)
		}
		if err := validateLaunchPolicy(s, policy); err != nil {
			// Launch-policy diagnostics contain only positions and fixed text.
			return fmt.Errorf("%w: %s", admissionFailure(s.Alias, "invalid_config", err), err)
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
	if err = validateTrustConfig([]Server{server}, pins, hostEnvPolicy()); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	prior, revision, err := pins.capturePin(ctx, server.Alias)
	if err != nil {
		return nil, admissionFailure(server.Alias, "pin_unavailable", err)
	}
	prepared, err := prepare(server, pins.workspace, hostLaunchEnv())
	if err != nil {
		return nil, err
	}
	conn, err := pins.digestConnection(ctx, prepared.identity)
	if err != nil {
		// As in connectOne: an unfingerprintable identity is an unusable launch.
		return nil, admissionFailure(server.Alias, "launch_invalid", err)
	}
	session, _, candidate, _, err := discover(ctx, impl, prepared)
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
	result = inspection(pins, server.Alias, prior.toolCatalog, candidate)
	if digest != "" {
		if candidate.digest() != digest {
			failure := admissionFailure(server.Alias, "digest_mismatch", nil)
			failure.PinnedDigest = prior.digest()
			failure.CandidateDigest = candidate.digest()
			failure.Diff = result.Diff
			return nil, failure
		}
		if err = pins.replacePin(ctx, server.Alias, revision, pinEntry{toolCatalog: candidate, conn: conn}); err != nil {
			return nil, admissionFailure(server.Alias, "pin_unavailable", err)
		}
	}
	return result, nil
}
