### Fixed — -agentflow-resume says what to do when a retried attempt leaves files unrecorded (#774)

A retried step stops before its gates when the new attempt leaves an in-scope
file unrecorded. That happens when a model finds a stopped attempt's edit already
right and writes nothing. The error used to read
`agentflow resumed step "P1" reached state "file_receipts_missing" before gates, want validation_missing`,
which named no file and no next step. It now reads:

```text
step P1 attempt A2 ended without recording its changed files, so no gate ran and the attempt stays open (unrecorded: src/answer.txt). To keep the files as they are, run the record-file-change command -agentflow-status shows for each one, then resume. To discard them, block the open attempt with agentflow block-step, then restore them, then resume; restoring first would settle the attempt on its gates without running the model
```

A second `-agentflow-resume` that finds the same state keeps its
`blocked by missing file receipts` text and adds the same files and ways out. Exit
codes, JSON output and `-agentflow-status` are unchanged. A script that matches
the old text must change.
