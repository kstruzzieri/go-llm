package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/kstruzzieri/go-llm/conversation"
)

const defaultSessionBudget = 2000

// validSessionName restricts explicit -session ids so display, DB keys, and any
// future session commands stay boring.
var validSessionName = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// sessionIDOpts selects which session id to use. Precedence (highest first):
// fresh, explicit, then the per-workspace default.
type sessionIDOpts struct {
	fresh    bool
	explicit string // raw -session value
	root     string // canonical absolute workspace root
}

// resolveSessionID derives the keyed session id. fresh => a new unique
// "golem:<uuid>"; explicit => validated "user:<id>"; default => a stable
// "workspace:<sha256(root) prefix>".
func resolveSessionID(o sessionIDOpts) (string, error) {
	if o.fresh {
		return "golem:" + conversation.NewID(), nil
	}
	if o.explicit != "" {
		e := strings.TrimSpace(o.explicit)
		if e == "" {
			return "", fmt.Errorf("golem: -session must not be blank")
		}
		if !validSessionName.MatchString(e) {
			return "", fmt.Errorf("golem: invalid -session %q: allowed characters are A-Z a-z 0-9 . _ -", e)
		}
		return "user:" + e, nil
	}
	sum := sha256.Sum256([]byte(o.root))
	return "workspace:" + hex.EncodeToString(sum[:])[:16], nil
}

// sessionDBPath locates the session DB OUTSIDE the repo, under the per-user data
// dir ($XDG_DATA_HOME/golem/sessions.db, else ~/.local/share/golem/sessions.db).
func sessionDBPath(getenv func(string) string) (string, error) {
	dir := getenv("XDG_DATA_HOME")
	if dir == "" {
		home := getenv("HOME")
		if home == "" {
			return "", fmt.Errorf("golem: cannot locate session data dir (HOME and XDG_DATA_HOME unset)")
		}
		dir = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(dir, "golem", "sessions.db"), nil
}
