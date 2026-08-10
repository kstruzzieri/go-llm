//go:build !windows

package main

// defaultEditorCommand is the editor of last resort when neither $VISUAL nor
// $EDITOR names one. vi is the only editor POSIX requires to exist.
const defaultEditorCommand = "vi"
