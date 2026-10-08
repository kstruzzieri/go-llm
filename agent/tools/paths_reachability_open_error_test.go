//go:build linux || darwin

package tools

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// An error opening the last walked file must not hide a root-level failure.
func TestReachabilitySearchOpenFailureCannotHideRootChange(t *testing.T) {
	for _, scenario := range []string{"root renamed and final file removed", "root parent and final file inaccessible"} {
		t.Run(scenario, func(t *testing.T) {
			if scenario == "root parent and final file inaccessible" && os.Geteuid() == 0 {
				t.Skip("permission assertions require non-root")
			}
			parent := t.TempDir()
			root := filepath.Join(parent, "ws")
			if err := os.Mkdir(root, 0o700); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"a.txt", "b.txt"} {
				if err := os.WriteFile(filepath.Join(root, name), []byte("MATCH "+name+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			ws, err := NewWorkspace(root)
			if err != nil {
				t.Fatal(err)
			}
			root = ws.root
			parent = filepath.Dir(root)
			fired := false
			ws.SetScopeGuard(func(rel string, _ bool) error {
				if rel != "b.txt" || fired {
					return nil
				}
				fired = true
				if scenario == "root renamed and final file removed" {
					if err := os.Rename(root, root+"-moved"); err != nil {
						t.Fatal(err)
					}
					if err := os.Remove(filepath.Join(root+"-moved", "b.txt")); err != nil {
						t.Fatal(err)
					}
					return nil
				}
				if err := os.Chmod(filepath.Join(root, "b.txt"), 0); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(parent, 0o700) })
				if err := os.Chmod(parent, 0); err != nil {
					t.Fatal(err)
				}
				return nil
			})
			res, err := NewSearch(ws).Invoke(t.Context(), json.RawMessage(`{"pattern":"MATCH"}`))
			want := "path changed during access"
			if scenario == "root parent and final file inaccessible" {
				want = "path is not accessible"
			}
			if !fired || err != nil || !res.IsError || res.Content != want {
				t.Fatalf("search = %+v, %v, guard fired=%v; want IsError and %q", res, err, fired, want)
			}
		})
	}
}
