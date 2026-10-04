package interceptor

import (
	"context"
	"regexp"
	"testing"
)

// TestIsCredentialPathMatrix (#627 T1) pins the read set with literals. It
// owns the set's correctness: the search parity test cannot see a defect here,
// because search and the invariant share this predicate.
func TestIsCredentialPathMatrix(t *testing.T) {
	// Literal lists, independent of the production lists: every directory as
	// itself and with a file, at the root and nested; every file name and
	// template at three depths.
	var blocked, allowed []string
	for _, dir := range []string{".ssh", ".gnupg", ".aws", ".kube"} {
		blocked = append(blocked, dir, dir+"/key", "sub/"+dir, "sub/"+dir+"/key")
	}
	for _, name := range []string{".env", ".netrc", "_netrc", ".npmrc", ".pypirc", ".git-credentials",
		".env.local", ".env.production.local", ".env."} {
		blocked = append(blocked, name, "sub/"+name, "a/b/"+name)
	}
	for _, name := range []string{".env.example", ".env.sample", ".env.template", ".env.dist"} {
		allowed = append(allowed, name, "sub/"+name, "a/b/"+name)
	}
	blocked = append(blocked,
		".git/config", "sub/.git/config", ".git/modules/x/config", ".Git/config",
		".ENV", ".Env.Local", ".SSH/x",
		".\u017Fsh/x", ".\u00DFh/x", ".\u1E9Eh/x", ".aw\u017F/credentials", ".git-credential\u017F", ".\u212Aube/config",
		".s\u200Csh/x", ".e\uFEFFnv",
	)
	allowed = append(allowed,
		".ENV.EXAMPLE", "env", ".environment", ".envrc", "prod.env", ".env/bin/activate", "py/.env/lib/site.py",
		".git/HEAD", "config", ".gitconfig", "stra\u00DFe.md", ".g\u0131t/config", ".s\u200Bsh/x",
		"README.md", ".github/workflows/ci.yml",
	)
	for _, p := range blocked {
		if !IsCredentialPath(p) {
			t.Errorf("IsCredentialPath(%q) = false, want true", p)
		}
	}
	for _, p := range allowed {
		if IsCredentialPath(p) {
			t.Errorf("IsCredentialPath(%q) = true, want false", p)
		}
	}
}

// TestIsProtectedPath (#627) pins the scope-refusal set: the write rules'
// components, after the same alias normalization.
func TestIsProtectedPath(t *testing.T) {
	for _, p := range []string{".git", ".git/modules", "x/.git", ".ssh", "a/.aws", ".gnupg/x", ".kube", ".SSH", ".\u017Fsh", ".s\u200Csh"} {
		if !IsProtectedPath(p) {
			t.Errorf("IsProtectedPath(%q) = false, want true", p)
		}
	}
	for _, p := range []string{".env", "home", ".github", ".gitignore", "a/b", ".envrc", ".git-credentials"} {
		if IsProtectedPath(p) {
			t.Errorf("IsProtectedPath(%q) = true, want false", p)
		}
	}
}

// TestReadRuleAncestorsAreProtected (#627 T4, spec §5.4). Let s be a dispatch
// scope IsProtectedPath does not flag, and c a clean strict-descendant child
// path. Then IsCredentialPath(s/c) == IsCredentialPath(c), so a scoped child
// judges its relative paths the way the parent judges the full path.
// Corpora come from the rule lists, so a new rule is covered automatically.
func TestReadRuleAncestorsAreProtected(t *testing.T) {
	names := []string{"a", "home", "env", ".env", ".envrc", "config", ".github", "stra\u00DFe",
		".git", ".Git", ".\u017Fsh", ".s\u200Csh"}
	names = append(names, credentialDirs...)
	names = append(names, credentialFiles...)
	names = append(names, envTemplates...)
	var scopes, children []string
	for _, a := range names {
		scopes = append(scopes, a, "a/"+a)
		children = append(children, a)
		for _, b := range names {
			children = append(children, a+"/"+b)
		}
	}
	for _, s := range scopes {
		if IsProtectedPath(s) {
			continue
		}
		for _, c := range children {
			if full, rel := IsCredentialPath(s+"/"+c), IsCredentialPath(c); full != rel {
				t.Errorf("scope %q, child %q: IsCredentialPath(full)=%v but IsCredentialPath(child)=%v", s, c, full, rel)
			}
		}
	}
	for _, d := range append([]string{".git"}, credentialDirs...) {
		if !IsProtectedPath(d) {
			t.Errorf("IsProtectedPath(%q) = false: a scope rooted there would hide the rule's ancestor", d)
		}
	}
}

// TestCredentialPathInCustomTables (#627 T6): the shipped check is usable in
// a consumer's own table, and a custom PathDeny written in normalized form
// matches an alias spelling.
func TestCredentialPathInCustomTables(t *testing.T) {
	iv, err := NewInvariants([]Invariant{
		{Tool: "read_notebook", Name: "credential_path", Field: "file", Check: CredentialPath{}},
		{Tool: "save_note", Name: "secret_dir", Field: "path", Check: PathDeny{Pattern: regexp.MustCompile(`(^|/)secrets(/|$)`)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	found, err := iv.InspectToolCall(context.Background(), guardCall("read_notebook", `{"file":".npmrc"}`))
	if err != nil {
		t.Fatal(err)
	}
	expectOne(t, found, "credential_path", `path ".npmrc" matches protected pattern`)
	found, err = iv.InspectToolCall(context.Background(), guardCall("save_note", `{"path":"\u017Fecrets/x","content":"y"}`))
	if err != nil {
		t.Fatal(err)
	}
	expectOne(t, found, "secret_dir", `path "secrets/x" matches protected pattern`)
}
