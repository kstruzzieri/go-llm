package main

// memorySearchRedactedMarker replaces a memory_search tool result when the turn
// is persisted, so raw retrieved memory rows do not enter session history or get
// folded into the pinned durable summary. The live turn already consumed the real
// result; this only affects what is stored.
const memorySearchRedactedMarker = "memory_search result omitted from session history"
