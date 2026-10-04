package mcpclient

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// launchEnv carries the process facts preparation reads. Production uses
// hostLaunchEnv; tests inject their own through connectHooks.
type launchEnv struct {
	lookup   func(string) (string, bool)
	lookPath func(string) (string, error)
	getwd    func() (string, error)
	policy   envPolicy
}

func hostLaunchEnv() launchEnv {
	return launchEnv{lookup: os.LookupEnv, lookPath: exec.LookPath, getwd: os.Getwd, policy: hostEnvPolicy()}
}

// preparedServer is one server's frozen policy: the transport that will be
// connected and the identity that is fingerprinted. Connect, Inspect and
// Approve prepare each server exactly once and launch only this value.
type preparedServer struct {
	alias     string
	identity  connectionIdentity
	transport gomcp.Transport
	refusals  *httpRefusals // HTTP only
}

// refusal names an HTTP policy refusal observed on this transport, if any.
func (p preparedServer) refusal() string {
	switch {
	case p.refusals == nil:
		return ""
	case p.refusals.redirect.Load():
		return "redirect_refused"
	case p.refusals.destination.Load():
		return "destination_refused"
	}
	return ""
}

// prepare freezes s (spec §5.3). Nothing is launched or contacted here;
// per-alias failures are *AdmissionError values.
func prepare(s Server, workspace string, le launchEnv) (preparedServer, error) {
	p := preparedServer{alias: s.Alias, identity: connectionIdentity{workspace: workspace, alias: s.Alias}}
	switch s.kind {
	case transportStdio:
		return prepareStdio(p, s, le)
	case transportHTTP:
		return prepareHTTP(p, s)
	default:
		return preparedServer{}, admissionFailure(s.Alias, "invalid_config", errors.New("mcpclient: unknown transport"))
	}
}

func prepareStdio(p preparedServer, s Server, le launchEnv) (preparedServer, error) {
	argv := append([]string(nil), s.command...)
	p.identity.kind = "stdio"
	p.identity.envBaseline = le.policy.id
	p.identity.env = envIdentity(s.env, le.policy)
	p.identity.argv = argv
	if s.tr != nil {
		// Test-only transport: identity fields are taken verbatim and nothing
		// is resolved or launched from the filesystem.
		if len(argv) > 0 {
			p.identity.launcher, p.identity.target = argv[0], argv[0]
		}
		p.identity.dir = s.dir
		p.transport = s.tr
		return p, nil
	}
	invalid := func(detail string, cause error) (preparedServer, error) {
		return preparedServer{}, launchInvalid(s.Alias, detail, cause)
	}
	dir := s.dir
	if dir == "" {
		wd, err := le.getwd()
		if err != nil {
			return invalid("mcpclient: cannot determine the working directory", err)
		}
		dir = wd
	}
	const notDir = "mcpclient: working directory is not an existing directory"
	// Resolving symlinks freezes the actual directory: retargeting a link
	// later changes the identity instead of silently moving the server.
	dir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return invalid(notDir, err)
	}
	if info, statErr := os.Stat(dir); statErr != nil || !info.IsDir() {
		return invalid(notDir, statErr)
	}
	// Validation rejects an empty command; an unvalidated one reaches
	// resolveLauncher's empty-executable refusal instead of panicking here.
	var argv0 string
	if len(argv) > 0 {
		argv0 = argv[0]
	}
	launcher, err := resolveLauncher(argv0, dir, le.lookPath)
	if err != nil {
		return invalid(err.Error(), err) // fixed text by construction
	}
	target, err := filepath.EvalSymlinks(launcher)
	if err != nil {
		return invalid("mcpclient: executable symlink target cannot be resolved", err)
	}
	env, unset := buildServerEnv(le.policy, s.env, le.lookup)
	if len(unset) > 0 {
		failure := admissionFailure(s.Alias, "env_unset", nil)
		failure.Names = unset
		return preparedServer{}, failure
	}
	p.identity.launcher, p.identity.target, p.identity.dir = launcher, target, dir
	// The launcher, not the target, is executed: some programs locate their
	// installation from the invoked path (a virtualenv's bin/python). The
	// process lifetime belongs to the session (Manager.Close), not a setup
	// context, so this is exec.Cmd rather than exec.CommandContext.
	p.transport = &gomcp.CommandTransport{Command: &exec.Cmd{Path: launcher, Args: argv, Dir: dir, Env: env}}
	return p, nil
}

func prepareHTTP(p preparedServer, s Server) (preparedServer, error) {
	p.identity.kind = "http"
	if s.tr != nil {
		p.identity.endpoint = s.endpoint
		p.transport = s.tr
		return p, nil
	}
	ep, err := canonicalEndpoint(s.endpoint)
	if err != nil {
		return preparedServer{}, admissionFailure(s.Alias, "invalid_config", err)
	}
	p.identity.origin = ep.origin
	p.identity.endpoint = strings.TrimPrefix(ep.sdkURL(), ep.origin)
	p.transport, p.refusals = newHTTPTransport(ep)
	return p, nil
}

// invalidIdentity is the launch_invalid detail for an identity that cannot be
// fingerprinted (argv or a path that is not valid UTF-8).
const invalidIdentity = "mcpclient: connection identity is not valid UTF-8"

// launchInvalid blocks alias as an unusable launch. detail is fixed text an
// operator can act on, never a path, argv or OS error; the cause stays
// reachable through errors.Is and errors.As.
func launchInvalid(alias, detail string, cause error) error {
	failure := admissionFailure(alias, "launch_invalid", cause)
	if failure.Reason != "launch_invalid" {
		return failure // canceled, say: the detail would misname it
	}
	return fmt.Errorf("%w: %s", failure, detail)
}

// resolveLauncher resolves argv0 to the absolute path that will be executed.
// Every form goes through lookPath, so Windows PATHEXT resolution here matches
// exec.Cmd.Start's own re-resolution of the same path. argv0 is cleaned
// lexically before lookup and lookPath's result is returned unchanged, so the
// path checked is exactly the path executed; a symlink followed by ".."
// therefore resolves lexically, not as the kernel would. Errors are fixed text.
func resolveLauncher(argv0, dir string, lookPath func(string) (string, error)) (string, error) {
	if argv0 == "" {
		return "", errors.New("mcpclient: empty executable")
	}
	name := argv0
	if !filepath.IsAbs(name) && (strings.ContainsRune(name, filepath.Separator) || strings.ContainsRune(name, '/')) {
		name = filepath.Join(dir, name)
	}
	resolved, err := lookPath(filepath.Clean(name))
	if errors.Is(err, exec.ErrDot) {
		return "", errors.New("mcpclient: executable resolves relative to the current directory")
	}
	if err != nil {
		return "", errors.New("mcpclient: executable not found or not executable")
	}
	if !filepath.IsAbs(resolved) {
		return "", errors.New("mcpclient: executable did not resolve to an absolute path")
	}
	return resolved, nil
}
