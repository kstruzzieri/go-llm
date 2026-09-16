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

const maxPinBytes = 16 * 1024 * 1024

var (
	errPinMissing          = errors.New("mcpclient: no trusted pin")
	errPinMismatch         = errors.New("mcpclient: tool catalog differs from trusted pin")
	errPinRevisionConflict = errors.New("mcpclient: pin changed during approval")
	errPinContention       = errors.New("mcpclient: pin lease contention")
	errPinDurability       = errors.New("mcpclient: pin durability unconfirmed; published bytes may already be present")
)

// PinStore persists tool trust independently for each workspace and server alias.
// Its immutable configuration permits concurrent operations; each operation owns
// its directory and OS lease handles. Construct one with NewPinStore.
type PinStore struct {
	workspace string
	base      string
	dir       string
	// Private per-instance seams keep deterministic fault tests isolated.
	ops pinFileOps
}

type pinRevision struct {
	exists bool
	hash   [sha256.Size]byte
}
type pinRecord struct {
	Version   int             `json:"version"`
	Workspace string          `json:"workspace"`
	Alias     string          `json:"alias"`
	Digest    string          `json:"digest"`
	Tools     json.RawMessage `json:"tools"`
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

func (s *PinStore) admit(ctx context.Context, alias string, candidate toolCatalog, requirePinned bool) (prior toolCatalog, created bool, err error) {
	if err = checkPinAlias(alias); err != nil {
		return
	}
	if err = validatePinCatalog(alias, candidate); err != nil {
		return
	}
	err = s.withLease(ctx, alias, func(root *os.Root, name string) error {
		current, rev, err := s.load(root, name, alias)
		if err != nil {
			return err
		}
		prior = current
		if err = ctx.Err(); err != nil {
			return err
		}
		if rev.exists {
			if current.digest() != candidate.digest() {
				return errPinMismatch
			}
			return nil
		}
		if requirePinned {
			return errPinMissing
		}
		if err = s.publish(ctx, root, name, alias, candidate); err != nil {
			return err
		}
		created = true
		return nil
	})
	return
}

func (s *PinStore) capturePin(ctx context.Context, alias string) (catalog toolCatalog, revision pinRevision, err error) {
	if err = checkPinAlias(alias); err != nil {
		return
	}
	err = s.withLease(ctx, alias, func(root *os.Root, name string) error {
		var e error
		catalog, revision, e = s.load(root, name, alias)
		if e != nil {
			return e
		}
		return ctx.Err()
	})
	return
}

func (s *PinStore) replacePin(ctx context.Context, alias string, prior pinRevision, candidate toolCatalog) error {
	if err := checkPinAlias(alias); err != nil {
		return err
	}
	if err := validatePinCatalog(alias, candidate); err != nil {
		return err
	}
	return s.withLease(ctx, alias, func(root *os.Root, name string) error {
		_, current, err := s.load(root, name, alias)
		if err != nil {
			return err
		}
		if current != prior {
			return errPinRevisionConflict
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
	budget, cancel := context.WithTimeout(ctx, 2*time.Second)
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
			return errors.Join(errPinContention, err)
		}
	}
	defer func() { err = errors.Join(err, lease.Close()) }()
	if err = budget.Err(); err != nil {
		return err
	}
	return errors.Join(run(root, name+".json"), ctx.Err())
}

func (s *PinStore) load(root *os.Root, name, alias string) (toolCatalog, pinRevision, error) {
	raw, exists, err := s.read(root, name)
	if err != nil {
		return toolCatalog{}, pinRevision{}, err
	}
	if !exists {
		return toolCatalog{}, pinRevision{}, nil
	}
	c, err := decodePin(raw, s.workspace, alias)
	if err != nil {
		return toolCatalog{}, pinRevision{}, fmt.Errorf("mcpclient: invalid pin %s: %w", alias, err)
	}
	// A prior writer may have renamed successfully but failed its directory sync.
	if err = s.ops.syncDir(root); err != nil {
		return toolCatalog{}, pinRevision{}, errors.Join(errPinDurability, err)
	}
	return c, pinRevision{exists: true, hash: sha256.Sum256(raw)}, nil
}

func decodePin(raw []byte, workspace, alias string) (toolCatalog, error) {
	canonical, err := signing.Canonicalize(raw)
	if err != nil {
		return toolCatalog{}, err
	}
	// encoding/json matches struct fields case-insensitively. The persisted
	// protocol instead requires these five exact keys, without extensions.
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(canonical, &fields); err != nil {
		return toolCatalog{}, err
	}
	if len(fields) != 5 {
		return toolCatalog{}, errors.New("invalid pin record fields")
	}
	for _, key := range []string{"version", "workspace", "alias", "digest", "tools"} {
		if _, ok := fields[key]; !ok {
			return toolCatalog{}, fmt.Errorf("missing pin record field %s", key)
		}
	}
	dec := json.NewDecoder(bytes.NewReader(canonical))
	dec.DisallowUnknownFields()
	var record pinRecord
	if err = dec.Decode(&record); err != nil {
		return toolCatalog{}, err
	}
	if record.Version != catalogFormatVersion || record.Workspace != workspace || record.Alias != alias {
		return toolCatalog{}, errors.New("record identity or version mismatch")
	}
	var entries []catalogEntry
	dec = json.NewDecoder(bytes.NewReader(record.Tools))
	dec.DisallowUnknownFields()
	if err = dec.Decode(&entries); err != nil {
		return toolCatalog{}, err
	}
	c, err := newToolCatalog(entries)
	if err != nil {
		return toolCatalog{}, err
	}
	if !bytes.Equal(record.Tools, c.canonicalBytes()) || record.Digest != c.digest() {
		return toolCatalog{}, errors.New("noncanonical catalog or digest mismatch")
	}
	if err = validatePinCatalog(alias, c); err != nil {
		return toolCatalog{}, err
	}
	return c, nil
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
