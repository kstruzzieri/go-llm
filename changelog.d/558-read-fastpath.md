### Changed — point reads on APFS no longer list every ancestor directory (#558)

Workspace opens by name (`read_file`, `list`, scoped opens, and the target
lookup of `write`, `edit` and undo) proved each path component's on-disk
spelling by listing its parent directory, so one read cost time proportional
to the size of every ancestor: about 100 ms with 100,000 siblings. On APFS the
spelling now comes from the stored name the filesystem reports for the
object, bound to that object's identity, so a read takes about a tenth of a
millisecond regardless of directory size (measured: 78-142 us from 3,000 to
100,000 siblings, against 1.6-112 ms before). Policy sees the same spelling, and
directories that cannot be listed are still denied. Linux, other
filesystems, mount points, multiply-linked files and other entry types keep
the listing, including APFS volume-group mount points that share a logical
device ID with their parent.
