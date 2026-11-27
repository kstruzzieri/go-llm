### Fixed — llm-bench corpus builders no longer delete files outside their output folder (#875)

The injection, forged-close and benign corpus builders removed stale trace files by treating the output path as a file pattern, so a path containing `[`, `*` or `?` could match a different folder and delete its trace files. They now list only the named folder. The benign-corpus report and registration check read their trace folder the same way.
