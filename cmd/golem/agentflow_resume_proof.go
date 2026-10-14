package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// resumeProofGuard ties replacement to the proof verified before any resume
// mutation. next-action hides proof errors behind unfinished step/run work,
// and proof_stale can include failures unrelated to Golem's ledger writes.
type resumeProofGuard struct {
	path    string
	present bool
	digest  [sha256.Size]byte
}

func readResumeProof(path string) (resumeProofGuard, error) {
	proof := resumeProofGuard{path: path}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return proof, nil
	}
	if err != nil {
		return proof, fmt.Errorf("read existing proof before resume: %w", err)
	}
	if !info.Mode().IsRegular() {
		return proof, errors.New("existing proof before resume must be a regular file")
	}
	// The nonblocking open also handles a file replaced by a FIFO after Lstat.
	f, err := openAgentflowStateFile(path)
	if err != nil {
		return proof, fmt.Errorf("open existing proof before resume: %w", err)
	}
	defer func() { _ = f.Close() }()
	opened, err := f.Stat()
	if err != nil {
		return proof, fmt.Errorf("stat existing proof before resume: %w", err)
	}
	if !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		return proof, errors.New("existing proof changed while opening before resume; expected the same regular file")
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, f); err != nil {
		return proof, fmt.Errorf("read existing proof before resume: %w", err)
	}
	proof.present = true
	copy(proof.digest[:], hash.Sum(nil))
	return proof, nil
}

func newResumeProofGuard(ctx context.Context, af afClient, root string) (*resumeProofGuard, error) {
	proof, err := readResumeProof(filepath.Join(root, ".agent", "proof-pack.json"))
	if err != nil {
		return nil, err
	}
	if proof.present {
		if err := af.VerifyProof(ctx); err != nil {
			return nil, fmt.Errorf("verify existing proof before resume: %w; no recovery work was performed; inspect .agent/proof-pack.json and move it out of the workspace before retrying -agentflow-resume", err)
		}
		if err := proof.checkUnchanged(); err != nil {
			return nil, err
		}
	}
	return &proof, nil
}

func (p *resumeProofGuard) checkUnchanged() error {
	current, err := readResumeProof(p.path)
	if err != nil {
		return err
	}
	if current != *p {
		return errors.New("agentflow proof changed during resume; no replacement proof was built; inspect .agent/proof-pack.json and run -agentflow-status")
	}
	// This narrows the concurrent-writer window, but cannot make the check
	// and AgentFlow's finish-run atomic (kstruzzieri/agentflow#58).
	return nil
}
