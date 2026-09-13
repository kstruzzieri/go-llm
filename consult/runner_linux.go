package consult

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

func groupExited(pgid int) bool {
	err := syscall.Kill(-pgid, 0)
	if err == syscall.ESRCH {
		return true
	}
	return err == nil && zombieGroup("/proc", pgid, syscall.Getpgid)
}

// Linux keeps an unreaped zombie's process group visible to kill(0). Require
// positive evidence of a zombie-only group; unreadable or malformed member
// data cannot establish cleanup. The proc root and PGID lookup are test seams.
func zombieGroup(procRoot string, pgid int, getpgid func(int) (int, error)) bool {
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return false
	}
	found := false
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 {
			continue
		}
		// Membership does not require reading another user's /proc files (which
		// hidepid=1 can forbid). Only read stat for members of our group.
		group, err := getpgid(pid)
		if errors.Is(err, syscall.ESRCH) || (err == nil && group != pgid) {
			continue
		}
		if err != nil {
			return false // Unknown membership cannot establish cleanup.
		}
		data, err := os.ReadFile(filepath.Join(procRoot, entry.Name(), "stat"))
		if errors.Is(err, os.ErrNotExist) {
			continue // The process was reaped during enumeration.
		}
		if err != nil {
			return false
		}
		// comm may contain spaces and parentheses. Fields after its final ')'
		// start at state (3); pgrp is field 5 and num_threads is field 20.
		end := strings.LastIndexByte(string(data), ')')
		if end < 0 {
			return false
		}
		fields := strings.Fields(string(data[end+1:]))
		if len(fields) < 18 {
			return false
		}
		group, err = strconv.Atoi(fields[2])
		if err != nil {
			return false
		}
		if group != pgid {
			continue
		}
		threads, err := strconv.Atoi(fields[17])
		// A zombie thread-group leader can still have live sibling threads.
		if err != nil || threads != 1 || (fields[0] != "Z" && fields[0] != "X") {
			return false
		}
		found = true
	}
	return found
}
