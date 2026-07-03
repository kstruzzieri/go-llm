// This file registers the opt-in agent_memory_* tools: the #114 agent-memory
// record store exposed to MCP clients (Firn IDE, Flux ML, Quantum Trader).
// Contracts mirror the #257 Golem built-ins: content-light write responses,
// FlattenRecordContent on all displayed content, tool-input scoping enforced
// by the store's derived-visibility clause, and injection-downgrade framing
// in every description.

package mcp

// registerMemoryTools registers the agent_memory_* tools. No-op unless
// WithAgentMemoryPath opened a record runtime (opt-in; absent tools, not
// erroring tools, are the disabled state).
func (s *Server) registerMemoryTools() {
	if s.agentMemory == nil {
		return
	}
	s.registerAgentMemorySearch()
	s.registerAgentMemoryCreate()
	s.registerAgentMemoryPromote()
}

// stub: implemented in a following commit (Task 5).
func (s *Server) registerAgentMemorySearch() {}

// stub: implemented in a following commit (Task 6).
func (s *Server) registerAgentMemoryCreate() {}

// stub: implemented in a following commit (Task 7).
func (s *Server) registerAgentMemoryPromote() {}
