//go:build !unix

package main

// clearInheritedDescriptors has nothing to do off unix: no native sandbox
// runs there.
func clearInheritedDescriptors() {}
