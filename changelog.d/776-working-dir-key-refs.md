### Security — Working-directory configs can no longer spend your environment secrets (#776)

A `models.json` found only because it sits in the current directory could name any environment variable in `api_key` (`"${AWS_SECRET_ACCESS_KEY}"`) and a `base_url` of its choosing, and go-llm would send that secret there. Four changes close it.

#### Behavior changes

- A config found by working-directory discovery now refuses every `${NAME}` `api_key` reference with the new error code `key_reference_refused` (subject: the provider), before any environment variable is read. A malformed reference is refused the same way. To use a file's references, name it: `golem -config ./models.json`, `go-llm-mcp -config ./models.json`, or set `GO_LLM_CONFIG` for one run (`GO_LLM_CONFIG=./models.json golem ...`). Do not export a relative `GO_LLM_CONFIG`: it would trust the `models.json` in every directory you run from. Saving the config to another path or as a profile lifts the refusal; saving it back to its own path keeps it. Literal keys, the user config (including when you run from inside its directory), `GO_LLM_CONFIG` and explicit paths are unchanged.
- Golem prints `pass -config ./models.json to use this file's key references, or set GO_LLM_CONFIG`; `golem ops` prints the code, the provider and that sentence; `go-llm-mcp` names its `-config` flag; `llm-bench` fails on the refusal (`set GO_LLM_CONFIG or pass -judge-model`) instead of silently falling back to its built-in judge.
- An `openai-compat` provider with an `api_key` never runs the loopback discovery scan (ports 8080-8090). Only its configured URL is probed, through the destination guard. If that URL does not serve the model, Golem prints `openai-compat backend discovery skipped: provider "<key>" has an api_key; pass -base-url to use another port`. Keyless providers still discover a moved local backend.
- The destination consent screen adds `sends api_key from $NAME` (several: `$A, $B`) or `sends a literal api_key` under each destination whose provider sends a key. This extends the #477 consent field set.
- The openai-compat client replaces its own key with `[redacted]` in error text built from a server response.

#### For consumers that mirror the error codes

`key_reference_refused` joins the closed `config` error-code list. Firn (kstruzzieri/firn-ide#524) must add it before it bumps go-llm.
