package mcpclient

import "testing"

func TestComposeName(t *testing.T) {
	tests := []struct {
		name, alias, remote, want string
		ok                        bool
	}{
		{"simple", "fs", "read_file", "mcp__fs__read_file", true},
		{"hyphens-digits", "srv-1", "do-x2", "mcp__srv-1__do-x2", true},
		{"bad char in remote", "fs", "read file", "", false},
		{"dot rejected (narrower than SDK)", "fs", "a.b", "", false},
		{"too long", "fs", "veryveryveryveryveryveryveryveryveryveryveryverylongtoolname", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := composeName(tt.alias, tt.remote)
			if ok != tt.ok || got != tt.want {
				t.Fatalf("composeName(%q,%q) = (%q,%v), want (%q,%v)", tt.alias, tt.remote, got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestValidAlias(t *testing.T) {
	for _, a := range []string{"fs", "srv-1", "a_b"} {
		if !validAlias(a) {
			t.Errorf("validAlias(%q) = false, want true", a)
		}
	}
	for _, a := range []string{"", "a b", "a.b", "a/b"} {
		if validAlias(a) {
			t.Errorf("validAlias(%q) = true, want false", a)
		}
	}
}
