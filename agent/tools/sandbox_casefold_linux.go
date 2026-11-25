//go:build linux

package tools

import (
	"unsafe"

	"golang.org/x/sys/unix"
)

// postRunCollect makes the before-walk record what only the post-run check
// uses (sandboxGitLayout.collect); the check runs on Linux.
var postRunCollect = true

// fsCasefoldFL is the inode flag (FS_CASEFOLD_FL, linux/fs.h) of a directory
// whose names the file system compares with case folded (ext4, f2fs, tmpfs).
const fsCasefoldFL = 0x40000000

// linux/xfs_fs.h defines XFS_IOC_FSGEOMETRY as _IOR('X', 126, struct xfs_fsop_geom).
// FS_IOC_GETFLAGS supplies the architecture-specific _IOC_READ direction bits.
const xfsIOCGeometry = unix.FS_IOC_GETFLAGS&0xc0000000 | 256<<16 | 'X'<<8 | 126

// xfsDirV2CI is XFS_FSOP_GEOM_FLAGS_DIRV2CI, the filesystem-wide ascii-ci flag.
const xfsDirV2CI = 1 << 12

// xfsGeometry preserves the xfs_fsop_geom ABI around its flags field at byte 92.
type xfsGeometry struct {
	_     [92]byte
	flags uint32
	_     [160]byte
}

// The ioctl encodes the kernel structure's size in its request number.
var _ [256]byte = [unsafe.Sizeof(xfsGeometry{})]byte{}

// dirProbe answers what the gate (mayFoldNames) needs about one directory.
type dirProbe interface {
	dev() (uint64, error)   // st_dev
	fsType() (int64, error) // statfs f_type
	flags() (uint32, error) // FS_IOC_GETFLAGS
	xfsGeometryFlags() (uint32, error)
	close()
}

// fdProbe is a dirProbe over one descriptor of the directory.
type fdProbe int

// dev is the directory's st_dev (fstat).
func (p fdProbe) dev() (uint64, error) {
	var st unix.Stat_t
	err := unix.Fstat(int(p), &st)
	return uint64(st.Dev), err //nolint:unconvert // Dev's width differs by architecture
}

// fsType is the file system's f_type (fstatfs).
func (p fdProbe) fsType() (int64, error) {
	var st unix.Statfs_t
	err := unix.Fstatfs(int(p), &st)
	return int64(st.Type), err //nolint:unconvert // Type's width differs by architecture
}

// flags reads the directory's inode flags (FS_IOC_GETFLAGS).
func (p fdProbe) flags() (uint32, error) { return unix.IoctlGetUint32(int(p), unix.FS_IOC_GETFLAGS) }

// xfsGeometryFlags reads the filesystem geometry through the open directory.
func (p fdProbe) xfsGeometryFlags() (uint32, error) {
	var geom xfsGeometry
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(p), xfsIOCGeometry, uintptr(unsafe.Pointer(&geom)))
	if errno != 0 {
		return 0, errno
	}
	return geom.flags, nil
}

// close releases the descriptor.
func (p fdProbe) close() { _ = unix.Close(int(p)) }

// openDirProbe opens dir, without following a symbolic link, for the gate. A
// test replaces it to give a directory a file system type and flags.
var openDirProbe = func(dir string) (dirProbe, error) {
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return fdProbe(fd), nil
}

const (
	lookupNames fsLookupPolicy = iota
	exactNames
	checkInodeFlags
)

// deviceLookupPolicy reads the properties shared by all directories on a device.
func deviceLookupPolicy(p dirProbe) fsLookupPolicy {
	t, err := p.fsType()
	if err != nil {
		return lookupNames
	}
	switch t {
	case unix.EXT4_SUPER_MAGIC, unix.F2FS_SUPER_MAGIC, unix.TMPFS_MAGIC:
		return checkInodeFlags
	case unix.BTRFS_SUPER_MAGIC:
		return exactNames
	case unix.XFS_SUPER_MAGIC:
		flags, err := p.xfsGeometryFlags()
		if err == nil && flags&xfsDirV2CI == 0 {
			return exactNames
		}
	}
	return lookupNames
}

// mayFoldNames reports whether a lookup in dir can resolve a name to another
// spelling, so the walks must look git's names up there (lookupGitNames).
// Each walk caches the device policy by st_dev and reads inode flags per
// directory on filesystems that support per-directory case folding.
//
// Outside the lookups a listing is exact, and visit matches it. The probe
// opens dir by path, as the walks read it: a process that swaps an ancestor
// while the walk runs can mislead it as it can the walk itself, which is the
// documented limit of what runs during a command.
func (l *sandboxGitLayout) mayFoldNames(dir string) bool {
	p, err := openDirProbe(dir)
	if err != nil {
		return true
	}
	defer p.close()
	dev, err := p.dev()
	if err != nil {
		return true
	}
	policy, ok := l.lookupPolicies[dev]
	if !ok {
		policy = deviceLookupPolicy(p)
		if l.lookupPolicies == nil {
			l.lookupPolicies = make(map[uint64]fsLookupPolicy)
		}
		l.lookupPolicies[dev] = policy
	}
	if policy == checkInodeFlags {
		f, err := p.flags()
		return err != nil || f&fsCasefoldFL != 0
	}
	return policy == lookupNames
}
