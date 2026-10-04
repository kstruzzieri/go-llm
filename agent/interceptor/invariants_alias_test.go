package interceptor

import "testing"

// TestNormalizePathAliases (#627 D5, D5a) pins every alias mapping with a
// literal: the 11 non-ASCII code points whose Unicode case fold is pure
// ASCII (APFS opens ".ssh" for ".ſsh" and ".ßh") and the 16 code points HFS+
// ignores in names. Neighbors that are not aliases are left alone.
func TestNormalizePathAliases(t *testing.T) {
	type tc struct{ in, want string }
	cases := []tc{
		{".\u017Fsh", ".ssh"}, // LATIN SMALL LETTER LONG S
		{"\u212Aube", "kube"}, // KELVIN SIGN
		{".\u00DFh", ".ssh"},  // LATIN SMALL LETTER SHARP S
		{".\u1E9Eh", ".ssh"},  // LATIN CAPITAL LETTER SHARP S
		{"\uFB00", "ff"},      // ligatures
		{"\uFB01", "fi"},
		{"\uFB02", "fl"},
		{"\uFB03", "ffi"},
		{"\uFB04", "ffl"},
		{"\uFB05", "st"},
		{"\uFB06", "st"},
		{"stra\u00DFe.md", "strasse.md"},
		{".g\u0131t", ".g\u0131t"},   // dotless i: not an APFS or HFS+ alias
		{".s\u200Bsh", ".s\u200Bsh"}, // ZERO WIDTH SPACE: HFS+ does not ignore it
		{".SSH/ID_RSA", ".ssh/id_rsa"},
	}
	for _, cp := range []rune{0x200C, 0x200D, 0x200E, 0x200F, 0x202A, 0x202B, 0x202C, 0x202D, 0x202E,
		0x206A, 0x206B, 0x206C, 0x206D, 0x206E, 0x206F, 0xFEFF} {
		cases = append(cases, tc{".s" + string(cp) + "sh", ".ssh"})
	}
	for _, c := range cases {
		if got := normalizePath(c.in); got != c.want {
			t.Errorf("normalizePath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestWindowsComponent (#627 D5b) runs on every platform: the per-component
// Windows transform is a pure string function, so its stream and trim rules
// are tested even though CI has no Windows runtime.
func TestWindowsComponent(t *testing.T) {
	for in, want := range map[string]string{
		".env::$DATA":   ".env",
		".env:secret":   ".env",
		".env. ":        ".env",
		".env. ::$DATA": ".env",
		"config::$DATA": "config",
		".ssh.":         ".ssh",
		"...":           "...",
		"plain":         "plain",
	} {
		if got := windowsComponent(in); got != want {
			t.Errorf("windowsComponent(%q) = %q, want %q", in, got, want)
		}
	}
}
