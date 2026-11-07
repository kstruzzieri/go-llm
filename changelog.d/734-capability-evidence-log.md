### Added — Shared capability evidence store for hosts and Golem (#734)

Hosts can now open one shared capability store with `fingerprint.OpenSharedStore`, and Golem opens it the same way. The store lives at `$XDG_DATA_HOME/golem/fingerprints.db` (else `~/.local/share/golem/fingerprints.db`). The directory is 0700, the file and its sidecars are 0600, and the path is refused inside the workspace. If the file cannot be used, the store falls back to memory for that run and `Warning()` says why; if even that fails, `Status()` is `disabled` and `Log()` and `Probes()` return nil. Golem prints the same warning as before and still honors `-no-cap-probe`.

- The store keeps an append-only log of capability evidence, keyed by endpoint, adapter and model digest, so the same model name on two endpoints, or a `localhost` and a `127.0.0.1` address of one server, are told apart. Old rows are pruned at open, only when there are some to prune.
- Provider listings are stored per credential. A key is never stored; each credential gets its own listing and access state, and none of it appears in any exported type.
- `configio.RefreshInventory` writes listings and provider-reported facts to the store only when the host passes `configio.WithPublish`. A listing that fails keeps the previous snapshot and reports one of a fixed set of reasons, such as `listing_unsupported` or `rate_limited`.
- Capability answers and fingerprint profiles for Ollama models now key on the model's real digest, so existing rows are re-checked once on next use.
- The existing `capability_probes` table is untouched and older binaries keep reading and writing it; Golem still uses it in this release.
