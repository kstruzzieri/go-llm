//go:build unix && !linux

package tools

// platformTestSetup is the no-op counterpart of the Linux test-parent
// descriptor hygiene in exec_linux_test.go. exec_unix_test.go's TestMain is
// tagged `unix`, so every unix GOOS needs a definition -- darwin, and also
// the BSDs and solaris where the package's tests must still build.
func platformTestSetup() {}
