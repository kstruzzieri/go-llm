### Added — `golem ops` read-only backend console, terminal (#654)

- `golem ops` (table), `golem ops -json` (opsview v1 document) and
  `golem ops -watch` (live view: redraws every second, polls every 2 s) show,
  for every model in `models.json`, residency, request statistics from
  llama-swap's retained history, observed loads (`-watch` only) and backend
  reachability from llama-swap v235 and Ollama, joined to the roles and use
  cases that use it, plus an attention list. It exits 0 whenever it prints a
  snapshot, including when a backend is down.
- Loopback providers whose `base_url` has no path only. An `openai-compat`
  provider is identified with `GET /api/version`; llama-swap v235 is then read
  with `/api/version`, `/running`, `/api/metrics` and `/v1/models` on every
  poll, and Ollama with `/api/ps`. Every request passes an exact
  method-and-path allowlist over the destination guard, so observation can never
  load or unload a model. Hosted providers are never contacted. Other
  llama-swap versions read unsupported, and other runtimes (`llama-server`
  alone, vLLM, LM Studio) unrecognized.
- Honest by construction: activity is always unknown (v235 publishes no
  reliable in-flight count); a configured alias reads unknown residency and
  n/a statistics rather than unloaded or zero; statistics cover only
  llama-swap's retained history (all clients) and never claim completeness;
  zero token counts are treated as missing; a reading that is no longer
  current, including a failed check past its next scheduled check plus a
  grace window, reads unknown (statistics instead keep their values marked
  stale, and a reachable backend in backoff stays reachable until its next
  scheduled check plus the grace window); and every backend-reported string is escaped
  and clipped before it reaches the terminal. Names over 512 bytes are
  counted, not listed.
- `-watch` uses the terminal's alternate screen and restores it on Ctrl-C and
  SIGTERM. Ctrl-Z suspends it and `fg` redraws, except when golem leads its own
  session (such as `tmux new-window 'golem ops -watch'`), where Ctrl-Z is
  ignored. It refuses to start on Windows, when stdout is not a terminal, or
  with `TERM=dumb`. The one-shot table has no loads column and no retry text.
- Known limitations (clock sharing with llama-swap, 2-second load sampling,
  wide characters) are listed in `docs/golem.md`.
- Each poll reads a backend's identity and residency before its other
  surfaces. When a slow backend's reads do not all fit one poll, its activity
  and model-listing reads take turns across polls, and providers take turns
  going first, within the same request and poll deadlines.
- Ollama model names with a scheme, such as
  `https://registry.ollama.ai/library/llama3`, match the backend's short name.
- `-watch` sizes its frame to the terminal down to a single row, which it
  leaves for the cursor.
