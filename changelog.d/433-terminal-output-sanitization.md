### Security — Neutralize terminal controls in streamed Golem output (#433)

Golem visibly quotes terminal control characters in streamed answers, thinking, tool-call echoes, and result summaries when writing to a terminal, and in the `-p` answer when stdout is a terminal. Tabs and CRLF line ends in model text pass through. Redirected output remains byte-for-byte unchanged.
