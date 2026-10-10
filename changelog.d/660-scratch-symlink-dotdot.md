### Fixed — scratch snapshots resolve `..` in symlink targets the way the host does (#660)

A scratch snapshot cleaned symlink targets lexically. With `a/s -> deep/er`, a
link `a/l -> s/../../x` resolves to `a/x` on the host, but its clone pointed at
the workspace-root `x`, so commands in scratch mode read a different file
through it. When the host resolves every prefix a `..` pops, the snapshot now
applies that `..` to the physical parent, for relative and absolute targets and
for dangling external chains, and an executable or working directory reached
through such a link is accepted instead of failing closed. Links the host
cannot resolve (including chains beyond the host's symlink traversal limit),
and links without `..`, are rewritten exactly as before. A
dangling external chain that nevertheless names an existing directory or
multiply linked file now fails the first snapshot pass with the gate's reason,
instead of being linked into the reference tree and failing one pass later.
