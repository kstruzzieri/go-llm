### Security — `search` skips credential files; read tripwire widened (#627)

`search` and the default `credential_path` invariant on `read_file` now share
one credential set: `.env` and `.env.*` (except `.env.example`, `.env.sample`,
`.env.template` and `.env.dist`), `.netrc`, `_netrc`, `.npmrc`, `.pypirc`,
`.git-credentials`, a `config` file under `.git`, and anything under `.ssh`,
`.gnupg`, `.aws` or `.kube`. `search` skips these files (it never walks a
directory named exactly `.git`); `read_file` refuses them where the invariants
are installed. Path guards also normalize spellings that filesystems treat as
the same name: APFS opens `.ssh` for `.ſsh` and `.ßh`, HFS+ ignores zero-width
joiners and non-joiners, direction marks and the BOM, and Windows reads `.env`
through `.env::$DATA`. A `dispatch` task scoped at or below `.git`, `.ssh`,
`.gnupg`, `.aws` or `.kube` is refused, because that child's guards would see
paths below the directory without it.

#### Upgrade note

- The `search` skip and the scope refusal are tool behavior in `agent/tools`
  and apply to every consumer, not only the Golem CLI: library `search` output
  can omit files that previously appeared, with or without interceptors.
- The `read_file` refusal stays opt-in for library consumers: install
  `interceptor.Invariants` on the orchestrator and pass it to
  `tools.NewDispatch` for children. The `golem.Runtime` bootstrap installs
  none. It is always on in the Golem CLI.
- The default table's `read_file` row now holds an `interceptor.CredentialPath`
  check instead of a `PathDeny`. `CredentialPath`, `interceptor.IsCredentialPath`
  (the read set) and `interceptor.IsProtectedPath` (the write set) are new
  exports for custom tables and hosts.
- Custom `PathDeny` patterns are matched against the normalized path, so a
  pattern that spells one of the normalized characters literally (for example
  `ß`) no longer matches. Write it in normalized form (`ss`); the `PathDeny`
  doc lists every mapped code point.
- `glob` and `list` still show these names. Shell commands, `retrieve`, MCP
  tools, verifier commands and `edit_file`'s pre-approval match errors can
  still reach the same bytes, as can a copy or hard link under another name.
  This is a tripwire, not confinement.
