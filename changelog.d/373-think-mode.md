### Added — Change thinking mode mid-session (#373)

- `/think off|on|low|medium|high` applies startup's active-chain support checks
  to subsequent turns. Unsupported requests print the existing notice and keep
  the previous setting. `/think` reports the runtime setting; `/think default`
  restores the unset, model-decides state. History and pending input are preserved;
  the setting survives conversation resets within the process but not restarts.
- Runtime reservations now capture System, Tools, and ModelOptions together.
  `Runtime.Replace(system, tools, modelOptions ...provider.ModelOptions)` accepts
  zero or one options value: omission preserves the options current at publication,
  and an explicit value replaces them. Changed settings synchronously revalidate
  the full tool list.
  Existing direct two-argument calls and inferred method values still work;
  interfaces and explicitly typed function variables using the old signature
  must be updated. `Runtime.ModelOptions()` returns an independent copy, including
  after Close.
- `provider.ModelOptions.Clone()` copies all optional pointers and stop strings.
  Runtime construction, replacement, status reads, and request construction use
  it to isolate published options from caller mutations. Active turns retain
  their original options through every model step.
- Name the CLI's startup-only options explicitly, keeping AgentFlow inputs
  distinct from the runtime's current REPL settings.
