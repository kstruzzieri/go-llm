package signing

import (
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
)

// assertNoKeyMaterial formats v with representative fmt verbs and fails if
// any secret appears raw or in common log encodings. Every verb must resolve
// to the same key-ID-only fmt.Formatter output.
func assertNoKeyMaterial(t *testing.T, v interface{ KeyID() string }, secrets ...[]byte) {
	t.Helper()
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X", "%d", "%o", "%b"} {
		out := fmt.Sprintf(verb, v)
		if !strings.Contains(out, v.KeyID()) {
			t.Errorf("verb %s output %q does not name the key id; Format is not wired", verb, out)
		}
		for i, sec := range secrets {
			forms := map[string]string{
				"raw":     string(sec),
				"hex":     hex.EncodeToString(sec),
				"base64":  base64.StdEncoding.EncodeToString(sec),
				"decimal": fmt.Sprintf("%d", sec),
			}
			for name, encoded := range forms {
				if strings.Contains(out, encoded) {
					t.Errorf("verb %s leaks secret %d as %s: %q", verb, i, name, out)
				}
			}
		}
	}
}
