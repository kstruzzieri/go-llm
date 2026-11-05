package agentflow

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"unicode"
)

// minVersion is the AgentFlow baseline this adapter is validated against;
// maxMajor bounds it from above (#612: AgentFlow 1.x only). 1.1 ships the
// recovery Golem relies on: superseded timeouts verify (agentflow#60),
// next-action leads timed-out and replacement work to a close (#70), and
// ledger writers refuse closed attempts (#61). 1.2 is the release CI verifies:
// the next-action projection Golem's status and resume act on comes only from
// ledger rows AgentFlow validated, and is state_invalid when they change
// mid-call.
var minVersion = [2]int{1, 2}

const maxMajor = 1

// VersionError is a rejected AgentFlow version: unparseable, too old or too
// new. Its text is Golem's own plus the version AgentFlow printed, so status
// may report it where it hides runner errors (#612).
type VersionError struct{ msg string }

func (e *VersionError) Error() string { return e.msg }

// CheckVersion runs only `agentflow --version` and the version gate. Status
// calls it instead of Probe, then reads next-action, to stay read-only and
// cheap; Probe calls it first.
func (c *Client) CheckVersion(ctx context.Context) error {
	vout, err := c.version(ctx)
	if err != nil {
		return err
	}
	return checkVersion(vout)
}

// ProbeRecovery refuses an AgentFlow release whose next-action is known to
// mis-project recovery. AgentFlow 1.2.1 (agentflow#97) counts a changed
// file's latest receipt from any attempt, so a stopped attempt's unaccepted
// edit satisfies its replacement and resume would gate and complete the step
// on it. Resume, status and -review-manifest runs call it; fresh runs never
// retry a stopped attempt and keep the base gate. The exclusion is
// permanent: CheckVersion compares only major.minor and would admit 1.2.1
// again. The refusal is a VersionError, so JSON status may report its text.
func (c *Client) ProbeRecovery(ctx context.Context) error {
	vout, err := c.version(ctx)
	if err != nil {
		return err
	}
	return checkRecoveryVersion(vout)
}

func (c *Client) version(ctx context.Context) (string, error) {
	vout, stderr, exit, err := c.r.Run(ctx, []string{"--version"}, nil)
	if err != nil {
		return "", fmt.Errorf("agentflow unavailable (--version failed): %w", err)
	}
	if exit != 0 {
		// A broken install's own stderr is the diagnosis; keep it. Still not a
		// VersionError: that text is not Golem's, so JSON status stays silent.
		return "", fmt.Errorf("agentflow unavailable (--version failed): %w",
			&CommandError{Cmd: "--version", Exit: exit, Stderr: string(stderr)})
	}
	return string(vout), nil
}

// requiredSubcommands must all appear in `agentflow --help` for the P0 sequence.
var requiredSubcommands = []string{
	"init", "init-execution", "lock-plan", "record-file-change", "run",
	"finish-step", "finish-run", "next-step", "next-action", "doctor", "status",
}

type featureProbe struct {
	subcommand string
	needles    []string
}

var requiredFeatures = []featureProbe{
	{"init", []string{"--root"}},
	{"lock-plan", []string{"--from-json", "--json"}},
	{"init-execution", []string{"--root"}},
	{"doctor", []string{"--root", "--json"}},
	{"next-step", []string{"--root", "--json", "--explain"}}, // #731: --explain is 1.2
	{"claim-step", []string{"--root", "--agent", "--json"}},
	{"record-file-change", []string{"--root", "--step", "--attempt", "--path", "--agent", "--json"}},
	{"run", []string{"--root", "--step", "--attempt", "--gate", "--agent", "--confirm-risk"}},
	{"finish-step", []string{"--root", "--attempt", "--agent", "--json"}},
	{"block-step", []string{"--root", "--attempt", "--reason", "--agent", "--json"}},
	{"finish-run", []string{"--root", "--json"}},
	{"next-action", []string{"--root", "--json"}},
	{"status", []string{"--root"}}, // status intentionally has no --json in 0.4.x and 1.0
}

var requiredReviewFeatures = []featureProbe{
	{"record-review", []string{"--root", "--manifest", "--json"}},
	{"amend-step", []string{"--root", "--agent", "--reason", "--reason-code", "--finding", "--json"}},
}

var requiredWorkflowFeatures = []featureProbe{
	{"recommend-workflow", []string{"--stdin", "--json", "--selected-profile", "--reason"}},
	{"workflow-contract", []string{"--root", "--from-json"}},
}

var requiredParallelFeatures = []featureProbe{
	{"next-action", []string{"--agent"}},
	{"aggregate-ledgers", []string{"--input", "--source-id", "--output", "--base", "--dry-run", "--json"}},
}

// Probe fail-closes unless the CLI is present, new enough, and exposes every
// required subcommand and per-subcommand flag. Version alone is not trusted.
func (c *Client) Probe(ctx context.Context) error {
	if err := c.CheckVersion(ctx); err != nil {
		return err
	}
	hout, _, exit, err := c.r.Run(ctx, []string{"--help"}, nil)
	if err != nil || exit != 0 {
		return fmt.Errorf("agentflow unavailable (--help failed): %w", errOrExit(err, exit))
	}
	help := string(hout)
	var missing []string
	for _, s := range requiredSubcommands {
		if !helpHasToken(help, s) {
			missing = append(missing, s)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("agentflow is missing required subcommands: %s (upgrade Agentflow)",
			strings.Join(missing, ", "))
	}
	for _, feature := range requiredFeatures {
		sout, _, exit, err := c.r.Run(ctx, []string{feature.subcommand, "--help"}, nil)
		if err != nil || exit != 0 {
			return fmt.Errorf("agentflow %s --help failed: %w", feature.subcommand, errOrExit(err, exit))
		}
		usage := string(sout)
		for _, needle := range feature.needles {
			if !helpHasToken(usage, needle) {
				return fmt.Errorf("agentflow %s %s unavailable (upgrade Agentflow)",
					feature.subcommand, needle)
			}
		}
	}
	return nil
}

// ProbeReview checks only the optional review/amendment surface. Callers run
// the base Probe first, so ordinary task mode remains compatible with its
// existing Agentflow contract.
func (c *Client) ProbeReview(ctx context.Context) error {
	return c.probeOptionalFeatures(ctx, requiredReviewFeatures, "", "", "")
}

// ProbeWorkflow checks Agentflow's stable recommendation and contract-writing
// commands before Golem does model work or mutates proof state.
func (c *Client) ProbeWorkflow(ctx context.Context) error {
	return c.probeOptionalFeatures(ctx, requiredWorkflowFeatures, "workflow ", " (upgrade Agentflow)", " (upgrade Agentflow)")
}

// ProbeParallel checks the optional resumability surface used only by parallel
// worktree execution. Callers run Probe first, preserving serial compatibility.
func (c *Client) ProbeParallel(ctx context.Context) error {
	return c.probeOptionalFeatures(ctx, requiredParallelFeatures, "parallel ", " (upgrade Agentflow)",
		" (upgrade Agentflow)")
}

func (c *Client) probeOptionalFeatures(ctx context.Context, features []featureProbe, kind, missingSuffix, unavailableSuffix string) error {
	hout, _, exit, err := c.r.Run(ctx, []string{"--help"}, nil)
	if err != nil || exit != 0 {
		return fmt.Errorf("agentflow unavailable (--help failed): %w", errOrExit(err, exit))
	}
	help := string(hout)
	for _, feature := range features {
		if !helpHasToken(help, feature.subcommand) {
			return fmt.Errorf("agentflow is missing required %ssubcommand: %s%s", kind, feature.subcommand, missingSuffix)
		}
		sout, _, exit, err := c.r.Run(ctx, []string{feature.subcommand, "--help"}, nil)
		if err != nil || exit != 0 {
			return fmt.Errorf("agentflow %s --help failed: %w", feature.subcommand, errOrExit(err, exit))
		}
		for _, needle := range feature.needles {
			if !helpHasToken(string(sout), needle) {
				return fmt.Errorf("agentflow %s %s unavailable%s", feature.subcommand, needle, unavailableSuffix)
			}
		}
	}
	return nil
}

func helpHasToken(help, want string) bool {
	for _, token := range strings.FieldsFunc(help, func(r rune) bool {
		return r != '-' && r != '_' && !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		if token == want {
			return true
		}
	}
	return false
}

func errOrExit(err error, exit int) error {
	if err != nil {
		return err
	}
	return fmt.Errorf("exit %d", exit)
}

// versionEcho bounds the --version text a VersionError repeats; a real
// AgentFlow prints about 16 bytes.
func versionEcho(s string) string {
	if len(s) > 64 {
		return s[:64] + "..."
	}
	return s
}

func checkVersion(s string) error {
	_, _, major, minor, err := parseVersion(s)
	if err != nil {
		return err
	}
	if major < minVersion[0] || (major == minVersion[0] && minor < minVersion[1]) {
		return &VersionError{fmt.Sprintf("agentflow %d.%d is too old; need >= %d.%d", major, minor, minVersion[0], minVersion[1])}
	}
	if major > maxMajor {
		return &VersionError{fmt.Sprintf("agentflow %d.%d is newer than this Golem supports; need %d.x", major, minor, maxMajor)}
	}
	return nil
}

// parseVersion reads the version token and its numeric major.minor. Only the
// first non-empty line is the version; later lines (warnings, paths) are
// neither parsed nor echoed. It reads like "agentflow 1.0.0"; parts keeps
// anything after the second dot ("0rc1") unparsed for the caller.
func parseVersion(s string) (v string, parts []string, major, minor int, err error) {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	line = strings.TrimSpace(line)
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return "", nil, 0, 0, &VersionError{fmt.Sprintf("cannot parse agentflow version from %q", versionEcho(line))}
	}
	v = fields[len(fields)-1]
	if fields[0] == "agentflow" {
		v = fields[1]
	}
	parts = strings.SplitN(v, ".", 3)
	if len(parts) < 2 {
		return "", nil, 0, 0, &VersionError{fmt.Sprintf("cannot parse agentflow version %q", versionEcho(v))}
	}
	major, majorErr := strconv.Atoi(parts[0])
	minor, minorErr := strconv.Atoi(parts[1])
	// Atoi also accepts a leading sign; a version part is plain decimal.
	if majorErr != nil || minorErr != nil || strings.ContainsAny(parts[0]+parts[1], "+-") {
		return "", nil, 0, 0, &VersionError{fmt.Sprintf("cannot parse agentflow version %q", versionEcho(v))}
	}
	return v, parts, major, minor, nil
}

// checkRecoveryVersion refuses AgentFlow 1.2.1 by its leading patch digits, so
// every 1.2.1 build (1.2.1rc1, 1.2.1.post1, 1.2.1+local, 1.2.1.dev0, 1.2.01)
// is refused and 1.2.10 is not. A 1.2 version without a readable patch fails
// closed: it cannot be told apart from 1.2.1.
func checkRecoveryVersion(s string) error {
	v, parts, major, minor, err := parseVersion(s)
	if err != nil {
		return err
	}
	if major != 1 || minor != 2 {
		return nil
	}
	digits := ""
	if len(parts) == 3 {
		digits = parts[2][:len(parts[2])-len(strings.TrimLeft(parts[2], "0123456789"))]
	}
	patch, err := strconv.Atoi(digits)
	if err != nil {
		return &VersionError{fmt.Sprintf("cannot parse agentflow patch version %q; recovery refuses an AgentFlow 1.2 it cannot tell from 1.2.1", versionEcho(v))}
	}
	if patch == 1 {
		// Fixed text: the token reaches stderr raw in JSON status, and every
		// version refused here is a 1.2.1 build.
		return &VersionError{"agentflow 1.2.1 cannot drive recovery: its next-action counts a stopped attempt's file receipts toward the replacement attempt (agentflow#97); use AgentFlow 1.2.0 or a later fixed release"}
	}
	return nil
}
