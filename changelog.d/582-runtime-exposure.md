### Added — Runs record whether outside content has entered the conversation (#582)

Every `agent` run now keeps a small record of whether content from outside the conversation has entered it, and reports it as `Result.Exposure`. The record is always on and needs no option.

- `Exposure` says whether a record was kept (`Recorded`), whether foreign content was seen (`Foreign`), whether content of unknown origin was seen (`Unknown`), and the first source that did so (`First`, for example `tool:fetch` or `consult`).
- A tool result admits its origin after interceptors have judged it: foreign results set `Foreign`, results of unknown origin (a tool that declares none) set `Unknown`, and workspace, model, user and system content set nothing. A staged advisory set in `Request.Advisory` counts as foreign once accepted. A blocked result, a denied call, an unknown tool name and malformed arguments admit nothing, because none of that content reaches the model.
- `Request.PriorExposure` carries a record from an earlier turn. A request with history but no usable prior record starts as `Unknown` with `First` set to `history`, so continuing a conversation never claims a clean record it cannot vouch for.
- A run started inside a tool of another run shares that run's record, so exposure in a child run is visible on the parent's `Result.Exposure`.

By itself this changes no behaviour: nothing is sent to the model differently, and no call is gated on the record yet.
