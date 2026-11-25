//go:build darwin

package tools

// postRunCollect makes the before-walk record what only the post-run check
// uses (sandboxGitLayout.collect). The check runs only on Linux, so Seatbelt's
// inspection reads nothing more than it mounts; a test sets it to exercise the
// shared code on macOS.
var postRunCollect = false

// mayFoldNames reports whether the walks must look git's names up in dir
// (lookupGitNames). The post-run check that uses those records runs only on
// Linux, so on macOS the walks rely on their listings, as before it.
func (*sandboxGitLayout) mayFoldNames(string) bool { return false }
