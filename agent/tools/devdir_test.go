package tools

import (
	"errors"
	"fmt"
	"maps"
	"strings"
	"testing"
)

const (
	testSelectLink = "/private/var/select/developer_dir"
	testDBLink     = "/private/var/db/xcode_select_link"
)

var (
	rootOwnedDir  = linkInfo{exists: true}
	rootOwnedLink = linkInfo{exists: true, symlink: true}
)

// fakeLinkHost is an lstat over a stock host, root-owned directories from /
// down to both link parents and neither link present, with over applied. An
// lstat of failing returns an error other than "does not exist"; one of a
// path the stock host does not list returns an error too, so a check that
// strays from the two links and their parents is refused.
func fakeLinkHost(over map[string]linkInfo, failing string) func(string) (linkInfo, error) {
	host := map[string]linkInfo{
		"/": rootOwnedDir, "/private": rootOwnedDir, "/private/var": rootOwnedDir,
		"/private/var/select": rootOwnedDir, "/private/var/db": rootOwnedDir,
		testSelectLink: {}, testDBLink: {},
	}
	maps.Copy(host, over)
	return func(p string) (linkInfo, error) {
		if p == failing {
			return linkInfo{}, errors.New("permission denied")
		}
		info, ok := host[p]
		if !ok {
			return linkInfo{}, fmt.Errorf("unexpected lstat of %q", p)
		}
		return info, nil
	}
}

// TestCheckDeveloperLinks pins the issue's link check (#722 D3): each
// selection link that exists must be a root-owned symlink in a root-owned
// directory chain; a missing link is fine; any other lstat error refuses.
// Valid controls run first; each refusal names its reason and the path.
func TestCheckDeveloperLinks(t *testing.T) {
	user := func(info linkInfo) linkInfo { info.uid = 501; return info }
	valid := map[string]map[string]linkInfo{
		"both absent":      nil,
		"select link only": {testSelectLink: rootOwnedLink},
		"db link only":     {testDBLink: rootOwnedLink},
		"both links":       {testSelectLink: rootOwnedLink, testDBLink: rootOwnedLink},
	}
	for name, over := range valid {
		t.Run(name, func(t *testing.T) {
			if err := checkDeveloperLinks(fakeLinkHost(over, "")); err != nil {
				t.Fatalf("valid control refused: %v", err)
			}
		})
	}
	refused := []struct {
		name    string
		over    map[string]linkInfo
		failing string
		reason  string
		path    string
	}{
		{"select link owned by 501", map[string]linkInfo{testSelectLink: user(rootOwnedLink)}, "", "is not a root-owned symlink", testSelectLink},
		{"db link owned by 501", map[string]linkInfo{testDBLink: user(rootOwnedLink)}, "", "is not a root-owned symlink", testDBLink},
		{"link not a symlink", map[string]linkInfo{testSelectLink: rootOwnedDir}, "", "is not a root-owned symlink", testSelectLink},
		{"parent owned by 501", map[string]linkInfo{testSelectLink: rootOwnedLink, "/private/var/select": user(rootOwnedDir)}, "", "is not a root-owned directory", "/private/var/select"},
		{"ancestor owned by 501", map[string]linkInfo{testDBLink: rootOwnedLink, "/private": user(rootOwnedDir)}, "", "is not a root-owned directory", "/private"},
		{"root owned by 501", map[string]linkInfo{testDBLink: rootOwnedLink, "/": user(rootOwnedDir)}, "", "is not a root-owned directory", "/"},
		{"parent is a symlink", map[string]linkInfo{testDBLink: rootOwnedLink, "/private/var/db": rootOwnedLink}, "", "is not a root-owned directory", "/private/var/db"},
		{"parent missing", map[string]linkInfo{testDBLink: rootOwnedLink, "/private/var/db": {}}, "", "is not a root-owned directory", "/private/var/db"},
		{"lstat error on a link", nil, testDBLink, "cannot be inspected", testDBLink},
		{"lstat error on a parent", map[string]linkInfo{testSelectLink: rootOwnedLink}, "/private/var", "cannot be inspected", "/private/var"},
	}
	for _, r := range refused {
		t.Run(r.name, func(t *testing.T) {
			err := checkDeveloperLinks(fakeLinkHost(r.over, r.failing))
			// The path is matched quoted, so a parent is not found inside
			// its link's own spelling.
			if err == nil || !strings.Contains(err.Error(), r.reason) || !strings.Contains(err.Error(), fmt.Sprintf("%q", r.path)) {
				t.Fatalf("checkDeveloperLinks = %v, want %q naming %q", err, r.reason, r.path)
			}
		})
	}
}
