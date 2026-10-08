//go:build linux || darwin

package main

import "os"

// PTY helpers shared by the Linux and Darwin real-terminal tests; openPTY is
// per platform.

// ptyPair is a master/slave PTY pair. The slave stands in for the process's
// real stdin and stdout, so the editor's descriptors are genuine terminals.
type ptyPair struct {
	master *os.File
	slave  *os.File
}

// close is idempotent: subtests close early to unblock a hung read, and the
// cleanup runs again at test end.
func (p *ptyPair) close() {
	_ = p.master.Close()
	_ = p.slave.Close()
}

// drainMaster consumes everything the editor writes to the terminal and makes
// it available to the test. Without a reader the PTY buffer fills and the
// editor blocks on its own prompt repaint, so this is required for progress,
// not just for assertions.
func drainMaster(p *ptyPair) *lockedBuffer {
	seen := &lockedBuffer{}
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := p.master.Read(buf)
			if n > 0 {
				_, _ = seen.Write(buf[:n])
			}
			if err != nil {
				return
			}
		}
	}()
	return seen
}
