### Security — Neutralize terminal controls in streamed Golem output (#433)

Golem visibly quotes terminal control characters in streamed answers, thinking, and tool-call echoes when writing to a terminal. Redirected output remains byte-for-byte unchanged.
