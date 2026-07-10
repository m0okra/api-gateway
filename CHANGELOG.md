# CHANGELOG

## v1.1 [745ee2f]-[cab926a] — 2026-07-05

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

## v1.2 [cab926a]-[9605d27] — 2026-07-05 ~ 2026-07-07

### Added

- **`pathPrefix` extra 参数**（`9605d27`）：新增 `applyPathPrefix`（`src/transform.go:558`），当目标 path 以 `/v1` 或 `/v1beta` 开头时替换为自定义前缀（如火山引擎的 `/api/v3`）。在 `src/gateway.go:592` 注入点同时覆盖透传路径、格式转换路径与列表转换路径；不依赖 `formatTransform` 开启，README 表格新增该参数说明。

### Changed

#### Cache / Reasoning 重序列化优化（`131aca1`，perf）
为避免不必要的 JSON 重序列化改变字节表示、降低上游缓存命中，引入"未修改即返回原 body"路径：
- `injectCacheControl` 改为返回 `bool`（是否修改）；`injectCacheControlIntoBytes` 在未修改时直接返回原 `body` 字节。
- `countAndUpgradeCacheControl` 改为返回 `(count, upgraded)`；`upgradeCacheControlTTL` 改为返回 `bool`，仅在真正删除/写入 `ttl` 字段时上报已升级。
- `stripEffortIfThinkingDisabledInBytes`（`src/reasoning_vendor.go:146`）同样在 `stripEffortIfThinkingDisabled` 返回 false 时跳过 marshal，保留原始字节。

#### 模型列表 alias 处理重构（`8d3ac55`，refactor）
将列表 alias 流程按 `formatTransform` 与直连两种场景拆分实现：
- 新增就地 JSON 实现 `applyAliasesReverseToListInPlace` + `ApplyAliasesReverseToListInPlaceBytes`（`src/transform.go:368/521`）：直连场景不经中性结构中转，直接改写 `data[]`/`models[]` 数组，**保留上游供应商特有字段不丢失**；Gemini `name` 字段以 `"models/x"` 尾段比较并还原前缀。
- `applyAliasesReverseToList` 改为**两阶段**：阶段1删除"被覆盖"条目（ID == alias key，避免与路由行为不符），阶段2追加 alias 克隆条目；过滤 `k==v` 自指与空串。
- 网关列表分支选择（`src/gateway.go:433`）：`outFormat != ""` 走 `TransformModelsListResponse`（中性结构 + alias）；`outFormat == ""` 但配了 alias 时走就地 JSON 路径（`listInPlace` 标志），无 auth 头重置、错误响应原样透传。
- `TransformModelsListResponse` fast-path 规则更新：有 alias 时即使同格式也走 parse→build；`swapAuthForTarget` 与 `TransformErrorResponse` 在 `listInPlace` 路径下跳过。

### Docs

- 新增 `CHANGELOG.md`（`d40e45b`），记录 v1.1 完整变更。
- README 首段补充"API 格式转换"能力描述（`1a0ff91`），随后精简冗余措辞（`a0da4a6`）。
- `.gitignore` 新增 `tools/` 与 `.trae/`（随 `1a0ff91`）。

## v1.3 [8a812b0]-[bae9e2c] — 2026-07-08 ~ 2026-07-10

### Security

- **/status 端点 token 鉴权**（`e54dea5`）：将原 JSON /status handler（泄露所有 upstream 配置，包括 token、targetBase 等）替换为 HTML 页面 + `POST /status/check` 端点，仅返回该 fakeToken 关联的 upstream 健康状态。移除了 `maskToken`、`statusUpstream`、`statusResponse` 等不再使用的类型。在 `main.go` 注册 `/status/check` 路由。

### Added

- **配置校验体系**（`ee87d78`）：为 `TokenMapConfig`、`UpstreamConfig`、`AvailabilityConfig`、`CacheInjectorConfig` 新增 `Validate()` 方法，在 `loadFromDB`（启动 fail-fast）与 `importFromJSON`（导入前校验，不触 DB）时集中调用，收集全部错误一次性返回。

- **导入自动备份**（`ee87d78`）：`importFromJSON` 在解析 JSON 并校验通过后，若 `gateway.db` 已存在则自动备份为 `gateway.db.bak`（0600），再执行事务写入。

- **Context-aware saveState**（`ee87d78`）：`saveState` 改为接收 `context.Context`，内部 `BeginTx`/`Prepare`/`Exec` 切换为 context-aware 变体；main 通过 `shutdownCtx` 传递给 scheduler，支持 10s 超时兜底取消（`saveStateTimeout` 常量），防止 final save 卡住停机。

- **Count 周期刷新**（`e155c74`）：`checkRecovery` 不再跳过未耗尽 count upstream，cron 匹配时即使不 exhausted 也归零计数（真正按 `RefreshCron` 周期刷新的语义）；`Count==0 && !Exhausted` 时为 no-op 不写 DB。

- **/status 增强**（`e155c74`）：`statusCheckUpstream` 新增 `Limit` 与 `RecoveryCron` 字段；`Count`/`Balance` 解除 `omitempty` 以确保零值序列化；HTML 渲染 "count/limit"（limit 缺省显示 ∞）和 "Refresh: cron"。

### Fixed

- **Nil guard 与数据竞态**（`9849425`）：`checkAvailability` 的 `availCount` 分支增加 `st == nil` 防护，防止 panic；handler 中 `AvailabilityState` 在 `RLock` 下拷贝，避免与写者数据竞争。
- **SQLite 并发限制**（`9849425`）：`openDB` 调用 `SetMaxOpenConns(1)`，防止 WAL 下单写模型下的 `SQLITE_BUSY` 冲突。

### Style

- `state.go` 与 `scheduler.go` 缩进修正（`8a812b0`）；其余文件缩进修正（`e0e101a`）。

### Docs

- README 与当前代码库同步（`bae9e2c`）：重写 `/status` 章节、新增配置校验小节、修正配置导入导出、模型列表 alias 反向展开、流式 error 事件格式、Gemini 工具调用 ID、各文件详解（globals/state/providers/scheduler）等 14 项差异。