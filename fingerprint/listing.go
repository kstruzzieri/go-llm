package fingerprint

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// refusedAccessTTL is how long a refused access row stays a current claim.
const refusedAccessTTL = 7 * 24 * time.Hour

// legacyToolCall is the only capability the legacy capability_probes table held.
const legacyToolCall = "tool_call"

// ListingMember is one model a provider listed.
type ListingMember struct{ Model, OwnedBy, Digest string }

// ListingPublish is one provider's successful listing, with the facts the
// provider reported alongside it. It is stored for the account derived from
// APIKey; the key itself is never stored.
type ListingPublish struct {
	Endpoint  string
	Adapter   string // required: one endpoint reached through two adapters keeps two listings
	APIKey    string `json:"-"` // the raw key; String, GoString and JSON leave it out
	StartedAt time.Time
	Members   []ListingMember
	Facts     []EvidenceRow
}

// String describes the publish without its credential.
func (p ListingPublish) String() string {
	return fmt.Sprintf("ListingPublish{Endpoint:%q Adapter:%q StartedAt:%s Members:%d Facts:%d APIKey:[redacted]}",
		p.Endpoint, p.Adapter, p.StartedAt.Format(time.RFC3339Nano), len(p.Members), len(p.Facts))
}

// GoString is String, so %#v does not print the credential either.
func (p ListingPublish) GoString() string { return p.String() }

// ListingSnapshot is the current listing of an endpoint for one credential.
type ListingSnapshot struct {
	Endpoint               string
	StartedAt, CommittedAt time.Time
	Members                []ListingMember
}

// NamedModel is a model the user named for an endpoint whose listing is
// unsupported or omits it. It is never part of a snapshot.
type NamedModel struct {
	Model   string
	AddedAt time.Time
}

// AccessState is what a call to a model showed about a credential's access.
type AccessState string

const (
	// AccessAnswered means the credential could call the model.
	AccessAnswered AccessState = "answered"
	// AccessRefused means the endpoint refused the credential for the model.
	AccessRefused AccessState = "refused"
)

// ModelAccess is the newest access claim for a model under one credential.
type ModelAccess struct {
	Model                 string
	State                 AccessState
	HTTPStatus            int
	ObservedAt, ExpiresAt time.Time // ExpiresAt is zero when it does not expire
}

// EvidenceSnapshot is one consistent read: every field comes from a single
// transaction. The maps are keyed by model.
type EvidenceSnapshot struct {
	Generation int64
	Listing    *ListingSnapshot // nil when none for the credential, or none asked for
	Named      []NamedModel
	Rows       []EvidenceRow
	Legacy     map[string]*CapProbe
	Access     map[string]*ModelAccess
}

// EvidenceReader is the read side of the evidence store.
type EvidenceReader interface {
	ReadSnapshot(ctx context.Context, endpoint, adapter string, apiKey *string, legacyProvider string, models []string, now time.Time) (EvidenceSnapshot, error)
	EvidenceRows(ctx context.Context, endpoint, model string) ([]EvidenceRow, error)
	GetCapProbe(ctx context.Context, backendID, modelName, capability string) (*CapProbe, error) // legacy history
	CurrentListing(ctx context.Context, endpoint, adapter, apiKey string) (*ListingSnapshot, error)
	NamedModels(ctx context.Context, endpoint string) ([]NamedModel, error)
	LatestModelAccess(ctx context.Context, endpoint, apiKey, model string) (*ModelAccess, error)
	Generation(ctx context.Context) (int64, error)
}

// EvidenceWriter is the write side of the evidence store.
type EvidenceWriter interface {
	AppendEvidence(ctx context.Context, rows ...EvidenceRow) error
	AppendCheckResult(ctx context.Context, endpoint, apiKey, model string, rows []EvidenceRow, access *ModelAccess) error
	PublishListings(ctx context.Context, pubs []ListingPublish) error
	AddNamedModel(ctx context.Context, endpoint, model string) error
	RemoveNamedModel(ctx context.Context, endpoint, model string) error
	RecordModelAccess(ctx context.Context, endpoint, apiKey, model string, state AccessState, status int, at time.Time) error
}

// EvidenceStore is the whole evidence store.
type EvidenceStore interface {
	EvidenceReader
	EvidenceWriter
}

var _ EvidenceStore = (*SQLiteStore)(nil)

var errUnresolvedCredential = errors.New("credential is not resolved")

// unresolved reports a credential that still holds a ${...} placeholder.
func unresolved(apiKey string) bool { return strings.Contains(apiKey, "${") }

// account is the HMAC-SHA256 of the endpoint, a NUL and the key, keyed by the
// store's secret, hex encoded; empty when there is no key. It never leaves the
// package: listings and access are per account, and nothing exported carries it.
func (s *SQLiteStore) account(endpoint, apiKey string) string {
	if apiKey == "" {
		return ""
	}
	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte(endpoint))
	mac.Write([]byte{0})
	mac.Write([]byte(apiKey))
	return hex.EncodeToString(mac.Sum(nil))
}

// queryer is what a read helper needs; both *sql.DB and *sql.Tx have it.
type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// PublishListings stores each listing as its endpoint-adapter-and-account's current
// snapshot, with its facts, in one transaction. A snapshot replaces the current
// one only when its StartedAt is newer; the older snapshot and its members go in
// the same transaction (a stored snapshot dated beyond the clock-skew allowance
// does not count as current, so a real publish replaces it), and a late older publish, facts included, inserts
// nothing. A StartedAt beyond the skew allowance is refused. An empty Members is an empty listing, recorded as such. Members
// repeating a model keep the first. An empty Adapter is refused. A credential holding an unresolved ${...}
// is refused.
func (s *SQLiteStore) PublishListings(ctx context.Context, pubs []ListingPublish) error {
	if len(pubs) == 0 {
		return nil
	}
	now := time.Now()
	for _, p := range pubs {
		if p.Endpoint == "" || p.Adapter == "" || p.StartedAt.IsZero() {
			return errors.New("fingerprint: publish listings: listing needs an endpoint, an adapter and a start time")
		}
		if p.StartedAt.After(now.Add(maxClockSkew)) {
			return errors.New("fingerprint: publish listings: listing is dated in the future")
		}
		if unresolved(p.APIKey) {
			return fmt.Errorf("fingerprint: publish listings: %w", errUnresolvedCredential)
		}
		if slices.ContainsFunc(p.Members, func(m ListingMember) bool { return m.Model == "" }) {
			return errors.New("fingerprint: publish listings: member has an empty model")
		}
		if err := validateEvidenceRows(p.Facts); err != nil {
			return fmt.Errorf("fingerprint: publish listings: %w", err)
		}
	}
	err := s.writeTx(ctx, func(tx *sql.Tx) error {
		for _, p := range pubs {
			if err := s.publishListing(ctx, tx, p, now); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("fingerprint: publish listings: %w", err)
	}
	return nil
}

// publishListing runs inside the write transaction, whose generation bump took
// the write lock first, so the started_at it reads cannot go stale before it
// writes.
func (s *SQLiteStore) publishListing(ctx context.Context, tx *sql.Tx, p ListingPublish, now time.Time) error {
	account := s.account(p.Endpoint, p.APIKey)
	var current sql.NullInt64
	err := tx.QueryRowContext(ctx,
		`SELECT MAX(started_at) FROM listing_snapshots
		WHERE key_version = ? AND endpoint = ? AND adapter = ? AND account = ? AND started_at <= ?`,
		EvidenceKeyVersion, p.Endpoint, p.Adapter, account, now.Add(maxClockSkew).UnixMilli()).Scan(&current)
	if err != nil {
		return fmt.Errorf("read current listing: %w", err)
	}
	if current.Valid && current.Int64 >= p.StartedAt.UnixMilli() {
		return nil
	}
	for _, q := range []string{
		`DELETE FROM listing_members WHERE snapshot_id IN
			(SELECT id FROM listing_snapshots WHERE key_version = ? AND endpoint = ? AND adapter = ? AND account = ?)`,
		`DELETE FROM listing_snapshots WHERE key_version = ? AND endpoint = ? AND adapter = ? AND account = ?`,
	} {
		if _, err := tx.ExecContext(ctx, q, EvidenceKeyVersion, p.Endpoint, p.Adapter, account); err != nil {
			return fmt.Errorf("replace listing: %w", err)
		}
	}
	res, err := tx.ExecContext(ctx,
		`INSERT INTO listing_snapshots (key_version, endpoint, adapter, account, started_at, committed_at) VALUES (?, ?, ?, ?, ?, ?)`,
		EvidenceKeyVersion, p.Endpoint, p.Adapter, account, p.StartedAt.UnixMilli(), time.Now().UnixMilli())
	if err != nil {
		return fmt.Errorf("insert listing: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return fmt.Errorf("insert listing: %w", err)
	}
	for _, m := range p.Members {
		if _, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO listing_members (snapshot_id, model_name, owned_by, model_digest) VALUES (?, ?, ?, ?)`,
			id, m.Model, m.OwnedBy, m.Digest); err != nil {
			return fmt.Errorf("insert listing member: %w", err)
		}
	}
	return insertEvidence(ctx, tx, p.Facts)
}

// CurrentListing returns the endpoint's current listing through the adapter for the credential, or
// nil when there is none for it, or the credential is unresolved.
func (s *SQLiteStore) CurrentListing(ctx context.Context, endpoint, adapter, apiKey string) (*ListingSnapshot, error) {
	if unresolved(apiKey) {
		return nil, nil
	}
	// One transaction: the snapshot row and its members must come from one commit.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("fingerprint: current listing %q: begin: %w", endpoint, err)
	}
	defer func() { _ = tx.Rollback() }()
	l, err := readListing(ctx, tx, endpoint, adapter, s.account(endpoint, apiKey), time.Now())
	if err != nil {
		return nil, fmt.Errorf("fingerprint: current listing %q: %w", endpoint, err)
	}
	return l, nil
}

// readListing reads the newest snapshot not dated beyond the skew allowance at now.
func readListing(ctx context.Context, q queryer, endpoint, adapter, account string, now time.Time) (*ListingSnapshot, error) {
	var id, startedAt, committedAt int64
	err := q.QueryRowContext(ctx,
		`SELECT id, started_at, committed_at FROM listing_snapshots
		WHERE key_version = ? AND endpoint = ? AND adapter = ? AND account = ? AND started_at <= ?
		ORDER BY started_at DESC, id DESC LIMIT 1`,
		EvidenceKeyVersion, endpoint, adapter, account, now.Add(maxClockSkew).UnixMilli()).Scan(&id, &startedAt, &committedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	l := &ListingSnapshot{Endpoint: endpoint, StartedAt: time.UnixMilli(startedAt), CommittedAt: time.UnixMilli(committedAt)}
	rows, err := q.QueryContext(ctx,
		`SELECT model_name, owned_by, model_digest FROM listing_members WHERE snapshot_id = ? ORDER BY model_name`, id)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var m ListingMember
		if err := rows.Scan(&m.Model, &m.OwnedBy, &m.Digest); err != nil {
			return nil, err
		}
		l.Members = append(l.Members, m)
	}
	return l, rows.Err()
}

// AddNamedModel names a model for an endpoint. Naming it again keeps the first
// AddedAt.
func (s *SQLiteStore) AddNamedModel(ctx context.Context, endpoint, model string) error {
	if endpoint == "" || model == "" {
		return errors.New("fingerprint: add named model: endpoint and model are required")
	}
	err := s.writeTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO named_models (key_version, endpoint, model_name, added_at) VALUES (?, ?, ?, ?)`,
			EvidenceKeyVersion, endpoint, model, time.Now().UnixMilli())
		return err
	})
	if err != nil {
		return fmt.Errorf("fingerprint: add named model %q/%q: %w", endpoint, model, err)
	}
	return nil
}

// RemoveNamedModel un-names a model; one that was not named is not an error.
func (s *SQLiteStore) RemoveNamedModel(ctx context.Context, endpoint, model string) error {
	err := s.writeTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`DELETE FROM named_models WHERE key_version = ? AND endpoint = ? AND model_name = ?`,
			EvidenceKeyVersion, endpoint, model)
		return err
	})
	if err != nil {
		return fmt.Errorf("fingerprint: remove named model %q/%q: %w", endpoint, model, err)
	}
	return nil
}

// NamedModels returns the endpoint's named models, ordered by name.
func (s *SQLiteStore) NamedModels(ctx context.Context, endpoint string) ([]NamedModel, error) {
	out, err := readNamed(ctx, s.db, endpoint)
	if err != nil {
		return nil, fmt.Errorf("fingerprint: named models %q: %w", endpoint, err)
	}
	return out, nil
}

func readNamed(ctx context.Context, q queryer, endpoint string) (out []NamedModel, err error) {
	rows, err := q.QueryContext(ctx,
		`SELECT model_name, added_at FROM named_models WHERE key_version = ? AND endpoint = ? ORDER BY model_name`,
		EvidenceKeyVersion, endpoint)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	for rows.Next() {
		var n NamedModel
		var addedAt int64
		if err := rows.Scan(&n.Model, &addedAt); err != nil {
			return nil, err
		}
		n.AddedAt = time.UnixMilli(addedAt)
		out = append(out, n)
	}
	return out, rows.Err()
}

// RecordModelAccess records what a call to the model showed about the
// credential's access. A refusal expires after 7 days; an answer does not.
func (s *SQLiteStore) RecordModelAccess(ctx context.Context, endpoint, apiKey, model string, state AccessState, status int, at time.Time) error {
	access := ModelAccess{Model: model, State: state, HTTPStatus: status, ObservedAt: at}
	if err := validateAccess(endpoint, apiKey, &access); err != nil {
		return fmt.Errorf("fingerprint: record model access: %w", err)
	}
	err := s.writeTx(ctx, func(tx *sql.Tx) error {
		return s.insertAccess(ctx, tx, endpoint, apiKey, access)
	})
	if err != nil {
		return fmt.Errorf("fingerprint: record model access: %w", err)
	}
	return nil
}

// validateAccess checks an access record before any transaction opens and fills
// in the expiry a refusal must have.
func validateAccess(endpoint, apiKey string, a *ModelAccess) error {
	switch {
	case endpoint == "" || a.Model == "":
		return errors.New("endpoint and model are required")
	case a.State != AccessAnswered && a.State != AccessRefused:
		return errors.New("access has an unknown state")
	case a.ObservedAt.IsZero():
		return errors.New("access needs an observation time")
	case unresolved(apiKey):
		return errUnresolvedCredential
	}
	// The expiry is a rule, not the caller's choice.
	a.ExpiresAt = time.Time{}
	if a.State == AccessRefused {
		a.ExpiresAt = a.ObservedAt.Add(refusedAccessTTL)
	}
	return nil
}

func (s *SQLiteStore) insertAccess(ctx context.Context, tx *sql.Tx, endpoint, apiKey string, a ModelAccess) error {
	var status any
	if a.HTTPStatus != 0 {
		status = a.HTTPStatus
	}
	_, err := tx.ExecContext(ctx,
		`INSERT INTO model_access (key_version, endpoint, account, model_name, state, http_status, observed_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		EvidenceKeyVersion, endpoint, s.account(endpoint, apiKey), a.Model, string(a.State), status,
		a.ObservedAt.UnixMilli(), millisOrNull(a.ExpiresAt))
	if err != nil {
		return fmt.Errorf("insert access %q/%q: %w", endpoint, a.Model, err)
	}
	return nil
}

// LatestModelAccess returns the credential's newest access row for the model, or
// nil when there is none (rows of an unknown state or dated beyond the skew
// allowance are not claims), it has expired, or the credential is unresolved. An
// expired newest row means no claim; it does not bring back an older row.
func (s *SQLiteStore) LatestModelAccess(ctx context.Context, endpoint, apiKey, model string) (*ModelAccess, error) {
	if unresolved(apiKey) {
		return nil, nil
	}
	a, err := readAccess(ctx, s.db, endpoint, s.account(endpoint, apiKey), model, time.Now())
	if err != nil {
		return nil, fmt.Errorf("fingerprint: latest model access %q/%q: %w", endpoint, model, err)
	}
	return a, nil
}

func readAccess(ctx context.Context, q queryer, endpoint, account, model string, now time.Time) (*ModelAccess, error) {
	var a ModelAccess
	var status, expiresAt sql.NullInt64
	var observedAt int64
	err := q.QueryRowContext(ctx,
		`SELECT state, http_status, observed_at, expires_at FROM model_access
		WHERE key_version = ? AND endpoint = ? AND account = ? AND model_name = ?
			AND state IN ('answered', 'refused') AND observed_at <= ?
		ORDER BY observed_at DESC, id DESC LIMIT 1`,
		EvidenceKeyVersion, endpoint, account, model, now.Add(maxClockSkew).UnixMilli()).Scan(&a.State, &status, &observedAt, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	a.Model, a.HTTPStatus, a.ObservedAt, a.ExpiresAt = model, int(status.Int64), time.UnixMilli(observedAt), timeOrZero(expiresAt)
	if !a.ExpiresAt.IsZero() && now.After(a.ExpiresAt) {
		return nil, nil
	}
	return &a, nil
}

// ReadSnapshot reads the generation, the credential's listing, the named models
// and, for each model asked about, its evidence rows, legacy history and latest
// access in one read transaction, so a concurrent publish is wholly before or
// after it. A nil apiKey skips the account reads; a pointer to "" is the keyless
// credential; a credential holding an unresolved ${...} reads no listing or
// access, but evidence and named models stay readable. No models reads only the
// listing and named models. legacyProvider names the old capability_probes
// label; empty skips legacy history. Expiry is decided at now. The transaction
// has ended when this returns, and everything returned is a copy.
func (s *SQLiteStore) ReadSnapshot(ctx context.Context, endpoint, adapter string, apiKey *string, legacyProvider string, models []string, now time.Time) (snap EvidenceSnapshot, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return EvidenceSnapshot{}, fmt.Errorf("fingerprint: read snapshot: begin: %w", err)
	}
	defer func() {
		if cErr := tx.Rollback(); cErr != nil && !errors.Is(cErr, sql.ErrTxDone) {
			err = errors.Join(err, cErr)
		}
	}()
	fail := func(e error) (EvidenceSnapshot, error) {
		return EvidenceSnapshot{}, fmt.Errorf("fingerprint: read snapshot %q: %w", endpoint, e)
	}
	snap.Legacy = make(map[string]*CapProbe)
	snap.Access = make(map[string]*ModelAccess)
	// Generation first: a read transaction's snapshot is fixed by its first read.
	if err := tx.QueryRowContext(ctx, `SELECT generation FROM store_meta`).Scan(&snap.Generation); err != nil {
		return fail(err)
	}
	account, haveAccount := "", false
	if apiKey != nil && !unresolved(*apiKey) {
		account, haveAccount = s.account(endpoint, *apiKey), true
	}
	if haveAccount {
		if snap.Listing, err = readListing(ctx, tx, endpoint, adapter, account, now); err != nil {
			return fail(err)
		}
	}
	if snap.Named, err = readNamed(ctx, tx, endpoint); err != nil {
		return fail(err)
	}
	seen := make(map[string]bool, len(models))
	for _, model := range models {
		if seen[model] {
			continue
		}
		seen[model] = true
		rows, err := tx.QueryContext(ctx,
			`SELECT `+evidenceColumns+` FROM capability_evidence
			WHERE key_version = ? AND endpoint = ? AND model_name = ? ORDER BY id`,
			EvidenceKeyVersion, endpoint, model)
		if err != nil {
			return fail(err)
		}
		got, err := scanEvidence(rows)
		if err != nil {
			return fail(err)
		}
		snap.Rows = append(snap.Rows, got...)
		if legacyProvider != "" {
			if p, err := readLegacy(ctx, tx, legacyProvider, model); err != nil {
				return fail(err)
			} else if p != nil {
				snap.Legacy[model] = p
			}
		}
		if haveAccount {
			if a, err := readAccess(ctx, tx, endpoint, account, model, now); err != nil {
				return fail(err)
			} else if a != nil {
				snap.Access[model] = a
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return fail(err)
	}
	return snap, nil
}

func readLegacy(ctx context.Context, q queryer, backendID, model string) (*CapProbe, error) {
	var p CapProbe
	var testedAt int64
	var expiresAt sql.NullInt64
	err := q.QueryRowContext(ctx,
		`SELECT backend_id, model_name, capability, state, model_digest, probe_version, tested_at, expires_at
		FROM capability_probes WHERE backend_id = ? AND model_name = ? AND capability = ?`,
		backendID, model, legacyToolCall).Scan(&p.BackendID, &p.ModelName, &p.Capability, &p.State,
		&p.ModelDigest, &p.ProbeVersion, &testedAt, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	p.TestedAt, p.ExpiresAt = time.UnixMilli(testedAt), timeOrZero(expiresAt)
	return &p, nil
}

// AppendCheckResult appends a check's capability rows and its access record in
// one transaction with one generation bump. Every row, and the access record,
// must belong to endpoint and model, or nothing is written. It takes access-only
// refusals (no rows) and evidence-only checks (nil access); both empty writes
// nothing. A failure or cancellation before the commit rolls everything back,
// generation included; the commit is the publication boundary.
func (s *SQLiteStore) AppendCheckResult(ctx context.Context, endpoint, apiKey, model string, rows []EvidenceRow, access *ModelAccess) error {
	if len(rows) == 0 && access == nil {
		return nil
	}
	if endpoint == "" || model == "" {
		return errors.New("fingerprint: append check result: endpoint and model are required")
	}
	for _, r := range rows {
		if r.Endpoint != endpoint || r.Model != model {
			return errors.New("fingerprint: append check result: a row belongs to another endpoint or model")
		}
	}
	if err := validateEvidenceRows(rows); err != nil {
		return fmt.Errorf("fingerprint: append check result: %w", err)
	}
	var a ModelAccess
	if access != nil {
		a = *access
		if a.Model != model {
			return errors.New("fingerprint: append check result: the access record belongs to another model")
		}
		if err := validateAccess(endpoint, apiKey, &a); err != nil {
			return fmt.Errorf("fingerprint: append check result: %w", err)
		}
	}
	err := s.writeTx(ctx, func(tx *sql.Tx) error {
		if err := insertEvidence(ctx, tx, rows); err != nil {
			return err
		}
		if access != nil {
			return s.insertAccess(ctx, tx, endpoint, apiKey, a)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("fingerprint: append check result: %w", err)
	}
	return nil
}

// accessRow is the part of a model_access row Prune decides on.
type accessRow struct {
	id         int64
	state      string
	partition  accessPartition
	observedAt int64
}

type accessPartition struct {
	keyVersion               int
	endpoint, account, model string
}

// counts reports whether the row can be a partition's newest claim at now: a
// known state, dated no later than now. A row dated inside the skew allowance
// does not qualify yet, because a reader behind now does not admit it (the
// defect class of commit 6cade156).
func (r accessRow) counts(now time.Time) bool {
	return (r.state == string(AccessAnswered) || r.state == string(AccessRefused)) && r.observedAt <= now.UnixMilli()
}

// prunableAccessIDs returns, sorted, the access rows older than the retention
// window that are not the newest of their account and model. The newest stays
// however old: an expired latest row means no claim, and deleting it would let
// an older row speak.
func prunableAccessIDs(rows []accessRow, now time.Time) []int64 {
	newest := make(map[accessPartition]accessRow)
	for _, r := range rows {
		if !r.counts(now) {
			continue
		}
		if n, ok := newest[r.partition]; !ok || r.observedAt > n.observedAt || (r.observedAt == n.observedAt && r.id > n.id) {
			newest[r.partition] = r
		}
	}
	cutoff := now.Add(-evidenceRetention).UnixMilli()
	var ids []int64
	for _, r := range rows {
		if r.state != string(AccessAnswered) && r.state != string(AccessRefused) {
			continue // another binary's row: never deleted
		}
		if n, ok := newest[r.partition]; ok && r.observedAt < cutoff && n.id != r.id {
			ids = append(ids, r.id)
		}
	}
	slices.Sort(ids)
	return ids
}

func (s *SQLiteStore) readAccessRows(ctx context.Context) (out []accessRow, err error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, state, key_version, endpoint, account, model_name, observed_at FROM model_access`)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	for rows.Next() {
		var r accessRow
		p := &r.partition
		if err := rows.Scan(&r.id, &r.state, &p.keyVersion, &p.endpoint, &p.account, &p.model, &r.observedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
