package main

// autoIndexMode decides at startup whether background auto-indexing runs.
// Every false cause falls through to the existing enableRetrieve path, whose
// warnings/notices already cover the OFF cases (spec 5.2 stage 1).
func autoIndexMode(f flags, autoErr, embChainErr error) bool {
	if f.noRag || f.ragDB != "" || f.noAutoIndex || f.promptSet {
		return false
	}
	return autoErr == nil && embChainErr == nil
}

// autoStartLine is the synchronous startup notice for auto mode.
func autoStartLine(dbExists bool) string {
	if dbExists {
		return "retrieve: refreshing workspace index in the background"
	}
	return "retrieve: building workspace index in the background (first build)"
}
