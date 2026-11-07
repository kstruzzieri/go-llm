### Fixed — -agentflow-resume tells a retried attempt which files a stopped attempt left behind (#774)

When an earlier attempt of a step stopped after changing in-scope files,
resume gave the next attempt the same instructions as the first. A model that
found the content already right often wrote nothing, and the step stopped before
its gates. After resume claims the new attempt, it now reads `next-action` once
more and adds a `Retry context` section that lists those files and says how to
keep each one: fix a file that is wrong, and confirm one that is already right
with an `edit_file` whose `old_string` and `new_string` are the same unique text,
which keeps the bytes and records the receipt.

If that read fails, the model does not run and the attempt is blocked as a
failed run is, with `golem: agent run failed`. Fresh `-plan` runs are
unchanged. A model can still leave a file untouched; resume then stops with the
message from the earlier #774 fix.
