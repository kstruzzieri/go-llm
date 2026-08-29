package tools

// Cross-platform tests for the bwrap policy layer (#441): the broad-root
// predicate and (from Task 3) the argv builder run on every CI platform, the
// same discipline seatbelt.go uses for SBPL rendering.

import (
	"math"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestBwrapBroadRoot(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"/", true},
		{"/bin", true},
		{"/boot", true},
		{"/dev", true},
		{"/etc", true},
		{"/home", true},
		{"/lib", true},
		{"/lib32", true},
		{"/lib64", true},
		{"/lost+found", true},
		{"/media", true},
		{"/mnt", true},
		{"/nix", true},
		{"/opt", true},
		{"/proc", true},
		{"/root", true},
		{"/run", true},
		{"/sbin", true},
		{"/snap", true},
		{"/srv", true},
		{"/sys", true},
		{"/tmp", true},
		{"/usr", true},
		{"/var", true},
		{"/home/user", false},
		{"/home/user/proj", false},
		{"/usr/bin", false},
		{"/data", false},
		{"/workspaces", false},
		{"usr", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := bwrapBroadRoot(tc.path); got != tc.want {
			t.Errorf("bwrapBroadRoot(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

func validBwrapPolicy() bwrapPolicy {
	return bwrapPolicy{
		workspaceRoot:   "/home/u/proj",
		exePath:         "/usr/bin/go",
		chdir:           "/home/u/proj/sub",
		systemReadRoots: []string{"/usr/lib", "/usr/bin"},
		topLevelDirs:    []string{"/lib"},
		topLevelLinks:   map[string]string{"/bin": "usr/bin"},
		etcLiterals:     []string{"/etc/ld.so.cache"},
		payloadEnv:      []string{"PATH=/usr/bin", "HOME=/home/u", "TMPDIR=/tmp"},
		allowNetwork:    false,
		tmpfsSizeBytes:  536870912,
	}
}

func TestBuildBwrapArgsFullPolicy(t *testing.T) {
	got, err := buildBwrapArgs(validBwrapPolicy())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"--unshare-user", "--disable-userns", "--unshare-pid",
		"--unshare-ipc", "--unshare-uts", "--unshare-net",
		"--die-with-parent", "--new-session", "--cap-drop", "ALL",
		"--clearenv",
		"--setenv", "PATH", "/usr/bin",
		"--setenv", "HOME", "/home/u",
		"--setenv", "TMPDIR", "/tmp",
		"--proc", "/proc", "--dev", "/dev",
		"--size", "536870912", "--tmpfs", "/dev/shm",
		"--ro-bind", "/usr/bin", "/usr/bin",
		"--ro-bind", "/usr/lib", "/usr/lib",
		"--ro-bind", "/lib", "/lib",
		"--symlink", "usr/bin", "/bin",
		"--ro-bind", "/etc/ld.so.cache", "/etc/ld.so.cache",
		"--size", "536870912", "--tmpfs", "/tmp",
		"--bind", "/home/u/proj", "/home/u/proj",
		"--chdir", "/home/u/proj/sub",
		"--remount-ro", "/dev",
		"--remount-ro", "/",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("argv mismatch:\n got %q\nwant %q", got, want)
	}
}

func TestBuildBwrapArgsAllowNetworkOmitsUnshareNet(t *testing.T) {
	p := validBwrapPolicy()
	p.allowNetwork = true
	got, err := buildBwrapArgs(p)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"--unshare-user", "--disable-userns", "--unshare-pid",
		"--unshare-ipc", "--unshare-uts",
		"--die-with-parent", "--new-session", "--cap-drop", "ALL",
		"--clearenv",
		"--setenv", "PATH", "/usr/bin",
		"--setenv", "HOME", "/home/u",
		"--setenv", "TMPDIR", "/tmp",
		"--proc", "/proc", "--dev", "/dev",
		"--size", "536870912", "--tmpfs", "/dev/shm",
		"--ro-bind", "/usr/bin", "/usr/bin",
		"--ro-bind", "/usr/lib", "/usr/lib",
		"--ro-bind", "/lib", "/lib",
		"--symlink", "usr/bin", "/bin",
		"--ro-bind", "/etc/ld.so.cache", "/etc/ld.so.cache",
		"--size", "536870912", "--tmpfs", "/tmp",
		"--bind", "/home/u/proj", "/home/u/proj",
		"--chdir", "/home/u/proj/sub",
		"--remount-ro", "/dev",
		"--remount-ro", "/",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("argv mismatch:\n got %q\nwant %q", got, want)
	}
}

func TestBuildBwrapArgsNoCapOmitsSizes(t *testing.T) {
	p := validBwrapPolicy()
	p.tmpfsSizeBytes = 0
	got, err := buildBwrapArgs(p)
	if err != nil {
		t.Fatal(err)
	}
	joined := " " + strings.Join(got, " ") + " "
	if strings.Contains(joined, " --size ") {
		t.Fatalf("uncapped policy emitted --size: %q", got)
	}
	for _, tmpfs := range []string{" --tmpfs /dev/shm ", " --tmpfs /tmp "} {
		if !strings.Contains(joined, tmpfs) {
			t.Fatalf("uncapped policy lost private mount %q: %q", tmpfs, got)
		}
	}
}

func TestBuildBwrapArgsSplitLayoutDirs(t *testing.T) {
	p := validBwrapPolicy()
	p.topLevelDirs = []string{"/lib64", "/bin"}
	p.topLevelLinks = nil
	got, err := buildBwrapArgs(p)
	if err != nil {
		t.Fatal(err)
	}
	joined := " " + strings.Join(got, " ") + " "
	for _, bind := range []string{" --ro-bind /bin /bin ", " --ro-bind /lib64 /lib64 "} {
		if !strings.Contains(joined, bind) {
			t.Fatalf("split layout bind missing %q: %q", bind, got)
		}
	}
	if strings.Contains(joined, "--symlink") {
		t.Fatalf("split layout emitted symlinks: %q", got)
	}
}

func TestBuildBwrapArgsBindsCanonicalRootAtAliasDestination(t *testing.T) {
	p := validBwrapPolicy()
	p.systemReadRoots = []string{"/usr/lib"}
	p.systemRootAliases = map[string]string{"/usr/lib64": "/usr/lib"}
	p.topLevelDirs = nil
	p.topLevelLinks = map[string]string{"/lib64": "usr/lib64"}

	got, err := buildBwrapArgs(p)
	if err != nil {
		t.Fatal(err)
	}
	joined := " " + strings.Join(got, " ") + " "
	want := " --ro-bind /usr/lib /usr/lib64 --symlink usr/lib64 /lib64 "
	if !strings.Contains(joined, want) {
		t.Fatalf("buildBwrapArgs(alias) = %q, want ordered sequence %q", got, want)
	}
	if bad := " --ro-bind /usr/lib64 /usr/lib64 "; strings.Contains(joined, bad) {
		t.Fatalf("buildBwrapArgs(alias) uses mutable symlink source %q: %q", bad, got)
	}
}

func TestBuildBwrapArgsDeterministicUnderShuffledInputs(t *testing.T) {
	a := validBwrapPolicy()
	a.systemReadRoots = []string{"/usr/lib", "/usr/bin", "/usr/share", "/usr/lib"}
	a.etcLiterals = []string{"/etc/ld.so.conf", "/etc/ld.so.cache"}
	b := validBwrapPolicy()
	b.systemReadRoots = []string{"/usr/share", "/usr/bin", "/usr/lib"}
	b.etcLiterals = []string{"/etc/ld.so.cache", "/etc/ld.so.conf"}
	gotA, errA := buildBwrapArgs(a)
	gotB, errB := buildBwrapArgs(b)
	if errA != nil || errB != nil {
		t.Fatalf("errs: %v %v", errA, errB)
	}
	if !slices.Equal(gotA, gotB) {
		t.Fatalf("shuffled inputs changed output:\n a %q\n b %q", gotA, gotB)
	}
}

func TestBuildBwrapArgsRejections(t *testing.T) {
	cases := map[string]struct {
		mutate func(*bwrapPolicy)
		frag   string
	}{
		"broad workspace":        {func(p *bwrapPolicy) { p.workspaceRoot = "/usr" }, "workspace"},
		"relative workspace":     {func(p *bwrapPolicy) { p.workspaceRoot = "home/u/proj" }, "workspace"},
		"non-clean workspace":    {func(p *bwrapPolicy) { p.workspaceRoot = "/home/u/../u/proj" }, "workspace"},
		"non-clean executable":   {func(p *bwrapPolicy) { p.exePath = "/usr//bin/go" }, "executable"},
		"broad executable":       {func(p *bwrapPolicy) { p.exePath = "/usr" }, "executable"},
		"empty chdir":            {func(p *bwrapPolicy) { p.chdir = "" }, "chdir"},
		"chdir outside ws":       {func(p *bwrapPolicy) { p.chdir = "/home/u/other" }, "chdir"},
		"chdir prefix trick":     {func(p *bwrapPolicy) { p.chdir = "/home/u/proj-evil" }, "chdir"},
		"broad system root":      {func(p *bwrapPolicy) { p.systemReadRoots = []string{"/usr"} }, "system read root"},
		"depth-1 system root":    {func(p *bwrapPolicy) { p.systemReadRoots = []string{"/data"} }, "two components deep"},
		"root covers workspace":  {func(p *bwrapPolicy) { p.systemReadRoots = []string{"/home/u"} }, "covers the workspace"},
		"relative system root":   {func(p *bwrapPolicy) { p.systemReadRoots = []string{"usr/lib"} }, "system read root"},
		"ws equals system root":  {func(p *bwrapPolicy) { p.systemReadRoots = []string{"/home/u/proj"} }, "covers the workspace"},
		"alias dest off-list":    {func(p *bwrapPolicy) { p.systemRootAliases = map[string]string{"/opt/lib64": "/usr/lib"} }, "alias destination"},
		"alias source missing":   {func(p *bwrapPolicy) { p.systemRootAliases = map[string]string{"/usr/lib64": "/opt/lib"} }, "uncollected source"},
		"alias dest canonical":   {func(p *bwrapPolicy) { p.systemRootAliases = map[string]string{"/usr/lib": "/usr/lib"} }, "already canonical"},
		"etc outside /etc":       {func(p *bwrapPolicy) { p.etcLiterals = []string{"/home/u/x"} }, "/etc"},
		"etc root itself":        {func(p *bwrapPolicy) { p.etcLiterals = []string{"/etc"} }, "/etc"},
		"layout dir off-list":    {func(p *bwrapPolicy) { p.topLevelDirs = []string{"/opt"} }, "layout"},
		"link dest off-list":     {func(p *bwrapPolicy) { p.topLevelLinks = map[string]string{"/opt": "usr/opt"} }, "layout"},
		"link target absolute":   {func(p *bwrapPolicy) { p.topLevelLinks = map[string]string{"/bin": "/usr/bin"} }, "layout"},
		"link target empty":      {func(p *bwrapPolicy) { p.topLevelLinks = map[string]string{"/bin": ""} }, "layout"},
		"layout dest both forms": {func(p *bwrapPolicy) { p.topLevelDirs = []string{"/bin"} }, "layout"},
		"negative tmpfs size":    {func(p *bwrapPolicy) { p.tmpfsSizeBytes = -1 }, "tmpfs"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			p := validBwrapPolicy()
			tc.mutate(&p)
			if _, err := buildBwrapArgs(p); err == nil {
				t.Fatalf("accepted invalid policy")
			} else if !strings.Contains(err.Error(), tc.frag) {
				t.Fatalf("error %q must mention %q", err, tc.frag)
			}
		})
	}
}

func TestBuildBwrapArgsPayloadEnvRejections(t *testing.T) {
	cases := map[string][]string{
		"malformed entry": {"PATH"},
		"empty name":      {"=x"},
		"duplicate name":  {"PATH=/usr/bin", "PATH=/bin"},
		"non-allowlisted": {"LD_PRELOAD=/evil.so"},
		"lowercase name":  {"path=/usr/bin"},
	}
	for name, env := range cases {
		t.Run(name, func(t *testing.T) {
			p := validBwrapPolicy()
			p.payloadEnv = env
			if _, err := buildBwrapArgs(p); err == nil {
				t.Fatal("accepted invalid payload env")
			} else if !strings.Contains(err.Error(), "payload env") {
				t.Fatalf("error %q must mention payload env", err)
			}
		})
	}
}

func TestBwrapMemoryCapBytes(t *testing.T) {
	if got, err := bwrapMemoryCapBytes(0); err != nil || got != 0 {
		t.Fatalf("zero cap = (%d, %v), want (0, nil)", got, err)
	}
	if got, err := bwrapMemoryCapBytes(256); err != nil || got != 268435456 {
		t.Fatalf("256MiB cap = (%d, %v), want (268435456, nil)", got, err)
	}
	// Literal expectations, never derived from the bound under test: a
	// self-referential bound cannot detect a wrong (or arch-dependent) one.
	// 4 GiB must be accepted on every architecture, including 32-bit where
	// math.MaxInt/(1024*1024) is only 2047.
	if got, err := bwrapMemoryCapBytes(4096); err != nil || got != 4294967296 {
		t.Fatalf("4096MiB cap = (%d, %v), want (4294967296, nil) on every arch", got, err)
	}
	if strconv.IntSize == 64 {
		overflowMB := int64(math.MaxInt64/(1024*1024) + 1)
		if _, err := bwrapMemoryCapBytes(int(overflowMB)); err == nil {
			t.Fatal("overflowing cap accepted")
		}
	}
	if _, err := bwrapMemoryCapBytes(-1); err == nil {
		t.Fatal("negative cap accepted")
	}
}

func TestBwrapConfigSupport(t *testing.T) {
	if err := bwrapConfigSupport(SandboxConfig{Runtime: SandboxRuntimeBwrap, CPULimit: 0.5}); err == nil ||
		!strings.Contains(err.Error(), "cannot enforce CPULimit") {
		t.Fatalf("CPULimit must be rejected: %v", err)
	}
	if err := bwrapConfigSupport(SandboxConfig{
		Runtime: SandboxRuntimeBwrap, MemoryCapMB: 4096,
	}); err != nil {
		t.Fatalf("4 GiB cap must be supported on every arch: %v", err)
	}
	if err := bwrapConfigSupport(SandboxConfig{
		Runtime: SandboxRuntimeBwrap, MemoryCapMB: 512, AllowNetwork: true,
		DropCaps: []string{"NET_RAW"},
	}); err != nil {
		t.Fatalf("supported config rejected: %v", err)
	}
}

// TestBuildBwrapArgsExeBindOnlyWhenUncovered pins the executable-literal
// rule: a bind into an already read-only-mounted region fails mountpoint
// creation and is redundant, so it is emitted only for workspace-resident
// and out-of-policy executables.
func TestBuildBwrapArgsExeBindOnlyWhenUncovered(t *testing.T) {
	cases := map[string]struct {
		exe  string
		want bool
	}{
		"under system root":  {"/usr/bin/go", false},
		"under layout dir":   {"/lib/ld-musl.so", false},
		"under layout link":  {"/bin/true", false},
		"inside workspace":   {"/home/u/proj/tool", true},
		"out-of-policy path": {"/data/tools/custom", true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			p := validBwrapPolicy()
			p.exePath = tc.exe
			got, err := buildBwrapArgs(p)
			if err != nil {
				t.Fatal(err)
			}
			joined := " " + strings.Join(got, " ") + " "
			has := strings.Contains(joined, " --ro-bind "+tc.exe+" "+tc.exe+" ")
			if has != tc.want {
				t.Fatalf("exe bind present=%v, want %v: %q", has, tc.want, got)
			}
		})
	}
}

// TestBwrapReviewedPolicySets pins the compiled-in reviewed mount policy:
// any widening (or narrowing) of the execution surface must show up as an
// explicit test change, and every fixed-layout entry's usr-merge target must
// be present in the reviewed roots so benign standard layouts pass the
// collector's covered-target check.
func TestBwrapReviewedPolicySets(t *testing.T) {
	wantRoots := []string{
		"/usr/bin", "/usr/sbin", "/usr/lib", "/usr/lib32", "/usr/lib64",
		"/usr/libexec", "/usr/share",
	}
	if !slices.Equal(bwrapDefaultSystemRoots, wantRoots) {
		t.Fatalf("reviewed system roots changed: %q", bwrapDefaultSystemRoots)
	}
	wantLayout := []string{"/bin", "/sbin", "/lib", "/lib32", "/lib64"}
	if !slices.Equal(bwrapLayoutDirs, wantLayout) {
		t.Fatalf("fixed layout set changed: %q", bwrapLayoutDirs)
	}
	for _, dir := range bwrapLayoutDirs {
		if !slices.Contains(bwrapDefaultSystemRoots, "/usr"+dir) {
			t.Fatalf("layout entry %q lacks its usr-merge target /usr%s in the reviewed roots", dir, dir)
		}
	}
	wantEtc := []string{
		"/etc/ld.so.cache", "/etc/ld.so.conf", "/etc/ld.so.conf.d",
		"/etc/alternatives",
	}
	if !slices.Equal(bwrapEtcLiterals, wantEtc) {
		t.Fatalf("reviewed /etc literals changed: %q", bwrapEtcLiterals)
	}
	wantNetEtc := []string{
		"/etc/resolv.conf", "/etc/hosts", "/etc/nsswitch.conf",
		"/etc/ssl/certs", "/etc/ssl/cert.pem", "/etc/pki/tls/certs",
		"/etc/pki/ca-trust/extracted",
	}
	if !slices.Equal(bwrapNetworkEtcLiterals, wantNetEtc) {
		t.Fatalf("reviewed network /etc literals changed: %q", bwrapNetworkEtcLiterals)
	}
}

// TestBuildBwrapArgsRemountsDevReadOnly pins the /dev sink fix: --dev creates
// its own tmpfs and --size attaches to the following --tmpfs (/dev/shm), so
// without this remount a payload can fill host RAM through /dev/<file> under
// an approved memory cap, bounded by neither RLIMIT_AS nor either quota.
func TestBuildBwrapArgsRemountsDevReadOnly(t *testing.T) {
	got, err := buildBwrapArgs(validBwrapPolicy())
	if err != nil {
		t.Fatal(err)
	}
	joined := " " + strings.Join(got, " ") + " "
	if !strings.Contains(joined, " --remount-ro /dev --remount-ro / ") {
		t.Fatalf("/dev is not remounted read-only before the root remount: %q", got)
	}
}
