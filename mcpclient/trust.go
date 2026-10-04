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
	// Names lists validated names relevant to Reason: for selection_missing,
	// the selected tools absent from the admitted catalog; for env_unset, the
	// inherited variable names the parent lacks. Never values or remote prose.
	Names []string
	// ConnectionChanges lists fixed labels for a changed connection identity
	// (connection_changed). Never values, paths or fingerprints.
	ConnectionChanges []string
	cause             error
}

// reviewReasons are refusals an operator resolves with inspect and approve.
var reviewReasons = map[string]bool{"pin_missing": true, "connection_missing": true, "connection_changed": true, "connection_mismatch": true}

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
	if isChanged && reason == "connection_changed" {
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

// ConnectionView is the candidate connection as an operator reviews it,
// computed live. Paths are local metadata shown only by explicit inspection;
// argv, URL path and query are never included.
type ConnectionView struct {
	Fingerprint string
	Kind        string
	Launcher    string
	Target      string
	Dir         string
	Env         []string // source:NAME
	Origin      string
}

// Inspection describes a fetched candidate and the prior pin. String includes
// quoted complete definitions for operator review; it never includes argv,
// URL paths or queries.
type Inspection struct {
	Alias               string
	PinPath             string
	PinnedDigest        string // empty means absent, distinct from a pinned empty catalog
	CandidateDigest     string
	Diff                CatalogDiff
	CandidateConnection ConnectionView
	PinnedConnection    string   // pinned fingerprint; empty when absent or a v1 record
	ConnectionChanges   []string // fixed labels; nil when unchanged or nothing pinned
	pinned, candidate   toolCatalog
}

func (i *Inspection) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "server %q\npin file: %s\npinned: %s\ncandidate: %s\n", i.Alias, strconv.QuoteToGraphic(i.PinPath), i.PinnedDigest, i.CandidateDigest)
	if diff := i.Diff.String(); diff != "" {
		fmt.Fprintln(&b, diff)
	}
	c := i.CandidateConnection
	fmt.Fprintf(&b, "connection pinned: %s\nconnection candidate: %s\n", i.PinnedConnection, c.Fingerprint)
	if len(i.ConnectionChanges) > 0 {
		fmt.Fprintf(&b, "connection changed: %s\n", strings.Join(i.ConnectionChanges, ", "))
	}
	fmt.Fprintf(&b, "connection kind: %s\n", c.Kind)
	if c.Kind == "http" {
		fmt.Fprintf(&b, "connection origin: %s\n", strconv.QuoteToGraphic(c.Origin))
	} else {
		fmt.Fprintf(&b, "connection launcher: %s\nconnection target: %s\nconnection dir: %s\nconnection env: %s\n",
			strconv.QuoteToGraphic(c.Launcher), strconv.QuoteToGraphic(c.Target), strconv.QuoteToGraphic(c.Dir), strings.Join(c.Env, ", "))
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

func inspection(s *PinStore, alias string, prior, candidate pinEntry, id connectionIdentity) *Inspection {
	result := &Inspection{
		Alias:               alias,
		PinPath:             filepath.Join(s.dir, pinKey(alias)+".json"),
		PinnedDigest:        prior.digest(),
		CandidateDigest:     candidate.digest(),
		Diff:                diffCatalogs(prior.toolCatalog, candidate.toolCatalog),
		CandidateConnection: ConnectionView{Fingerprint: candidate.conn.fingerprint, Kind: id.kind, Launcher: id.launcher, Target: id.target, Dir: id.dir, Env: id.env, Origin: id.origin},
		pinned:              prior.toolCatalog,
		candidate:           candidate.toolCatalog,
	}
	if prior.conn != nil {
		result.PinnedConnection = prior.conn.fingerprint
		result.ConnectionChanges = compareConnectionPins(prior.conn, candidate.conn)
	}
	return result
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

// ApprovalDigests binds an approval to both identities the operator reviewed
// with Inspect. Both are required; an empty field is an error, never a
// wildcard.
type ApprovalDigests struct {
	Catalog    string // "sha256:" + 64 lowercase hex
	Connection string // "hmac-sha256:" + 64 lowercase hex
}

// Inspect fetches and validates a complete catalog without writing a pin
// record or invoking a tool. It does launch or contact the candidate: that is
// the explicit operator action. Callers must supply a stable alias.
func Inspect(ctx context.Context, impl Implementation, server Server, pins *PinStore) (*Inspection, error) {
	return inspectOrApprove(ctx, impl, server, pins, hostLaunchEnv(), nil)
}

// Approve checks the connection fingerprint before anything is launched,
// re-fetches the catalog, and replaces the captured pin revision only if both
// digests equal the operator's.
func Approve(ctx context.Context, impl Implementation, server Server, pins *PinStore, want ApprovalDigests) (*Inspection, error) {
	if !digestRE.MatchString(want.Catalog) {
		return nil, errors.New("mcpclient: digest must be sha256: followed by 64 lowercase hex digits")
	}
	if !fingerprintRE.MatchString(want.Connection) {
		return nil, errors.New("mcpclient: connection must be hmac-sha256: followed by 64 lowercase hex digits")
	}
	return inspectOrApprove(ctx, impl, server, pins, hostLaunchEnv(), &want)
}

// inspectOrApprove prepares server under le exactly as Connect would, so the
// fingerprint reviewed is the one Connect checks and the process launched is
// the one Connect launches.
func inspectOrApprove(ctx context.Context, impl Implementation, server Server, pins *PinStore, le launchEnv, want *ApprovalDigests) (result *Inspection, err error) {
	if err = validateTrustConfig([]Server{server}, pins, le.policy); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	prepared, err := prepare(server, pins.workspace, le)
	if err != nil {
		return nil, err
	}
	conn, err := pins.digestConnection(ctx, prepared.identity)
	if err != nil {
		// As in connectOne: an unfingerprintable identity is an unusable launch.
		return nil, admissionFailure(server.Alias, "launch_invalid", err)
	}
	if want != nil && !equalTag(conn.fingerprint, want.Connection) {
		return nil, admissionFailure(server.Alias, "connection_mismatch", nil)
	}
	prior, revision, err := pins.capturePin(ctx, server.Alias)
	if err != nil {
		return nil, admissionFailure(server.Alias, "pin_unavailable", err)
	}
	session, _, candidate, _, err := discover(ctx, impl, prepared)
	if err != nil {
		return nil, err
	}
	// Close discovery before publication so failed cleanup cannot look like an
	// uncommitted approval to the operator after we have changed the pin. A
	// refused redirect on the session DELETE is not a failure: the client side
	// is closed, nothing reached the Location, and Connect admits such a server.
	if err = session.Close(); err != nil && !errors.Is(err, errRedirectRefused) {
		return nil, admissionFailure(server.Alias, "unavailable", err)
	}
	if err = ctx.Err(); err != nil {
		return nil, admissionFailure(server.Alias, "canceled", err)
	}
	entry := pinEntry{toolCatalog: candidate, conn: conn}
	result = inspection(pins, server.Alias, prior, entry, prepared.identity)
	if want != nil {
		if candidate.digest() != want.Catalog {
			failure := admissionFailure(server.Alias, "digest_mismatch", nil)
			failure.PinnedDigest, failure.CandidateDigest, failure.Diff = prior.digest(), candidate.digest(), result.Diff
			return nil, failure
		}
		if err = pins.replacePin(ctx, server.Alias, revision, entry); err != nil {
			return nil, admissionFailure(server.Alias, "pin_unavailable", err)
		}
	}
	return result, nil
}
