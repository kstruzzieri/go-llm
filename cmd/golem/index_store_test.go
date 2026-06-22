package main

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"strings"
	"testing"
)

func TestIndexDBPathForWorkspace_Derivation(t *testing.T) {
	root := "/work/proj"
	getenv := func(k string) string {
		if k == "XDG_DATA_HOME" {
			return "/data"
		}
		return ""
	}
	dbPath, wsID, err := indexDBPathForWorkspace(getenv, root)
	if err != nil {
		t.Fatalf("indexDBPathForWorkspace: %v", err)
	}
	sum := sha256.Sum256([]byte(root))
	key := hex.EncodeToString(sum[:])[:16]
	wantDB := filepath.Join("/data", "golem", "indexes", key+".db")
	if dbPath != wantDB {
		t.Errorf("dbPath = %q, want %q", dbPath, wantDB)
	}
	if wsID != "workspace:"+key {
		t.Errorf("workspaceID = %q, want workspace:%s", wsID, key)
	}
	if got := sidecarPath(dbPath); got != filepath.Join("/data", "golem", "indexes", key+".json") {
		t.Errorf("sidecarPath = %q, want .../%s.json", got, key)
	}
}

func TestIndexDBPathForWorkspace_RejectsInsideWorkspace(t *testing.T) {
	root := "/work/proj"
	// XDG inside the workspace => index DB would be inside the tree => error.
	getenv := func(k string) string {
		if k == "XDG_DATA_HOME" {
			return "/work/proj/.data"
		}
		return ""
	}
	if _, _, err := indexDBPathForWorkspace(getenv, root); err == nil {
		t.Fatal("want error when index DB would be inside the workspace")
	}
}

func TestIndexDBPathForWorkspace_MatchesSessionWorkspaceKey(t *testing.T) {
	// The index <sha16> must equal resolveSessionID's default workspace key.
	root := "/work/proj"
	id, err := resolveSessionID(sessionIDOpts{root: root})
	if err != nil {
		t.Fatal(err)
	}
	_, wsID, err := indexDBPathForWorkspace(func(string) string { return "/data" }, root)
	if err != nil {
		t.Fatal(err)
	}
	if id != wsID {
		t.Errorf("session id %q != index workspaceID %q (sha16 must match)", id, wsID)
	}
	if !strings.HasPrefix(wsID, "workspace:") {
		t.Errorf("workspaceID = %q, want workspace: prefix", wsID)
	}
}
