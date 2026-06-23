package memory

import "diegoc-agent/internal/schema"

// ContextChecker 上下文检查器。检查消息列表是否超过 token 阈值，
// 超了就把消息切成两半：需要压缩的旧消息 + 保留在上下文中的最近消息。
//
// 对应 ReMe 的 ContextChecker（reme/memory/file_based/components/context_checker.py:12）。
type ContextChecker struct {
	handler  *MsgHandler
	threshold int // 触发压缩的 token 阈值
	reserve   int // 保留在上下文中的 token 数
}

// NewContextChecker 创建上下文检查器。
// threshold: 消息总 token 超过这个值就触发切割。
// reserve: 从尾部保留多少 token 的最近消息。
func NewContextChecker(handler *MsgHandler, threshold, reserve int) *ContextChecker {
	return &ContextChecker{
		handler:   handler,
		threshold: threshold,
		reserve:   reserve,
	}
}

// Check 检查上下文，返回切割结果。
// 返回空 toCompact + 全部消息作为 toKeep 表示不需要压缩。
// isValid=false 表示切割后 tool_use/tool_result 配对不完整，不应压缩。
func (c *ContextChecker) Check(messages []schema.Message) (toCompact, toKeep []schema.Message, isValid bool) {
	return c.handler.ContextCheck(messages, c.threshold, c.reserve)
}
