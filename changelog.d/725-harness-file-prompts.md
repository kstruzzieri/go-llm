### Security — File-tool writes to agent instruction and harness files always prompt (#725)

`write_file` and `edit_file` calls that target an agent instruction or harness configuration file now always need a human. `/auto-edits` and the `a` answer no longer cover these files, `golem -p -allow-tool write_file` (or `edit_file`) and Agentflow `-approve-plan-edits` deny them, and every listed file, including `models.json` and `.vscode/settings.json`, which many go-llm and VS Code users edit often, now prompts where it was an ordinary write before. `submit_plan` under `-approve-plan-lock` still writes `.agent/` itself.

The set has 28 entries: `AGENTS.md`, `AGENTS.override.md`, `CLAUDE.md`, `CLAUDE.local.md`, `GEMINI.md`, the legacy rules files `.cursorrules`, `.windsurfrules` and `.clinerules`, `.mcp.json`, `opencode.json` and `opencode.jsonc`, `.golem.json`, `models.json`, the `.claude`, `.cursor`, `.windsurf`, `.gemini`, `.codex`, `.opencode` and `.agent` folders, GitHub Copilot's instruction, prompt, agent, hook and settings paths, and `.vscode/mcp.json` and `.vscode/settings.json`. A path is matched by the requested spelling, by the spelling the filesystem resolves, and by the workspace root counted from the repository folder; a spelling Golem cannot verify prompts rather than passing.

Library hosts: `agent/tools` plans these writes with an empty approval key, and `agent/interceptor` exports `IsHarnessPath` and `HarnessPaths`. A host that uses a plain `agent.Approver` never receives the key.

Not covered: writes made by commands, files that a harness file imports or runs, symlinks inside harness folders that point at ordinary folders (#784), and a workspace rooted in a git submodule or nested repository inside a harness folder.
