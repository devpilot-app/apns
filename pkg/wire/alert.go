package wire

import (
	"fmt"
	"strings"
)

// 审批通知的可见文案生成。
//
// 放 wire 而不是 deliver：通知正文（普通 alert 推送）与 Live Activity 起卡
// 的 alert 是同一句话，两处必须逐字一致——下沉到共享层就不存在「改一处忘
// 另一处」的漂移。

// DisplayAgent 把 wire 的 agent 标识换成锁屏可见的抬头。
func DisplayAgent(a Agent) string {
	switch a {
	case AgentClaude:
		return "Claude"
	case AgentCodex:
		return "Codex"
	case AgentOpenCode:
		return "OpenCode"
	case AgentGemini:
		return "Gemini"
	case AgentCopilot:
		return "Copilot"
	case AgentCursor:
		return "Cursor"
	case AgentGrok:
		return "Grok"
	case AgentQwen:
		return "Qwen"
	default:
		return "Agent"
	}
}

// ApprovalBody 生成审批通知正文，只使用摘要白名单字段。
//
// 摘要可能是 Markdown / JSON 原文（plan、问题选项），锁屏上逐字显示很难读，
// 统一过 PlainPreview 压成单行自然语言；三种工具各给一句人话。
func ApprovalBody(env Envelope) string {
	agent := DisplayAgent(env.Agent)
	summary := PlainPreview(env.Payload.CommandSummary, 200)
	// 新版 hook 已给文件类工具生成带动词的完整短语（"Claude wants to read:
	// foo.swift"，见 hook agent.toolLead）；这里若再包一层 "%s wants to run
	// %s" 会双前缀。识别到 hook 短语就原样透传（只做长度截断）。
	if strings.HasPrefix(summary, agent+" wants to ") {
		return Truncate(summary, 200)
	}
	// 只通知不阻塞的等待卡（TerminalOnly agent 的 PermissionRequest 通知、
	// AskUserQuestion 历史链路）：手机上没有可点的决定，正文说清「在终端
	// 等你」。放在 switch 之前：ExitPlanMode/AskUserQuestion 的特形文案
	// 只属于可作答的链路。
	if env.IsNotifyOnly() && env.Payload.ToolName != "ExitPlanMode" {
		if env.Payload.ToolName == "AskUserQuestion" {
			if summary == "" {
				return agent + " has a question"
			}
			return Truncate(agent+" has a question — "+summary, 200)
		}
		if summary == "" {
			return agent + " is waiting for approval"
		}
		return Truncate(agent+" is waiting for approval: "+summary, 200)
	}
	switch env.Payload.ToolName {
	case "ExitPlanMode":
		if summary == "" {
			return "Plan ready for review"
		}
		return Truncate("Plan ready for review — "+summary, 200)
	}
	tool := env.Payload.ToolName
	if tool == "" {
		tool = "a tool"
	}
	body := fmt.Sprintf("%s wants to run %s", agent, tool)
	if summary != "" {
		body += ": " + summary
	}
	return Truncate(body, 200)
}

// ApprovalTitle 生成审批通知标题（会话标题 > 任务标题 > 兜底）。
func ApprovalTitle(env Envelope) string {
	switch {
	case env.Payload.SessionTitle != "":
		return Truncate(env.Payload.SessionTitle, MaxTitleRunes)
	case env.Payload.Title != "":
		return Truncate(env.Payload.Title, MaxTitleRunes)
	default:
		return "Approval Required"
	}
}
