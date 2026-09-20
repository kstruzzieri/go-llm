package consult

import "context"

// runCodexAppServer leaves host facts separate so the public caller can apply
// launched-process precedence before interpreting an exchange failure. It never
// returns raw RPC bytes, identifiers, metadata, or a partially admitted answer.
func runCodexAppServer(ctx context.Context, spec runSpec, model string, disabledMCPServers ...string) (runOutcome, appServerResult, error) {
	if !supportedModel(codexAdapter, model) {
		return runOutcome{}, appServerResult{}, errAppServerProtocol
	}
	if spec.stdin == "" {
		return runOutcome{}, appServerResult{}, errStdinInvalid
	}
	s := &appServerStream{model: model, prompt: spec.stdin}
	spec.adapter = codexAdapter
	spec.args = codexAppServerArgs(disabledMCPServers...)
	out, err := runDuplex(ctx, spec, duplexExchange{start: s.start, receive: s.receive, interrupt: s.interrupt})
	out.Stdout = nil
	if err != nil {
		return out, appServerResult{}, err
	}
	if out.CapExceeded || out.Canceled || out.TimedOut || !out.GroupCleanupOK || !out.CleanupOK || out.WaitErrorKind != "none" || out.ExitCode != 0 {
		return out, appServerResult{}, errAppServerIncomplete
	}
	facts, err := s.finish()
	return out, facts, err
}

func codexAppServerArgs(disabledMCPServers ...string) []string {
	args := []string{
		"-c", `approval_policy="never"`, "-c", `approvals_reviewer="user"`,
		"-c", "project_doc_max_bytes=0", "-c", `web_search="disabled"`,
		"-c", "features.hooks=false", "-c", "features.apps=false", "-c", "features.plugins=false",
		"-c", "features.external_agent_memory_import=false", "-c", "features.memories=false",
		"-c", "features.goals=false", "-c", "features.image_generation=false",
		"-c", "features.multi_agent_v2=false", "-c", "features.current_time_reminder=false",
		"-c", "agents.enabled=false", "-c", "orchestrator.skills.enabled=false",
		"-c", "skills.bundled.enabled=false", "-c", "orchestrator.mcp.enabled=false",
		"-c", "tools.update_plan.enabled=false", "-c", "tools.experimental_request_user_input.enabled=false",
	}
	for _, name := range disabledMCPServers {
		args = append(args, "-c", "mcp_servers."+name+".enabled=false")
	}
	return append(args, "app-server", "--listen", "stdio://", "--strict-config")
}
