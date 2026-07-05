# CHANGELOG

## v1.1 [cab926a-745ee2f] — 2026-07-05

### ⚠️ Breaking Changes

- **别名机制迁移至 per-upstream DB 配置**：移除了 `-aliases` 启动参数与全局 `aliases.json` 文件机制。
  - 别名现通过 `upstreams` 表的 `aliases` JSON 列按上游独立配置，启动时通过 `ALTER TABLE ADD COLUMN` 自动迁移。
  - 配置方式：通过 `-i` 导入 JSON，或直接用 `sqlite3` CLI 编辑 `gateway.db`。
  - 移除了 `src/aliases.go` 与全局 aliases 变量。

### Added

#### API 格式转换体系（核心新增）
- 新增 4 种格式（openai_chat / openai_responses / anthropic / gemini）两两互转的请求与响应转换器（`transform.go`、`transform_stream.go`）。
- 新增 SSE 流式转换状态机，覆盖 4 格式两两互转的流式场景。
- 新增模型列表 6 向直转（4 种格式，不经 anthropic pivot 中转），并在列表响应中执行反向别名展开。

#### Reasoning / Thinking 兼容
- 新增 `reasoning_vendor.go`：处理 Kimi / DeepSeek 等 vendor 的 thinking 块兼容。
- 在 Anthropic 请求中新增 pre-transform pass，处理 thinking 重写与 effort 剥离。
- reasoning effort 自适应模式与阈值对齐（4096→4000，16384→16000；带 budget 未指定时映射为 `high`）。

#### Codex 后端兼容
- 新增 Codex 后端请求整形逻辑，适配 ChatGPT Codex OAuth 接口要求。
- 适配 `reasoning_summary_text` 事件以匹配 Codex 版本。
- 新增 OpenAI Responses `refusal.delta` / `refusal.done` 与 `function_call_arguments.done` 事件。

#### Cache / Shadow / Rectifier
- 新增 `cache_injector.go`：Anthropic `cache_control` 自动注入（至多 4 个断点），预统计已有断点并升级 TTL。
- 新增 `gemini_shadow.go`：Gemini `thoughtSignature` 影子存储（按 tool_call_id 存完整 parts，多轮回放）；并支持 thinking-only turn 的 thoughtSignature 影子存储。
- 新增 `thinking_rectifier.go`：400 重试时自动剥离 thinking 块以修复 thinking signature。

### Removed

- `src/aliases.go` 与全局 aliases 变量。
- `example.json` 中旧的别名示例条目。

### Docs

- README 全面更新：per-upstream 别名说明、模型列表转换、passthrough 行为澄清、extras 配置范围增强。