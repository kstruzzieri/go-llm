package mcpclient

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kstruzzieri/go-llm/internal/pathguard"
)

type pinFileOps struct {
	afterLstat    func(string)
	beforePublish func()
	write         func(*os.File, []byte) (int, error)
	syncFile      func(*os.File) error
	rename        func(*os.Root, string, string) error
	syncDir       func(*os.Root) error
	wait          func(context.Context) error
}

func defaultPinFileOps() pinFileOps {
	return pinFileOps{
		write: (*os.File).Write, syncFile: (*os.File).Sync,
		rename: (*os.Root).Rename, syncDir: syncPinDirectory,
		wait: func(ctx context.Context) error {
			timer := time.NewTimer(10 * time.Millisecond)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-timer.C:
				return nil
			}
		},
	}
}

// Resolve existing base ancestors once (including OS aliases such as /var on
// macOS). Dedicated golem/mcp-pins directories never permit symlink traversal.
func resolvePinBase(base string) (string, error) {
	if !filepath.IsAbs(base) {
		return "", errors.New("mcpclient: pin data base must be absolute")
	}
	current := filepath.Clean(base)
	var suffix []string
	for {
		_, err := os.Lstat(current)
		if err == nil {
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		suffix = append(suffix, filepath.Base(current))
		parent := filepath.Dir(current)
		if parent == current {
			return "", err
		}
		current = parent
	}
	current, err := filepath.EvalSymlinks(current)
	if err != nil {
		return "", err
	}
	for i := len(suffix) - 1; i >= 0; i-- {
		current = filepath.Join(current, suffix[i])
	}
	return current, nil
}

func (s *PinStore) openRoot(create bool) (*os.Root, error) {
	if err := pathguard.ValidateOutside(s.dir, s.workspace); err != nil {
		return nil, err
	}
	volume := filepath.VolumeName(s.base) + string(filepath.Separator)
	root, err := os.OpenRoot(volume)
	if err != nil {
		return nil, err
	}
	baseParts := strings.Split(strings.TrimPrefix(s.base, volume), string(filepath.Separator))
	parts := append(baseParts, "golem", "mcp-pins", pinKey(s.workspace))
	for i, part := range parts {
		if part == "" {
			continue
		}
		child, e := s.openDirectory(root, part, i >= len(baseParts), create)
		closeErr := root.Close()
		if e != nil || closeErr != nil {
			if child != nil {
				closeErr = errors.Join(closeErr, child.Close())
			}
			return nil, errors.Join(e, closeErr)
		}
		root = child
	}
	return root, nil
}

func (s *PinStore) openDirectory(parent *os.Root, name string, private, create bool) (*os.Root, error) {
	before, err := parent.Lstat(name)
	created := false
	if errors.Is(err, os.ErrNotExist) && create {
		created = true
		if err = parent.Mkdir(name, 0700); err != nil && !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		before, err = parent.Lstat(name)
	}
	if err != nil {
		return nil, err
	}
	if !before.IsDir() || (private && !pinOwnerModeOK(before)) {
		return nil, fmt.Errorf("mcpclient: unsafe pin directory %s", name)
	}
	if s.ops.afterLstat != nil {
		s.ops.afterLstat(filepath.Join(parent.Name(), name))
	}
	root, err := parent.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	opened, err := root.Stat(".")
	if err != nil || !os.SameFile(before, opened) || !opened.IsDir() || (private && !pinOwnerModeOK(opened)) {
		return nil, errors.Join(fmt.Errorf("mcpclient: pin directory %s changed while opening", name), err, root.Close())
	}
	// Confirm dedicated entries even on retries after a failed parent sync.
	// Newly created base directories also need sync; pre-existing general
	// ancestors follow the project's existing data-directory conventions.
	// In particular, Windows need not open system ancestors for writing.
	if create && (private || created) {
		if err = s.ops.syncDir(parent); err != nil {
			return nil, errors.Join(errPinDurability, err, root.Close())
		}
	}
	return root, nil
}

func (s *PinStore) openRegular(root *os.Root, name string, create bool) (*os.File, bool, error) {
	before, err := root.Lstat(name)
	exists := err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, false, err
	}
	if !exists && !create {
		return nil, false, nil
	}
	if exists && (!before.Mode().IsRegular() || !pinOwnerModeOK(before)) {
		return nil, true, fmt.Errorf("mcpclient: unsafe pin file %s", name)
	}
	if s.ops.afterLstat != nil {
		s.ops.afterLstat(name)
	}
	flags := os.O_RDONLY | pinNonblock
	if create {
		flags = os.O_RDWR | pinNonblock
		if !exists {
			flags |= os.O_CREATE | os.O_EXCL
		}
	}
	f, err := root.OpenFile(name, flags, 0600)
	if create && !exists && errors.Is(err, os.ErrExist) {
		return s.openRegular(root, name, create)
	}
	if err != nil {
		return nil, exists, err
	}
	after, err := f.Stat()
	if err != nil || !after.Mode().IsRegular() || !pinOwnerModeOK(after) || (exists && !os.SameFile(before, after)) {
		return nil, true, errors.Join(fmt.Errorf("mcpclient: pin file %s changed or is unsafe", name), err, f.Close())
	}
	// Recheck the directory entry too: never lock a file already unlinked or
	// replaced while opening, even when the descriptor itself is still regular.
	current, err := root.Lstat(name)
	if err != nil || !os.SameFile(after, current) {
		return nil, true, errors.Join(fmt.Errorf("mcpclient: pin file %s replaced", name), err, f.Close())
	}
	if create && !exists {
		if err = chmodPinFile(f); err != nil {
			return nil, true, errors.Join(err, f.Close())
		}
	}
	return f, true, nil
}

func (s *PinStore) read(root *os.Root, name string) (raw []byte, exists bool, err error) {
	f, exists, err := s.openRegular(root, name, false)
	if err != nil || !exists {
		return nil, exists, err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	info, err := f.Stat()
	if err != nil {
		return nil, true, err
	}
	if info.Size() > maxPinBytes {
		return nil, true, errors.New("mcpclient: pin exceeds 16 MiB")
	}
	raw, err = io.ReadAll(io.LimitReader(f, maxPinBytes+1))
	if err != nil {
		return nil, true, err
	}
	if len(raw) > maxPinBytes {
		return nil, true, errors.New("mcpclient: pin exceeds 16 MiB")
	}
	return raw, true, nil
}

func (s *PinStore) acquireLease(root *os.Root, name string) (*pinLease, error) {
	f, _, err := s.openRegular(root, name, true)
	if err != nil {
		return nil, err
	}
	lease, err := lockPinFile(f)
	if err != nil {
		if closeErr := f.Close(); closeErr != nil {
			return nil, errors.Join(err, closeErr)
		}
		return nil, err
	}
	return lease, nil
}

func (s *PinStore) publish(ctx context.Context, root *os.Root, name, alias string, c toolCatalog) (err error) {
	raw, err := json.Marshal(pinRecord{Version: c.version(), Workspace: s.workspace, Alias: alias, Digest: c.digest(), Tools: c.canonicalBytes()})
	if err != nil {
		return err
	}
	if len(raw) > maxPinBytes {
		return errors.New("mcpclient: serialized pin exceeds 16 MiB")
	}
	temp := "." + name + ".tmp-" + rand.Text()
	f, err := root.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|pinNonblock, 0600)
	if err != nil {
		return err
	}
	createdInfo, err := f.Stat()
	if err != nil {
		return errors.Join(err, f.Close(), root.Remove(temp))
	}
	closed := false
	defer func() {
		if !closed {
			err = errors.Join(err, f.Close())
		}
		if e := root.Remove(temp); e != nil && !errors.Is(e, os.ErrNotExist) {
			err = errors.Join(err, e)
		}
	}()
	if err = chmodPinFile(f); err != nil {
		return err
	}
	n, err := s.ops.write(f, raw)
	if err != nil {
		return err
	}
	if n != len(raw) {
		return io.ErrShortWrite
	}
	if err = s.ops.syncFile(f); err != nil {
		return err
	}
	err = f.Close()
	closed = true
	if err != nil {
		return err
	}
	if s.ops.beforePublish != nil {
		s.ops.beforePublish()
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	currentInfo, err := root.Lstat(temp)
	if err != nil || !os.SameFile(createdInfo, currentInfo) || !currentInfo.Mode().IsRegular() || !pinOwnerModeOK(currentInfo) {
		return errors.Join(errors.New("mcpclient: pin temporary file changed before publication"), err)
	}
	if err = s.ops.rename(root, temp, name); err != nil {
		return err
	}
	if err = s.ops.syncDir(root); err != nil {
		return errors.Join(errPinDurability, err)
	}
	return nil
}
