//go:build unix && !darwin

package tools

// platformHelperMain has no platform helper modes off darwin.
func platformHelperMain([]string) (int, bool) { return 0, false }
