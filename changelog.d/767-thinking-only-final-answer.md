### Fixed — Runs whose final answer is only in the model's reasoning (#767)

Some reasoning models end a turn with their answer inside the reasoning and nothing in the reply itself, so Golem used to finish with no answer.

- `agent.Request` has a new opt-in, `RecoverThinkingOnlyAnswer`. When a run's final step returns no tool calls, a blank reply and non-blank reasoning, and a step is left, the run spends one more tool-free step asking the model for its final answer only. The empty assistant turn and the request for the answer are sent on that step's request alone; they are never saved in the run's messages or a session's history, and reasoning never becomes the answer. Each such step records a `final_answer_retry` event in `Result.Events`. Reasoning a model caller only streams, without returning it in the response, is not recovered.
- Golem turns the opt-in on for every turn it runs: REPL goals, `-p` runs and hosts that use the Golem runtime. The `-trace` record's request carries `recover_thinking_only_answer: true`, and the `-telemetry` run span counts the extra step.
- AgentFlow planner runs and task steps, and dispatch children, build their own requests and do not opt in, so their behavior is unchanged.
- A turn whose follow-up also has no answer ends as before: a `-p` run exits 1, text format prints `one-shot: model produced no final answer`, and `json` and `stream-json` report `status: error` with `empty_answer`. `golem.result.v1` gains and loses no field.
- The stderr footer of a REPL or `-p` turn that ends without an answer now starts `no answer ·` instead of `done ·`, whatever stopped it. Everything after the prefix is unchanged.
