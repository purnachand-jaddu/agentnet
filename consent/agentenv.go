package consent

// agentEnvVars are set by coding agent harnesses, never by a person's own shell.
var agentEnvVars = []string{
	"CLAUDECODE",
	"CLAUDE_CODE_SESSION_ID",
	"CODEX_THREAD_ID",
	"CODEX_SANDBOX",
	"CURSOR_AGENT",
	"GEMINI_CLI",
	"OPENCODE",
	"COPILOT_AGENT_SESSION_ID",
}

// AgentEnv reports whether the process runs under a coding agent.
func AgentEnv(getenv func(string) string) bool {
	for _, k := range agentEnvVars {
		if getenv(k) != "" {
			return true
		}
	}
	return false
}
