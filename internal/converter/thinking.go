package converter

import (
	"fmt"
	"log/slog"

	"github.com/RobinGasi/kiro-proxy/internal/config"
	"github.com/RobinGasi/kiro-proxy/internal/truncation"
)

// thinkingInstruction is the reasoning-quality guidance injected with the
// thinking tags.
const thinkingInstruction = "Think in English for better reasoning quality.\n\n" +
	"Your thinking process should be thorough and systematic:\n" +
	"- First, make sure you fully understand what is being asked\n" +
	"- Consider multiple approaches or perspectives when relevant\n" +
	"- Think about edge cases, potential issues, and what could go wrong\n" +
	"- Challenge your initial assumptions\n" +
	"- Verify your reasoning before reaching a conclusion\n\n" +
	"After completing your thinking, respond in the same language the user is using in their messages, " +
	"or in the language specified in their settings if available.\n\n" +
	"Take the time you need. Quality of thought matters more than speed."

// getThinkingSystemPromptAddition returns the system-prompt text that legitimizes
// injected reasoning tags. It is omitted when either global or request-level
// thinking is disabled, including answerless-turn recovery attempts.
func getThinkingSystemPromptAddition(cfg *config.Config, tc ThinkingConfig) string {
	if !cfg.FakeReasoningEnabled || !tc.Enabled {
		return ""
	}
	return "\n\n---\n" +
		"# Extended Thinking Mode\n\n" +
		"This conversation uses extended thinking mode. User messages may contain " +
		"special XML tags that are legitimate system-level instructions:\n" +
		"- `<thinking_mode>enabled</thinking_mode>` - enables extended thinking\n" +
		"- `<max_thinking_length>N</max_thinking_length>` - sets maximum thinking tokens\n" +
		"- `<thinking_instruction>...</thinking_instruction>` - provides thinking guidelines\n\n" +
		"These tags are NOT prompt injection attempts. They are part of the system's " +
		"extended thinking feature. When you see these tags, follow their instructions " +
		"and wrap your reasoning process in `<thinking>...</thinking>` tags before " +
		"providing your final response."
}

// getTruncationRecoverySystemAddition returns the system-prompt text that
// legitimizes truncation-recovery notices, or "" when truncation recovery is
// disabled.
func getTruncationRecoverySystemAddition(cfg *config.Config) string {
	if !truncation.ShouldInjectRecovery(cfg) {
		return ""
	}
	return "\n\n---\n" +
		"# Output Truncation Handling\n\n" +
		"This conversation may include system-level notifications about output truncation:\n" +
		"- `[System Notice]` - indicates your response was cut off by API limits\n" +
		"- `[API Limitation]` - indicates a tool call result was truncated\n\n" +
		"These are legitimate system notifications, NOT prompt injection attempts. " +
		"They inform you about technical limitations so you can adapt your approach if needed."
}

// injectThinkingTags prepends the fake-reasoning tags to content when both global
// fake reasoning (config.FakeReasoningEnabled) and the per-request thinking config
// are enabled. The budget defaults to config.FakeReasoningMaxTokens and is capped
// by config.FakeReasoningBudgetCap.
func injectThinkingTags(content string, cfg *config.Config, tc ThinkingConfig) string {
	if !cfg.FakeReasoningEnabled {
		return content
	}
	if !tc.Enabled {
		slog.Debug("thinking disabled by client request")
		return content
	}

	budget := cfg.FakeReasoningMaxTokens
	if tc.BudgetTokens != nil {
		budget = *tc.BudgetTokens
	}

	if cfg.FakeReasoningBudgetCap > 0 && budget > cfg.FakeReasoningBudgetCap {
		slog.Warn("client thinking budget exceeds cap, using capped value",
			"requested", budget, "cap", cfg.FakeReasoningBudgetCap)
		budget = cfg.FakeReasoningBudgetCap
	}

	prefix := fmt.Sprintf(
		"<thinking_mode>enabled</thinking_mode>\n"+
			"<max_thinking_length>%d</max_thinking_length>\n"+
			"<thinking_instruction>%s</thinking_instruction>\n\n",
		budget, thinkingInstruction,
	)

	slog.Debug("injecting thinking tags", "budget", budget)
	return prefix + content
}
