### Changed — golem ops reads backends in parallel (#662)

`golem ops` reads every local backend in its own goroutine with its own 5 s budget. A slow or hung backend no longer defers, delays or opens a residency gap in another backend's readings, and an Ollama whose `/api/ps` times out always gets its `/api/version` check. Providers no longer take turns going first; a slow backend's activity and model-listing reads still take turns across polls when they do not all fit its budget. A collection lasts as long as its slowest backend, still at most 5 seconds.
