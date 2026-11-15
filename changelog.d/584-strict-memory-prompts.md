### Changed — Strict approval: agent-memory writes ask in an exposed conversation, and its labels say what is tracked (#584)

With `-strict-approval`, outside content can no longer save or promote an agent-memory record without a person seeing it. A later clean conversation reads such a record as workspace content, so the write now asks first.

- In an exposed conversation, `agent_memory_create` and `agent_memory_promote` ask `[y/N]` before they run. The prompt shows `agent memory note:` and the note, then `Save this agent memory note? [y/N] `, or `promote agent memory record <id> to <kind>`, then `Promote this agent memory record? [y/N] `. Arguments that do not decode are shown as they are. In a clean conversation, or with strict approval off, both run unprompted as before.
- Library: `golem.Runtime` now passes only `delegate_code` and `dispatch` in `agent.Request.StrictExempt`. A library host's `agent.FreshApprover` is therefore asked about the two memory writes in an exposed strict run, with an empty preview, and should render the call's arguments. The approval floor still exempts all four tools.
- `/grants` shows a clean conversation as `exposure: clean (files and command output are not tracked)`.
- Startup's `permissions:` line shows `strict-approval=on (exposed by MCP results, accepted /consult advice or unrecorded history)`.
