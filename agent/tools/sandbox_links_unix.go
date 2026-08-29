//go:build darwin || linux

package tools

// Shared workspace hard-link validation for the pathname/bind-mount sandbox
// backends (#442 Seatbelt, #441 bwrap). Both authorize the workspace by
// pathname (SBPL subpath, bind mount), so an inode reachable through an
// outside name must be rejected before either lifetime spawns.

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// validateSandboxWorkspaceLinks rejects an inode with any directory entry
// outside root. Pathname-based sandboxes authorize names, so an allowed
// workspace hard link would otherwise let a child mutate the same inode
// through an outside name. Internal-only hard links remain valid. This is the
// final host check; an unsandboxed same-UID process can still race it by the
// documented threat model.
func validateSandboxWorkspaceLinks(root string) error {
	type inode struct {
		dev uint64
		ino uint64
	}
	type linkRecord struct {
		total uint64
		seen  uint64
		first string
	}

	links := make(map[inode]linkRecord)
	order := make([]inode, 0)
	err := filepath.WalkDir(root, func(name string, entry fs.DirEntry, walkErr error) error {
		rel, relErr := filepath.Rel(root, name)
		if relErr != nil {
			return fmt.Errorf("tools: sandbox inspect workspace links: %w", relErr)
		}
		rel = filepath.ToSlash(rel)
		if walkErr != nil {
			return fmt.Errorf("tools: sandbox inspect workspace entry %q: %w", rel, walkErr)
		}
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("tools: sandbox inspect workspace entry %q: %w", rel, err)
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return fmt.Errorf("tools: sandbox inspect workspace entry %q: link metadata unavailable", rel)
		}
		total := uint64(stat.Nlink)
		if total <= 1 {
			return nil
		}
		key := inode{dev: uint64(stat.Dev), ino: uint64(stat.Ino)}
		record, exists := links[key]
		if !exists {
			record = linkRecord{total: total, first: rel}
			order = append(order, key)
		} else if record.total != total {
			return fmt.Errorf("tools: sandbox workspace link count changed while inspecting %q", rel)
		}
		record.seen++
		links[key] = record
		return nil
	})
	if err != nil {
		return err
	}
	for _, key := range order {
		record := links[key]
		if record.seen < record.total {
			return fmt.Errorf("tools: sandbox workspace entry %q is linked outside the workspace", record.first)
		}
		if record.seen > record.total {
			return fmt.Errorf("tools: sandbox workspace link count changed while inspecting %q", record.first)
		}
	}
	return nil
}
