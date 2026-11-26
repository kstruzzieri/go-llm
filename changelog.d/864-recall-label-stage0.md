### Added — llm-bench recall-label measurement for Stage 0 (#864)

`llm-bench` can now test whether marking a recalled Golem memory record `files=differ`, meaning a file it was learned from has changed, makes a model stop repeating an outdated fact (re-reading the file, searching memory again or asking) without making it doubt facts that are still true. `-build-recall-corpus` builds the requests, `-calibrate-capture` captures the replies under a registration fixed in advance, and `-recall-report` reports the result.

A reply that an interrupted capture left failed after one recorded attempt, with no retry recorded, can be retried once by a later gap-fill capture, so an interruption no longer counts against the label. The registered decision rule now also states the checks the report already makes on the registration and the captures (the four Stage 0 models, the full set of test cases, the local endpoint, trial order and one reply per request).
