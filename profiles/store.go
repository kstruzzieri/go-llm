package profiles

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/kstruzzieri/go-llm/config"
)

// Threat model: the PRIVATE boundary is <root>/profiles (0700, euid-owned,
// non-symlink; unix-enforced, same-user best-effort elsewhere). The root
// itself is shared (it hosts models.json) and is only required to be a
// real directory when present. Lstat-then-open races and manipulation
// outside the private dir are residual risks accepted on ownership+mode.

// ErrorCode is the bounded vocabulary store errors expose to consumers.
// Firn maps CodeOf(err); error text never carries filesystem paths.
type ErrorCode string

const (
	CodeInvalidID       ErrorCode = "invalid_id"
	CodeNotFound        ErrorCode = "not_found"
	CodeCuratedReadOnly ErrorCode = "curated_read_only"
	CodeConflict        ErrorCode = "conflict"
	CodeDurability      ErrorCode = "durability_uncertain" // SaveOutcome.Warning value, never an error code path
	CodeStoreUnsafe     ErrorCode = "store_unsafe"
	CodeIO              ErrorCode = "io"             // filesystem failures only — content failures are config_invalid
	CodeConfigInvalid   ErrorCode = "config_invalid" // profile content failed config parse/validation
)

// storeError pairs a code with the profile id it concerns. Error text is
// bounded to those two — never a filesystem path; Unwrap exposes any inner
// cause for programmatic inspection only.
type storeError struct {
	code ErrorCode
	id   ID
	err  error
}

func (e *storeError) Error() string {
	if e.id == "" {
		return "profiles: " + string(e.code)
	}
	return "profiles: " + string(e.code) + " (" + string(e.id) + ")"
}

func (e *storeError) Unwrap() error { return e.err }

func codeErr(code ErrorCode, id ID, err error) *storeError {
	return &storeError{code: code, id: id, err: err}
}

// CodeOf returns the ErrorCode carried by a store error, or "" when err is
// nil or not a store error (context cancellation passes through raw).
func CodeOf(err error) ErrorCode {
	var se *storeError
	if errors.As(err, &se) {
		return se.code
	}
	return ""
}

// Store reads curated profiles from the embedded catalog and user profiles
// from <root>/profiles.
type Store struct {
	root string
	opts config.DocumentOptions
}

// NewStore returns a Store over root. The root is not touched until a
// method needs it.
func NewStore(root string) *Store { return &Store{root: root} }

// DefaultStore places the store in the shared go-llm config root
// (UserConfigDir()/go-llm — the same directory models.json discovery uses).
func DefaultStore() (*Store, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return nil, err
	}
	return NewStore(filepath.Join(base, "go-llm")), nil
}

// NewStoreWithOptions is NewStore with document construction options
// (spec §7): the LookupEnv seam threads into every profile Document this
// store parses, so a host can resolve ${ENV} API-key refs in cloud
// profiles without process-env mutation. Zero options = ambient behavior,
// identical to NewStore.
func NewStoreWithOptions(root string, opts config.DocumentOptions) *Store {
	s := NewStore(root)
	s.opts = opts
	return s
}

// DefaultStoreWithOptions mirrors DefaultStore with options.
func DefaultStoreWithOptions(opts config.DocumentOptions) (*Store, error) {
	s, err := DefaultStore()
	if err != nil {
		return nil, err
	}
	s.opts = opts
	return s, nil
}

// checkProfilesDir verifies the safety of <root>/profiles. On reads
// (forWrite=false), present=false with nil error means root or profiles dir
// is absent — reads serve curated only and create nothing. On writes, the
// missing directories are created (root with default modes, profiles/ 0700)
// and each creation fsyncs its parent — first-use durability. Symlinks,
// non-directories, and (on unix) loose modes or foreign ownership are
// CodeStoreUnsafe.
func (s *Store) checkProfilesDir(forWrite bool) (present bool, err error) {
	fi, err := os.Lstat(s.root)
	if errors.Is(err, fs.ErrNotExist) {
		if !forWrite {
			return false, nil
		}
		if err := os.MkdirAll(s.root, 0o755); err != nil {
			return false, codeErr(CodeIO, "", err)
		}
		if err := syncParentDir(s.root); err != nil {
			return false, codeErr(CodeIO, "", err)
		}
		fi, err = os.Lstat(s.root)
		if err != nil {
			return false, codeErr(CodeIO, "", err)
		}
	} else if err != nil {
		return false, codeErr(CodeIO, "", err)
	}
	if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
		return false, codeErr(CodeStoreUnsafe, "", nil)
	}
	pdir := filepath.Join(s.root, "profiles")
	pfi, err := os.Lstat(pdir)
	if errors.Is(err, fs.ErrNotExist) {
		if !forWrite {
			return false, nil
		}
		// A concurrent first write may have created it between the Lstat
		// and here — that is success, and the re-Lstat below still applies
		// the full safety checks to whatever now occupies the name.
		if err := os.Mkdir(pdir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
			return false, codeErr(CodeIO, "", err)
		}
		if err := syncParentDir(pdir); err != nil {
			return false, codeErr(CodeIO, "", err)
		}
		pfi, err = os.Lstat(pdir)
		if err != nil {
			return false, codeErr(CodeIO, "", err)
		}
	} else if err != nil {
		return false, codeErr(CodeIO, "", err)
	}
	if pfi.Mode()&os.ModeSymlink != 0 || !pfi.IsDir() || !ownerAndModeOK(pfi) {
		return false, codeErr(CodeStoreUnsafe, "", nil)
	}
	return true, nil
}

// syncParentDir fsyncs the parent directory of path so a just-created
// directory entry is durable (build-tagged syncDir handles Windows).
func syncParentDir(path string) error {
	return syncDir(filepath.Dir(path))
}

// List returns the curated block (sorted) followed by the user block
// (sorted). User entries that are non-regular files or whose stems do not
// parse as valid user IDs are skipped. User Info rows carry ID only —
// Description and Revision stay empty so listing never opens profile files.
func (s *Store) List(ctx context.Context) ([]Info, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var out []Info
	for _, id := range curatedIDs() {
		if info, ok := curatedInfo(id); ok {
			out = append(out, info)
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	present, err := s.checkProfilesDir(false)
	if err != nil {
		return nil, err
	}
	if !present {
		return out, nil
	}
	pdir := filepath.Join(s.root, "profiles")
	entries, err := os.ReadDir(pdir)
	if err != nil {
		return nil, codeErr(CodeIO, "", err)
	}
	var user []Info
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		stem, isJSON := strings.CutSuffix(e.Name(), ".json")
		if !isJSON {
			continue
		}
		id, perr := ParseID("user/" + stem)
		if perr != nil {
			continue
		}
		user = append(user, Info{ID: id})
	}
	sort.Slice(user, func(i, j int) bool { return user[i].ID < user[j].ID })
	return append(out, user...), nil
}

// Load returns a Document for id: curated ids from the embedded catalog
// (origin path "embedded:<slug>"), user ids from <root>/profiles with the
// real path as origin. The ID is validated before any filesystem access.
func (s *Store) Load(ctx context.Context, id ID) (*config.Document, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	parsed, err := ParseID(string(id))
	if err != nil {
		return nil, codeErr(CodeInvalidID, "", err)
	}
	ns, slug, _ := strings.Cut(string(parsed), "/")
	if ns == "curated" {
		raw, err := curatedBytes(parsed)
		if err != nil {
			return nil, codeErr(CodeNotFound, parsed, nil)
		}
		return s.newProfileDocument(parsed, raw, "embedded:"+slug)
	}
	present, err := s.checkProfilesDir(false)
	if err != nil {
		return nil, err
	}
	if !present {
		return nil, codeErr(CodeNotFound, parsed, nil)
	}
	path := filepath.Join(s.root, "profiles", slug+".json")
	fi, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, codeErr(CodeNotFound, parsed, nil)
	}
	if err != nil {
		return nil, codeErr(CodeIO, parsed, err)
	}
	if !fi.Mode().IsRegular() {
		return nil, codeErr(CodeStoreUnsafe, parsed, nil)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, codeErr(CodeIO, parsed, err)
	}
	return s.newProfileDocument(parsed, raw, path)
}

// SaveOutcome reports what SaveAs actually did (spec amendment 4, round-3
// error discipline). Persisted==true ⇒ the accompanying error is ALWAYS
// nil; Warning carries the bounded durability code when applicable.
type SaveOutcome struct {
	Persisted bool
	Warning   ErrorCode // "" or CodeDurability
	Revision  string
}

// saveOutcomeFromErr maps a config-layer save error to the store contract.
// Unexported and pure — the unit tests drive every branch without hooks.
// Vanished targets (os.ErrNotExist mid-overwrite) are conflicts: the world
// changed under the caller's expected revision.
func saveOutcomeFromErr(err error, revision string, id ID) (SaveOutcome, error) {
	switch {
	case err == nil:
		return SaveOutcome{Persisted: true, Revision: revision}, nil
	case errors.Is(err, config.ErrDurabilityUncertain):
		return SaveOutcome{Persisted: true, Warning: CodeDurability, Revision: revision}, nil
	case errors.Is(err, os.ErrExist), errors.Is(err, config.ErrRevisionConflict), errors.Is(err, os.ErrNotExist):
		return SaveOutcome{}, codeErr(CodeConflict, id, err)
	default:
		// A read-only (duplicate_keys) refusal is a CONTENT failure, not a
		// filesystem one — CodeIO stays filesystem-only (spec §8).
		if d, ok := config.DiagnosticOf(err); ok && d.Code == config.CodeDuplicateKeys {
			return SaveOutcome{}, codeErr(CodeConfigInvalid, id, err)
		}
		return SaveOutcome{}, codeErr(CodeIO, id, err)
	}
}

// SaveAs persists the document as a user profile. Empty overwriteRevision
// is create-only; a non-empty one is a compare-and-replace against the
// stored revision. The document's origin becomes the profile path
// (OriginProfile). Persisted==true always pairs with a nil error —
// durability uncertainty travels inside the outcome as Warning.
func (s *Store) SaveAs(ctx context.Context, id ID, d *config.Document, overwriteRevision string) (SaveOutcome, error) {
	if err := ctx.Err(); err != nil {
		return SaveOutcome{}, err
	}
	parsed, err := ParseID(string(id))
	if err != nil {
		return SaveOutcome{}, codeErr(CodeInvalidID, "", err)
	}
	ns, slug, _ := strings.Cut(string(parsed), "/")
	if ns != "user" {
		return SaveOutcome{}, codeErr(CodeCuratedReadOnly, parsed, nil)
	}
	if err := d.CheckWritable(); err != nil {
		return saveOutcomeFromErr(err, d.Revision(), parsed)
	}
	if _, err := s.checkProfilesDir(true); err != nil {
		return SaveOutcome{}, err
	}
	path := filepath.Join(s.root, "profiles", slug+".json")
	if overwriteRevision != "" {
		fi, err := os.Lstat(path)
		if errors.Is(err, fs.ErrNotExist) {
			return SaveOutcome{}, codeErr(CodeConflict, parsed, err)
		}
		if err != nil {
			return SaveOutcome{}, codeErr(CodeIO, parsed, err)
		}
		if !fi.Mode().IsRegular() {
			return SaveOutcome{}, codeErr(CodeStoreUnsafe, parsed, nil)
		}
		return saveOutcomeFromErr(d.SaveReplaceAs(path, overwriteRevision, config.OriginProfile), d.Revision(), parsed)
	}
	return saveOutcomeFromErr(d.SaveNewAs(path, config.OriginProfile), d.Revision(), parsed)
}

// Export writes profile id to destPath as a plain loadable models.json — a
// host-CLI affordance excluded from the Wails surface. The destination must
// not exist (os.ErrExist ⇒ CodeConflict); the source rides Load's full
// Lstat discipline; the exported file's provenance is explicit-path.
func (s *Store) Export(ctx context.Context, id ID, destPath string) error {
	d, err := s.Load(ctx, id)
	if err != nil {
		return err
	}
	if err := d.SaveNewAs(destPath, config.OriginExplicit); err != nil {
		switch {
		case errors.Is(err, os.ErrExist):
			return codeErr(CodeConflict, id, err)
		case errors.Is(err, config.ErrDurabilityUncertain):
			return nil // bytes are live; export has no outcome channel and the file is a copy
		default:
			// Read-only refusal = content failure, mirroring saveOutcomeFromErr.
			if d, ok := config.DiagnosticOf(err); ok && d.Code == config.CodeDuplicateKeys {
				return codeErr(CodeConfigInvalid, id, err)
			}
			return codeErr(CodeIO, id, err)
		}
	}
	return nil
}

// newProfileDocument builds the Document with profile origin; config
// parse/validation failures surface as CodeConfigInvalid with the cause
// chain preserved, so config.DiagnosticOf answers "what exactly is wrong".
// CodeIO remains for filesystem failures only.
func (s *Store) newProfileDocument(id ID, raw []byte, originPath string) (*config.Document, error) {
	d, err := config.ParseDocument(raw, config.Origin{Source: config.OriginProfile, Path: originPath}, s.opts)
	if err != nil {
		return nil, codeErr(CodeConfigInvalid, id, err)
	}
	return d, nil
}
