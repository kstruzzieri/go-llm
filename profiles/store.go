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
	CodeIO              ErrorCode = "io"
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
type Store struct{ root string }

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

// checkProfilesDir verifies the safety of <root>/profiles for reading.
// present=false (nil error) means root or profiles dir is absent — reads
// serve curated only. Symlinks, non-directories, and (on unix) loose modes
// or foreign ownership are CodeStoreUnsafe. Write-side creation arrives
// with SaveAs.
func (s *Store) checkProfilesDir() (present bool, err error) {
	fi, err := os.Lstat(s.root)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, codeErr(CodeIO, "", err)
	}
	if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
		return false, codeErr(CodeStoreUnsafe, "", nil)
	}
	pdir := filepath.Join(s.root, "profiles")
	pfi, err := os.Lstat(pdir)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, codeErr(CodeIO, "", err)
	}
	if pfi.Mode()&os.ModeSymlink != 0 || !pfi.IsDir() || !ownerAndModeOK(pfi) {
		return false, codeErr(CodeStoreUnsafe, "", nil)
	}
	return true, nil
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
	present, err := s.checkProfilesDir()
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
		return newProfileDocument(parsed, raw, "embedded:"+slug)
	}
	present, err := s.checkProfilesDir()
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
	return newProfileDocument(parsed, raw, path)
}

// newProfileDocument builds the Document with profile origin; content that
// fails config validation surfaces as CodeIO with the cause unwrappable.
func newProfileDocument(id ID, raw []byte, originPath string) (*config.Document, error) {
	d, err := config.NewDocumentFromBytes(raw, config.Origin{Source: config.OriginProfile, Path: originPath})
	if err != nil {
		return nil, codeErr(CodeIO, id, err)
	}
	return d, nil
}
