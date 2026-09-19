//go:build windows

package interceptor

import "testing"

// TestPathDenyWindowsSeparators pins the native Windows interpretation:
// backslashes are separators, drive-absolute paths are still matched by
// component, and case folding applies. CI compiles this file in its Windows
// smoke step; a native run needs a Windows host and is reported separately.
func TestPathDenyWindowsSeparators(t *testing.T) {
	cases := []struct {
		name   string
		tool   string
		args   string
		detail string // "" means no finding
	}{
		{"backslash credential dir", "write_file", `{"path":"sub\\.aws\\credentials","content":"x"}`, `path "sub/.aws/credentials" matches protected pattern`},
		{"backslash git hook", "write_file", `{"path":".git\\hooks\\pre-commit","content":"x"}`, `path ".git/hooks/pre-commit" matches protected pattern`},
		{"drive absolute ssh", "write_file", `{"path":"C:\\Users\\me\\.ssh\\id_rsa","content":"x"}`, `path "c:/users/me/.ssh/id_rsa" matches protected pattern`},
		{"mixed separators and case", "edit_file", `{"path":"sub/.Git\\config","old_string":"a","new_string":"b"}`, `path "sub/.git/config" matches protected pattern`},
		{"dot-dot with backslashes", "write_file", `{"path":"foo\\..\\.kube\\config","content":"x"}`, `path ".kube/config" matches protected pattern`},
		{"read env with backslash parent", "read_file", `{"path":"sub\\.env"}`, `path "sub/.env" matches protected pattern`},
		// Win32 drops a single trailing period from an intermediate segment
		// and all trailing periods and spaces from the final one; the guard
		// trims both from every component, which can only over-block.
		{"trailing period alias", "write_file", `{"path":".git.\\hooks\\pre-commit","content":"x"}`, `path ".git/hooks/pre-commit" matches protected pattern`},
		{"trailing space alias", "read_file", `{"path":"sub\\.ssh \\id_rsa"}`, `path "sub/.ssh/id_rsa" matches protected pattern`},
		{"final component trailing periods", "read_file", `{"path":".env.."}`, `path ".env" matches protected pattern`},
		{"all-period component is kept", "write_file", `{"path":"...\\x","content":"x"}`, ""},
		{"plain git dir", "write_file", `{"path":"notes\\git\\x","content":"x"}`, ""},
		{"gitignore", "write_file", `{"path":"sub\\.gitignore","content":"x"}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			found := inspect(t, tc.tool, tc.args)
			if tc.detail == "" {
				expectNone(t, found)
				return
			}
			rule := "protected_path"
			if tc.tool == "read_file" {
				rule = "credential_path"
			}
			expectOne(t, found, rule, tc.detail)
		})
	}
}
