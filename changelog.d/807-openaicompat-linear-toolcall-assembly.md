### Fixed — openai-compat assembles streamed tool-call arguments in linear time (#807)

A tool call that an OpenAI-compatible server streams in many small fragments used to cost more to assemble the longer it got: every fragment copied the argument text received so far and re-normalized every tool call in the stream, so the cost grew with the square of the fragment count. One 1 MiB tool call in 10,000 fragments took 13 seconds and 24 GB of short-lived allocation to assemble; it now takes 7 milliseconds and 7.6 MB.

- `ChatStream` hands callers the same values as before: the same `ToolCall.Function.Arguments` on the Done chunk, the same partial tool calls on the Done+Partial chunk after cancellation, and no Done chunk on a failed stream.
- Nothing to change in calling code, and no new option.
- There is still no limit on how large a streamed tool call may be; a server decides how much the client holds until the stream ends.
