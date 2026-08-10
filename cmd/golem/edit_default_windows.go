//go:build windows

package main

// defaultEditorCommand is the editor of last resort when neither $VISUAL nor
// $EDITOR names one.
const defaultEditorCommand = "notepad.exe"
