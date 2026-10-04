package mcpclient

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	agenttools "github.com/kstruzzieri/go-llm/agent/tools"
	"github.com/kstruzzieri/go-llm/internal/datadir"
	"github.com/kstruzzieri/go-llm/internal/pathguard"
	"github.com/kstruzzieri/go-llm/signing"
)

const (
	// maxPinBytes bounds one persisted pin record. A catalog at every limit
	// (128 tools, 8 KiB descriptions of six-byte JSON escapes, 32 KiB canonical
	// schemas) serializes to about 10 MiB; publication writes the canonical
	// bytes verbatim, so nothing expands past that.
	maxPinBytes = 16 * 1024 * 1024
	// leaseAcquireBudget bounds waiting for a competing holder of the per-alias
	// lease; the default poll interval is leaseRetryInterval. Filesystem I/O
	// after acquisition is bounded only by the caller's context.
	leaseAcquireBudget = 2 * time.Second
	leaseRetryInterval = 10 * time.Millisecond
)

var (
	errPinMissing          = errors.New("mcpclient: no trusted pin")
	errPinMismatch         = errors.New("mcpclient: tool catalog differs from trusted pin")
	errPinRevisionConflict = errors.New("mcpclient: pin changed during approval")
	errPinContention       = errors.New("mcpclient: pin lease contention")
	errPinDurability       = errors.New("mcpclient: pin durability unconfirmed; published bytes may already be present")
)

var errConnectionMissing = errors.New("mcpclient: pin predates connection identity; approval required")

// connectionChangedError reports a changed connection with fixed labels.
type connectionChangedError struct{ labels []string }

func (e *connectionChangedError) Error() string {
	return "mcpclient: connection identity differs from trusted pin"
}

// pinRecordVersion is the record format this binary writes. Version 1
// (catalogFormatVersion) records are read as "connection absent".
const pinRecordVersion = 2

// pinEntry is everything one pin record binds: the complete catalog and the
// connection digests. conn is nil only for a version 1 record read from disk.
type pinEntry struct {
	toolCatalog
	conn *connectionPin
}

type connectionRecord struct {
	Fingerprint string            `json:"fingerprint"`
	KeyID       string            `json:"key_id"`
	Kind        string            `json:"kind"`
	Fields      map[string]string `json:"fields"`
}

func (p *connectionPin) record() *connectionRecord {
	fields := make(map[string]string, len(p.fields))
	for k, v := range p.fields {
		fields[k] = v
	}
	return &connectionRecord{Fingerprint: p.fingerprint, KeyID: p.keyID, Kind: p.kind, Fields: fields}
}

func decodeConnection(r *connectionRecord) (*connectionPin, error) {
	if r == nil {
		return nil, errors.New("pin record lacks a connection")
	}
	names, ok := connectionFieldNames[r.Kind]
	if !ok || !fingerprintRE.MatchString(r.Fingerprint) || !hexTagRE.MatchString(r.KeyID) || len(r.Fields) != len(names) {
		return nil, errors.New("invalid pin connection")
	}
	fields := make(map[string]string, len(names))
	for _, name := range names {
		tag, ok := r.Fields[name]
		if !ok || !hexTagRE.MatchString(tag) {
			return nil, errors.New("invalid pin connection field")
		}
		fields[name] = tag
	}
	return &connectionPin{fingerprint: r.Fingerprint, keyID: r.KeyID, kind: r.Kind, fields: fields}, nil
}

// checkConnection compares a loaded record's connection with a candidate:
// a version 1 record is never evidence of a launch (spec D14).
func checkConnection(current pinEntry, candidate *connectionPin) error {
	if current.conn == nil {
		return errConnectionMissing
	}
	if labels := compareConnectionPins(current.conn, candidate); len(labels) > 0 {
		return &connectionChangedError{labels: labels}
	}
	return nil
}

func validatePinEntry(alias string, e pinEntry) error {
	if e.conn == nil {
		return errors.New("mcpclient: pin candidate lacks a connection identity")
	}
	return validatePinCatalog(alias, e.toolCatalog)
}

// PinStore persists tool trust independently for each workspace and server alias.
// Its immutable configuration permits concurrent operations; each operation owns
// its directory and OS lease handles. Construct one with NewPinStore.
type PinStore struct {
	workspace string
	base      string
	dir       string
	// connKey fingerprints connection identities; one key per user data dir.
	connKey *signing.HMACSigner
	// Private per-instance seams keep deterministic fault tests isolated.
	ops pinFileOps
}

type pinRevision struct {
	exists bool
	hash   [sha256.Size]byte
}
type pinRecord struct {
	Version    int               `json:"version"`
	Workspace  string            `json:"workspace"`
	Alias      string            `json:"alias"`
	Digest     string            `json:"digest"`
	Tools      json.RawMessage   `json:"tools"`
	Connection *connectionRecord `json:"connection,omitempty"`
}

// NewPinStore selects the canonical workspace trust namespace under the user
// data directory. Its private storage must be outside the workspace.
func NewPinStore(workspace string) (*PinStore, error) {
	base, err := datadir.Base(os.Getenv)
	if err != nil {
		return nil, err
	}
	return newPinStore(workspace, base)
}

func newPinStore(workspace, base string) (*PinStore, error) {
	canonical, err := agenttools.CanonicalWorkspaceRoot(workspace)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(canonical)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, errors.New("mcpclient: workspace is not a directory")
	}
	base, err = resolvePinBase(base)
	if err != nil {
		return nil, err
	}
	s := &PinStore{workspace: canonical, base: base, dir: filepath.Join(base, "golem", "mcp-pins", pinKey(canonical)), ops: defaultPinFileOps()}
	if err = pathguard.ValidateOutside(s.dir, canonical); err != nil {
		return nil, err
	}
	root, err := s.openRoot(true)
	if err != nil {
		return nil, err
	}
	if err = root.Close(); err != nil {
		return nil, err
	}
	// The connection key lives beside the per-workspace pin directories, in
	// the private golem/mcp-pins directory openRoot just created. A missing key
	// is created; an unreadable or corrupt one fails here and is never
	// regenerated (spec §5.6).
	keyPath := filepath.Join(base, "golem", "mcp-pins", connectionKeyFile)
	if err = pathguard.ValidateOutside(keyPath, canonical); err != nil {
		return nil, err
	}
	if s.connKey, _, err = signing.LoadOrCreateHMAC(keyPath); err != nil {
		return nil, err
	}
	return s, nil
}

func pinKey(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func checkPinAlias(alias string) error {
	if !validAlias(alias) {
		return fmt.Errorf("mcpclient: invalid pin alias %q", alias)
	}
	return nil
}

// admitWith runs one admission under a single alias lease. guard decides from
// the current revision before the record is decoded, compared or published;
// an existing record must then match both identities, and an absent one is
// published.
func (s *PinStore) admitWith(ctx context.Context, alias string, candidate pinEntry, guard func(pinRevision) error) (prior pinEntry, created bool, err error) {
	if err = checkPinAlias(alias); err != nil {
		return
	}
	if err = validatePinEntry(alias, candidate); err != nil {
		return
	}
	err = s.withLease(ctx, alias, func(root *os.Root, name string) error {
		current, rev, err := s.load(root, name, alias, guard)
		if err != nil {
			return err
		}
		prior = current
		if err = ctx.Err(); err != nil {
			return err
		}
		if rev.exists {
			if err = checkConnection(current, candidate.conn); err != nil {
				return err
			}
			if current.digest() != candidate.digest() {
				return errPinMismatch
			}
			return nil
		}
		if err = s.publish(ctx, root, name, alias, candidate); err != nil {
			return err
		}
		created = true
		return nil
	})
	return
}

// admitAt admits at the revision preflight captured (spec §5.8 step 5): any
// change since then, deletion or undecodable bytes included, is a revision
// conflict. A revision is existence plus a hash of the raw bytes, so a
// byte-identical delete and re-create is indistinguishable and admits the
// record preflight approved.
func (s *PinStore) admitAt(ctx context.Context, alias string, prior pinRevision, candidate pinEntry) (pinEntry, bool, error) {
	return s.admitWith(ctx, alias, candidate, sameRevision(prior))
}

// sameRevision is a load check that refuses any change since prior.
func sameRevision(prior pinRevision) func(pinRevision) error {
	return func(current pinRevision) error {
		if current != prior {
			return errPinRevisionConflict
		}
		return nil
	}
}

func (s *PinStore) capturePin(ctx context.Context, alias string) (entry pinEntry, revision pinRevision, err error) {
	if err = checkPinAlias(alias); err != nil {
		return
	}
	err = s.withLease(ctx, alias, func(root *os.Root, name string) error {
		var e error
		entry, revision, e = s.load(root, name, alias, nil)
		if e != nil {
			return e
		}
		return ctx.Err()
	})
	return
}

func (s *PinStore) replacePin(ctx context.Context, alias string, prior pinRevision, candidate pinEntry) error {
	if err := checkPinAlias(alias); err != nil {
		return err
	}
	if err := validatePinEntry(alias, candidate); err != nil {
		return err
	}
	return s.withLease(ctx, alias, func(root *os.Root, name string) error {
		if _, _, err := s.load(root, name, alias, sameRevision(prior)); err != nil {
			return err
		}
		return s.publish(ctx, root, name, alias, candidate)
	})
}

func (s *PinStore) withLease(ctx context.Context, alias string, run func(*os.Root, string) error) (err error) {
	if err = ctx.Err(); err != nil {
		return err
	}
	root, err := s.openRoot(false)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	name := pinKey(alias)
	// This budget applies to acquisition only, not filesystem I/O after it.
	// Its own expiry is reported as contention: only the caller's context may
	// classify a failure as cancellation.
	budget, cancel := context.WithTimeout(ctx, leaseAcquireBudget)
	defer cancel()
	var lease *pinLease
	for {
		lease, err = s.acquireLease(root, name+".lock")
		if err == nil {
			break
		}
		if err != errPinContention {
			return err
		}
		if err = s.ops.wait(budget); err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return errors.Join(errPinContention, ctxErr)
			}
			return errPinContention
		}
	}
	defer func() { err = errors.Join(err, lease.Close()) }()
	if err = ctx.Err(); err != nil {
		return err
	}
	return errors.Join(run(root, name+".json"), ctx.Err())
}

// load reads, hashes and decodes one record. check, when non-nil, sees the
// revision before decoding, so a record replaced by bytes that do not decode
// is still classified by its revision (spec §5.8 step 5) rather than as an
// invalid pin.
func (s *PinStore) load(root *os.Root, name, alias string, check func(pinRevision) error) (pinEntry, pinRevision, error) {
	raw, exists, err := s.read(root, name)
	if err != nil {
		return pinEntry{}, pinRevision{}, err
	}
	var rev pinRevision
	if exists {
		rev = pinRevision{exists: true, hash: sha256.Sum256(raw)}
	}
	if check != nil {
		if err = check(rev); err != nil {
			return pinEntry{}, pinRevision{}, err
		}
	}
	if !exists {
		return pinEntry{}, pinRevision{}, nil
	}
	c, err := decodePin(raw, s.workspace, alias)
	if err != nil {
		return pinEntry{}, pinRevision{}, fmt.Errorf("mcpclient: invalid pin %s: %w", alias, err)
	}
	// A prior writer may have renamed successfully but failed its directory sync.
	if err = s.ops.syncDir(root); err != nil {
		return pinEntry{}, pinRevision{}, errors.Join(errPinDurability, err)
	}
	return c, rev, nil
}

func decodePin(raw []byte, workspace, alias string) (pinEntry, error) {
	canonical, err := signing.Canonicalize(raw)
	if err != nil {
		return pinEntry{}, err
	}
	// encoding/json matches struct fields case-insensitively. The persisted
	// protocol instead requires each version's exact keys, without extensions.
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(canonical, &fields); err != nil {
		return pinEntry{}, err
	}
	var version int
	if err = json.Unmarshal(fields["version"], &version); err != nil {
		return pinEntry{}, errors.New("invalid pin record version")
	}
	keys := []string{"version", "workspace", "alias", "digest", "tools"}
	switch version {
	case catalogFormatVersion:
	case pinRecordVersion:
		keys = append(keys, "connection")
	default:
		return pinEntry{}, errors.New("unsupported pin record version")
	}
	if len(fields) != len(keys) {
		return pinEntry{}, errors.New("invalid pin record fields")
	}
	for _, key := range keys {
		if _, ok := fields[key]; !ok {
			return pinEntry{}, fmt.Errorf("missing pin record field %s", key)
		}
	}
	if version == pinRecordVersion {
		// Struct decoding matches keys case-insensitively, so the nested
		// object's exact key set is checked here, as the top level's is above.
		var conn map[string]json.RawMessage
		if err = json.Unmarshal(fields["connection"], &conn); err != nil || len(conn) != 4 {
			return pinEntry{}, errors.New("invalid pin connection fields")
		}
		for _, key := range []string{"fingerprint", "key_id", "kind", "fields"} {
			if _, ok := conn[key]; !ok {
				return pinEntry{}, fmt.Errorf("missing pin connection field %s", key)
			}
		}
	}
	dec := json.NewDecoder(bytes.NewReader(canonical))
	dec.DisallowUnknownFields()
	var record pinRecord
	if err = dec.Decode(&record); err != nil {
		return pinEntry{}, err
	}
	if record.Workspace != workspace || record.Alias != alias {
		return pinEntry{}, errors.New("record identity or version mismatch")
	}
	var entries []catalogEntry
	dec = json.NewDecoder(bytes.NewReader(record.Tools))
	dec.DisallowUnknownFields()
	if err = dec.Decode(&entries); err != nil {
		return pinEntry{}, err
	}
	c, err := newToolCatalog(entries)
	if err != nil {
		return pinEntry{}, err
	}
	if !bytes.Equal(record.Tools, c.canonicalBytes()) || record.Digest != c.digest() {
		return pinEntry{}, errors.New("noncanonical catalog or digest mismatch")
	}
	if err = validatePinCatalog(alias, c); err != nil {
		return pinEntry{}, err
	}
	entry := pinEntry{toolCatalog: c}
	if version == pinRecordVersion {
		if entry.conn, err = decodeConnection(record.Connection); err != nil {
			return pinEntry{}, err
		}
	}
	return entry, nil
}

func validatePinCatalog(alias string, c toolCatalog) error {
	if len(c.entries) > maxToolsPerServer || len(c.canonical) == 0 {
		return errors.New("mcpclient: invalid pin catalog")
	}
	previous := ""
	prefix := "mcp__" + alias + "__"
	for _, e := range c.entries {
		remote, ok := strings.CutPrefix(e.Name, prefix)
		if !ok || remote == "" {
			return errors.New("mcpclient: catalog alias mismatch")
		}
		name, ok := composeName(alias, remote)
		if !ok || name != e.Name || e.Name <= previous {
			return errors.New("mcpclient: invalid or duplicate catalog name")
		}
		desc, truncated := normalizeDescription(e.Description)
		if truncated || desc != e.Description {
			return errors.New("mcpclient: nonnormalized catalog description")
		}
		if len(e.InputSchema) > maxSchemaBytes {
			return errors.New("mcpclient: catalog schema exceeds limit")
		}
		previous = e.Name
	}
	return nil
}
