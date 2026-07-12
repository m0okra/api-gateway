# api-gateway

Go + SQLite 轻量级 API 反向代理网关，兼容三大主流AI API（OpenAI/Anthropic/Google Gemini），支持 API 格式转换、多 token 轮转、可用性检查、自动恢复与状态持久化。

## 目录结构

```
src/
├── main.go            入口：flag 解析、加载配置、启动 HTTP 服务器、优雅关闭
├── globals.go         全局变量：配置状态、共享 transport/HTTP 客户端、writeJSON 工具
├── constants.go       常量定义：超时阈值、可用性类型共用名、并发与超时保护常量
├── types.go           数据结构：TokenMapConfig/UpstreamConfig/AvailabilityConfig/...
├── singleflight.go    可用性检查 singleflight（自实现，仅用标准库）
├── cron.go            轻量 Cron 解析调度 + LRU 缓存
├── providers.go       可用性 Provider：DeepSeek 余额、OpenCode-Go 用量等检查实现
├── state.go           状态管理：从 SQLite 加载/保存（锁外 I/O 快照写）、一致性协调
├── scheduler.go       定时调度：exhaust 恢复检查 + 状态持久化触发
├── transform.go       API 格式转换：4 格式 × 请求/响应转换器（openai/anthropic/gemini 互转，anthropic pivot）+ 模型列表 6 向直转
├── transform_stream.go SSE 流式转换状态机（4 格式两两互转，经 anthropic pivot）
├── gemini_shadow.go   Gemini thoughtSignature 影子存储（按 tool_call_id 存完整 parts，多轮回放）
├── reasoning_vendor.go reasoning vendor 兼容（thinking 历史重写、thinking 禁用时剥离 effort）
├── cache_injector.go  Anthropic cache_control 自动注入（最多 4 个断点）
├── thinking_rectifier.go thinking signature 自动修复（400 重试时剥离 thinking 块）
└── gateway.go         核心转发：fakeToken → upstream 队列轮转（原子挑选）、请求注入、格式转换、流式响应
```

## 编译说明

- Go ≥ 1.26
- 依赖 `modernc.org/sqlite`（纯 Go 实现，**无需 CGo**），保持单 exe 静态构建，跨平台编译方便
- 在 `src/` 目录下执行：

```bash
go build -o api-gateway.exe .
```

产物 `api-gateway.exe` 可独立部署，运行时仅需 `gateway.db`（首次启动自动创建空库）。

## 使用说明

### 启动参数

| flag | 说明 |
|---|---|
| `-p` / `-port` | 运行端口，可重复指定以同时监听多个端口，支持纯端口（`9090`）或 `host:port`（`127.0.0.1:9091`、`:9092`），未传默认 `:9090`。所有端口共享同一份配置/DB/状态 |
| `-db` | SQLite 数据库文件路径，默认 `gateway.db` |
| `-e <file>` / `-export <file>` | 将 `-db` 库全量导出为 JSON 文件后退出（不启动服务器） |
| `-i <file>` / `-import <file>` | 将 JSON 文件全量导入 `-db` 库后退出（不启动服务器，全量覆盖） |

`-e` 与 `-i` 互斥。

多端口示例：

```bash
# 同时监听 0.0.0.0:9090、127.0.0.1:9091、:9092，共享同一份配置
api-gateway -p 9090 -p 127.0.0.1:9091 -p :9092
```

### 快速开始

1. 首次运行无 `gateway.db` 时启动会创建空库，日志提示：

```
TokenMap loaded from DB (fakeTokens=0, upstreams=0)
State loaded from DB (0 upstreams)
请使用 -i example.json 导入配置，或直接用 sqlite3 CLI 编辑 gateway.db 后重启。
Gateway running on port 9090
```

2. 参考 `example.json` 编写配置（脱敏示例，含 fakeTokens 队列与 upstreams 配置），导入：

```bash
api-gateway -i my-config.json
```

3. 正常启动：

```bash
api-gateway -p 9090
# 或同时监听多个端口
api-gateway -p 9090 -p 127.0.0.1:9091
```

4. 客户端用 fakeToken 请求，token 注入方式按优先级递减：`Authorization: Bearer xxx` / `X-Goog-Api-Key` / `?key=` / `X-Api-Key`。网关将 fakeToken 替换为 upstream 的 realToken 转发到 targetBase。

### 配置导入导出

- **导出**：`api-gateway -e dump.json` — 备份当前库为可读 JSON
- **导入**：`api-gateway -i dump.json` — 从 JSON 恢复/迁移配置（全量覆盖 DB）
- 也可直接用 `sqlite3` CLI 编辑 `gateway.db` 后重启生效
- JSON 文件含明文 `realToken`，需自行保护文件权限

详见下文「状态持久化」与「配置导入导出」章节。

###  OpenCode Go所需 `Extra`（`cookie` 与 `workspaceId`）获取方式：
1. 登录 https://opencode.ai
2. 打开浏览器开发者工具 → Network
3. 访问 `/workspace/{workspaceId}/usage` 页面
4. 找到 `_server` 请求，从 Request Headers 复制完整的 cookie
5. workspaceId 从 URL 中获取（格式：`wrk_` 开头）

## 核心概念

### fakeToken / upstream

- **fakeToken**: 用户对外持有的假 token，请求中通过 `Authorization: Bearer xxx`、`X-Goog-Api-Key`、`?key=` 或 `X-Api-Key` 传入。
- **upstream**: 一个真实 API Key 的抽象标识，包含 `realToken`（真实密钥）、`targetBase`（上游 base URL）以及可用性配置。
- 每个 fakeToken 对应一个有序的 upstream 队列（`FakeTokens`），请求被转发到队列首部非 exhaust 的 upstream；请求后若触发 exhaust 则该 upstream 轮转到队尾，实现自动容灾。

### 可用性类型

`count` 类型的 `limit` 若为 `0`（或未配置），表示无上限，计数永不触发 exhaust。

未声明 `type` 的 upstream 默认使用 `none` 类型（不统计，永不耗尽）。

| 类型 | 含义 | exhaust 条件 | 恢复依据 |
|---|---|---|---|
| `none` | 不统计（默认） | 永不 exhaust | 无（透传所有错误，不进入恢复调度） |
| `count` | 请求次数限流 | `count >= limit` | `RecoveryCron`（cron 周期匹配 = `RefreshCron`），按表达式定时重置 count |
| `balance` | 余额型 | provider 返回 balance ≤ 0 | `RecoveryAt`（精确时间点），由 provider 按最长的 resetInSec 设定 |
| `usage` | 用量型 | provider 返回任一层级 ≥ 100% | `RecoveryAt`（精确时间点），由 provider 按已耗尽层级最长的 resetInSec 设定 |
| `exhaust` | 触发即耗尽 | 响应可用性错误时立即 exhaust | `RecoveryAt`（精确时间点），默认 30min 后自动恢复 |

### 配置校验

所有配置结构（`TokenMapConfig` / `UpstreamConfig` / `AvailabilityConfig` / `CacheInjectorConfig`）在 `loadFromDB()` 启动加载与 `importFromJSON()` 导入时集中调用 `Validate()`，**校验失败即 fail-fast**：启动加载直接 `log.Fatal` 退出，导入在触碰 DB 前返回错误（DB 保持原状）。校验收集全部错误一次性返回，便于一次定位所有问题。

校验规则：

- **`UpstreamConfig`**：
  - `targetBase` 非空、URL 合法、scheme 为 `http`/`https`、host 非空。
  - `formatTransform` 空串合法（透传）；非空必须命中 `{openai, openai_responses, anthropic, gemini}`。
  - `realToken` 可空（本地 Ollama 等无需鉴权的 upstream 合法）。
- **`AvailabilityConfig`**：
  - `type` 必须命中 `{count, usage, balance, exhaust, none}`。
  - `count` 型要求 `limit > 0`（否则 `Count>=0` 立即耗尽）。
  - `balance` / `usage` 型要求 `provider` 非空（否则静默走 fallback）。
- **`CacheInjectorConfig`**：`Enabled=false` 时跳过 TTL 校验；启用时 `ttl` 仅可为 `"5m"`、`"1h"` 或空串（=默认 `5m`）。
- **空库合法**：`Upstreams` 为空不算错误（首次启动空库是正常场景）。

### 请求流程

```
client → 网关 (带 fakeToken)
          │
          ▼
    ┌─ 全局并发信号量（channel semaphore，容量 256）——超限请求阻塞排队
    ▼
    提取 fakeToken（bearer / goog header / query key / api-key header 优先级递减）
          │
          ▼
    查找 FakeTokens[fakeToken] 队列
          │
          ▼
    pickFirstAvailableUpstream：写锁内原子扫描队列，跳过已 exhausted 的前缀（移到队尾），
    返回首个可用 upstream 及其配置快照（省去三次单锁间的 TOCTOU 竞态）
          │
          ├─ 整队列不可用 → 返回 503
          │
          └─ upstream 可用 → 用 realToken 替换 fakeToken，转发到 targetBase
                            │
                            ├─ count 型 → incrementCount，达 limit 则标记 exhaust
                            │
                            └─ 检查响应状态码
                                 ├─ 401/402/403/429 → singleflight 去重（同 upstream
                                 │   并发仅首次调用 provider），exhaust 则 rotate
                                 ├─ 流式 (SSE) → streamIdleTimeout 空闲超时 + maxStreamLife 硬上限
                                 └─ 其他 → 直接转发响应
```

## 关键实现细节

### 多 token 注入语义

请求中的 token 注入方式取决于原始请求如何传入 fakeToken：

- `X-Goog-Api-Key` header → 替换同 header 为 realToken
- `?key=` query → 替换 query 中 key 值为 realToken
- 两者同时存在 → 两者同时替换
- `X-Api-Key` header → 替换该 header
- `Authorization: Bearer` / 其他 → 替换 Authorization header

### 流式响应保护

流式分支有两个保护层：
- **空闲读超时**（`streamIdleTimeout = 5min`）：启动 goroutine 监控，超时内未读到上游数据则调用 `cancelReq()` 中断上游连接。
- **最大生命周期**（`maxStreamLife = 30min`）：流式上下文叠加 `context.WithTimeout` 硬上限。即使空闲监控 goroutine 在边界情况未触发，到期也会强制取消上游连接，防止流式 goroutine 无限堆积。

### 状态持久化

- 所有配置与运行时状态统一存储在 `gateway.db`（SQLite，WAL 模式，明文不加密，依赖文件权限保护）。
- 表结构：`upstreams`（配置列 + 运行时状态列同行，含 per-upstream `aliases` JSON 列）、`upstream_tiers`（usage 型层级配置+状态）、`upstream_extra`（Extra map）、`fake_tokens`（fakeToken→有序 upstream 队列，priority=队列下标）。
- 恢复调度依据按类型二选一：
  - **count 型**：`RecoveryCron`（cron 表达式，对应配置的 `RefreshCron`）
  - **usage/balance/exhaust 型**：`RecoveryAt`（精确时间点 `time.Time`，由 provider 在 exhaust 时设定）
- 内存中维护 `stateDirty` 标志与 `stateGen` 代际计数器（`atomic.Uint64`），每 5min 检查并写入。
- `saveState()` 优化为锁内快照 → 锁外 I/O：在写锁内深拷贝 state 快照并记录代际后立即释放锁，SQLite 事务在锁外执行；提交后再次取锁，仅当代际未变（无新写入）才清 dirty，避免写库期间（可能数百 ms）阻塞所有请求。
- 调度器退出前 final save 确保持久化一致性；main 通过 `schedDone` channel 等待 final save 完成后再 `db.Close()`，避免事务被截断。
- 启动时执行 `cleanFakeTokenQueues`（清理 FakeTokens 队列中引用不存在的 upstream）与对称的 `reconcileStateWithConfig`（为 tokenMap 中存在的 upstream 补齐初始 AvailabilityState、删除孤立 state 条目），并重置 `stateDirty=false`。
- 运行时 upstream 队列轮转顺序不持久化（重启恢复 `fake_tokens.priority` 定义的配置顺序）。
- 旧文件迁移：usage/balance/exhaust 型旧 `RecoveryCron` 字段在重启后被忽略，`RecoveryAt` 为零值触发立即复查，首次写入后清空旧字段。

### 配置导入导出

两个一次性管理 flag，执行后立即退出，不启动 HTTP 服务器：

- `-e <file>` / `--export <file>`：将 `-db` 库全量导出为单个 JSON 文件
- `-i <file>` / `--import <file>`：将 JSON 文件全量导入 `-db` 库（**全量覆盖**，DB 中原有数据被清空替换）

两者互斥。JSON 文件格式为 `DBDump`：`{ "tokenMap": {fakeTokens, upstreams}, "state": {upstream: AvailabilityState} }`，结构与运行时内存模型一致，`time.Time` 走 RFC3339Nano 字符串，人类可读可编辑。

导入流程（按顺序）：
1. **预校验**：解析 JSON 后立即对其调用 `Validate()`（见上文「配置校验」），失败则拒绝，**不触碰 DB**，打印全部错误。
2. **自动备份**：若 `gateway.db` 已存在，先整体读出并写入同目录 `gateway.db.bak`（0600 权限），再开始写入——失败回滚也仍有原库备份可恢复。
3. **全量覆盖**：单事务内 `DELETE` 4 张表（子表 `upstream_tiers`/`upstream_extra`/`fake_tokens` 先于父 `upstreams`）后依次 `INSERT`。
4. **导入防御**：fakeToken 队列中重复 upstream 在 Go 层用 `seen` map 跳过保留首次（`INSERT OR IGNORE` 仅作数据库层兜底）；队列引用不存在的 upstream 跳过避免 FK 违约；state 中有但 upstreams 中无的孤儿条目警告忽略。
5. 任一步失败整体 `Rollback`，DB 保持原状（前面已备份的 `.bak` 不受影响）。

典型用途：备份/恢复 `gateway.db`、手工编辑配置后导入、跨环境迁移。导出文件含明文 `realToken`，需自行保护文件权限。

### 模型别名（per-upstream）

每个 upstream 可选配置 `aliases` 字段，提供客户端请求模型名 → 上游真实模型名的映射，用于在转发前重写请求中的模型名。**该字段缺省时该 upstream 不启用别名**（请求原样转发）；不同 upstream 可独立配置不同 alias 映射，互不干扰。**别名替换不依赖 `formatTransform`**——未配置格式转换时同样生效（body 的 `model` 字段或 Gemini path 会被改写，非字节级零改动）。

`aliases` 结构为 `map[string]string`，key 与 value 均为字符串：

```json
"deepseek": {
  "realToken": "***",
  "targetBase": "https://api.deepseek.com",
  "aliases": {
    "gpt-4-turbo": "deepseek-chat",
    "claude-3-opus": "deepseek-chat"
  }
}
```

替换规则：

- 仅当请求中提取到模型名（body 的 `model` 字段，或 Gemini 风格 URL path `/models/{name}`）且该模型名命中当前 upstream 的 `aliases` 的 key 时，替换为对应 value。
- 同时覆盖三种 API 风格：
  - OpenAI/Anthropic 风格：重写请求体 JSON 的 `model` 字段（重新序列化 body，`Content-Length` 由 transport 自动重算）。
  - Gemini 风格：重写 URL path 中的模型名段（如 `/v1beta/models/gemini-pro:generateContent` → `/v1beta/models/gemini-1.5-pro:generateContent`）。
- body 非 JSON 或 `model` 字段非字符串时静默跳过 body 重写（不影响 URL path 重写）。
- **作用于 attempt 循环内**：alias 重写仅作用于本次 attempt 的局部 `sendBody`/`sendModel`/`basePath`，不污染跨 attempt 复用的原始 `bodyBytes`/`r.URL.Path`/`modelStr`。重试到另一个 different-format upstream 时按其各自的 `aliases` 重新计算，避免跨 upstream 串污染。
- **DB 持久化**：`upstreams` 表 `aliases` 列存 JSON 编码的 map 字符串；导入导出（`-e`/`-i`）跟着 upstream 配置一起序列化。旧库无此列时启动自动 `ALTER TABLE ADD COLUMN` 兼容。
- **模型列表响应反向展开**：上游返回模型列表时，按 value→key 反向展开——见下文「模型列表请求转换」。

### API 格式转换（formatTransform）

每个 upstream 可选配置 `formatTransform` 字段，使网关在转发前将客户端请求体转换为目标上游的 API 格式，并在响应返回时反向转换回客户端格式。**不配置或留空时，formatTransform 相关逻辑完全跳过**，请求体/path 多数情况下字节级透传（视 `aliases` / `extra` / `cacheInjection` 等独立配置而定，见下文「高级转换特性」）。

#### 可选值

| 值 | 目标格式 | 说明 |
|---|---|---|
| `openai` | OpenAI Chat Completions | 上游走 `/v1/chat/completions` |
| `openai_responses` | OpenAI Responses API | 上游走 `/v1/responses` |
| `anthropic` | Anthropic Messages | 上游走 `/v1/messages` |
| `gemini` | Google Gemini | 上游走 `/v1beta/models/{model}:generateContent`（流式 `:streamGenerateContent?alt=sse`） |
| 不指定 | — | formatTransform 透传（body/path 不变）；但 `aliases`/`extra` 增强项/cacheInjection 仍独立生效 |

#### 转换规则

- **目标格式** = upstream 配置的 `formatTransform`。
- **客户端格式** 按请求 URL path 自动检测：`/v1/chat/completions`→openai、`/v1/responses`→openai_responses、`/v1/messages`→anthropic、`/v1beta/models/{model}:...`→gemini，其他→unknown（透传）。
- **链式转换**：任何两种不同格式间都经 anthropic 作为 pivot 中间格式两步完成（含 `openai_chat` ↔ `openai_responses`），对调用方透明。仅当客户端格式 == 目标格式时才透传。
- **目标 path 重写**：转换路径下，目标 URL path 按目标格式规范端点替换；透传路径保留原始 `r.URL.Path`。
- **认证头重写**：转换路径下，按目标格式注入认证（gemini→`X-Goog-Api-Key`/`?key=`；anthropic→`x-api-key`+`anthropic-version`；openai→`Authorization: Bearer`）；透传路径沿用原 token 注入优先级（`X-Goog-Api-Key`/`?key=`/`X-Api-Key`/`Authorization`）。

#### 模型列表请求转换

模型列表端点（`GET /v1/models` / `GET /v1beta/models`）也走格式转换路径，但与业务端点（chat/messages/responses/gemini generate）不同——不经 anthropic pivot，4 种格式 6 个方向**直接两两转换**，保留 `inputTokenLimit`/`outputTokenLimit` 等元数据，避免经 anthropic 中转丢失。

- **客户端格式判定**（按 path + auth header）：
  - `/v1beta/models` → gemini（path 自带上游风格信号）。
  - `/v1/models` + `X-Api-Key` 或 `anthropic-version` 头任一 → anthropic。
  - `/v1/models` + 其余（Bearer / 兜底） → openai_chat（openai_responses 复用同一列表端点）。
- **目标 path 重写**：gemini 上游走 `/v1beta/models`，其余走 `/v1/models`。
- **认证头**：与业务端点共用 `swapAuthForTarget` 按目标格式注入。
- **6 向直转**：openai↔anthropic、openai↔gemini、anthropic↔gemini（每对两向）共 6 个方向各自实现经中性 `modelsList` 结构；openai_chat ↔ openai_responses 列表结构完全相同走 fast path 透传。
- **字段映射约定**：
  - OpenAI → Anthropic：`id`→`id`，`display_name` 用 `id` 兜底，`owned_by` **丢弃**。
  - Anthropic → OpenAI：`id`→`id`，`owned_by` 留空。
  - Gemini → OpenAI/Anthropic：从 `name="models/x"` 提取尾段作为 `id`，`displayName` 直接保留为 `display_name`/`displayName`。
  - 反向 OpenAI/Anthropic → Gemini：`id` 拼回 `name="models/"+id`，`displayName` 兜底用 `id`，固定填 `supportedGenerationMethods: ["generateContent","streamGenerateContent"]`，`inputTokenLimit`/`outputTokenLimit` 在 openai/anthropic 源无对应字段时填 0。
- **别名反向展开（两阶段）**：upstream 配置 `aliases` 时，模型列表响应按下列两阶段处理，使列表显示与请求路由的 alias 行为完全一致：
  1. **删除被覆盖条目**：若某 entry 的 ID 等于某个 alias key（且该 key 的 `value` 不同——即该客户端名已被重定向到另一真实上游模型），则删除其原条目，避免显示与路由行为不符的字段。
  2. **追加 alias 克隆**：为每个指向真实名的 alias key 追加一条克隆 entry，与原真实名条目字段**完全相同**——**仅 `id`/`name`（ID 字段）改为 alias key**，`display_name`/`displayName` 等其余字段保留原真实名条目的值，不随 alias key 变。
  一对多（多个 alias key → 同一真实名）会展开为多条 alias entry；`k==v` 自指跳过，空串无效果。
- **错误响应**：列表请求的 4xx/5xx 错误响应同样走 `TransformErrorResponse` 转为客户端列表格式后返回（inFormat 此时被覆写为客户端列表格式）。
- **未配置 `formatTransform` 时**完全透传（`outFormat==""` → `doListTransform=false`），行为同现状。上游格式 == 客户端列表判定格式时也透传。

#### 流式支持

所有 4 种格式两两之间的 SSE 流式转换均支持（openai_chat↔anthropic、openai_responses↔anthropic、gemini↔anthropic 三个直接方向 + 经 anthropic pivot 的链式方向如 openai_chat↔openai_responses、openai↔gemini）。流式转换器实现为状态机，按 SSE 事件块增量转换，保持上游→客户端的低延迟。Gemini 流式输出采用累积快照 diff 语义（每个 chunk 携带截至当前的完整内容）。

流式转换要点：
- **reasoning 透传**：OpenAI Chat 的 `delta.reasoning`/`reasoning_content`、Responses 的 `response.reasoning.delta`/`.done` 与 `response.reasoning_summary_text.*` 事件均转成 Anthropic `thinking` block（`content_block_start` + `thinking_delta` + `content_block_stop`）。
- **懒发 message_start**：`message_start` 推迟到首个实际内容/usage 事件时才发送，避免空响应留下"悬挂消息"。
- **UTF-8 安全累积**：跨 TCP chunk 边界拆分的多字节 UTF-8 字符会正确累积，不损坏工具调用 JSON 参数。
- **流式 error 事件**：上游流式传输中途出错时按**客户端格式**分流渲染：Anthropic 客户端发 `event: error` + `data: {...}`；OpenAI Chat/Responses 客户端发 `data: {"error":{...}}` + 终止 `data: [DONE]`；Gemini 客户端发 `data: {"error":{"code":500,"status":"INTERNAL",...}}`。并抑制合成的 `message_stop`，避免把失败伪装成正常完成。
- **无限空白中止**：工具调用 `arguments` 中超过 500 连续空白字符视为上游异常，中止该 tool block 以防客户端挂起。
- **重复 finish_reason 去重**：异常上游多次发送终止事件时，`stop_reason` 仅设置一次。

#### 配置示例

`example.json` 片段（让 OpenAI/Anthropic 客户端复用 Gemini 上游）：

```json
"gemini": {
  "realToken": "***",
  "targetBase": "https://generativelanguage.googleapis.com",
  "formatTransform": "gemini",
  "availability": { "type": "count", "limit": 250, "refreshCron": "0 0 16 * * *" }
}
```

DB 直接编辑：`upstreams` 表 `format_transform` 列存配置值字符串（`openai`/`openai_responses`/`anthropic`/`gemini`/空）。注意新版 `/status/check` 响应不回显 `formatTransform`（仅返回健康/计数/恢复信息），需查看完整配置请直接读 DB 或用 `-e` 导出。

#### 限制说明

- **错误响应转换**：4xx/5xx 错误响应体在转换路径下会经 `TransformErrorResponse` 转为客户端格式后返回（包括可用性错误 401/402/403/429）。各厂商错误 JSON 结构差异较大，转换尽量保留 `error.message`/`type` 等通用字段，无法映射的字段按目标格式兜底。仅对 `resp.StatusCode < 300` 的成功响应走 `TransformResponse`。
- **请求转换失败 → 继续尝试下一个 upstream**：客户端请求体无法解析为目标格式时，不直接 400 中断，而是记录 `[TRANSFORM] request convert failed (will try next upstream)` 日志后继续尝试队列中的下一个 upstream（透传 upstream 不进入转换路径，可正常处理）。全部 upstream 均失败后由循环外兜底返回 `503`。
- **响应转换失败 → 回退原 body**：响应转换出错时记日志并原样返回上游响应（非流式）；流式转换 Feed 出错则中断流并记日志。
- **Gemini 工具调用 ID**：Gemini `functionCall` 无独立 ID 字段，转换到 anthropic/openai 时用 `crypto/rand` 合成随机十六进制 ID（前缀 `gemini_synth_`，无状态），可能与上游真实 ID 不一致；反向（anthropic/openai→gemini）时**真实 ID 保留**写入 `functionCall.id`，仅丢弃 `gemini_synth_` 合成前缀的 ID。
- **链式转换语义损耗**：`openai_chat` ↔ `openai_responses` 等跨子格式转换经 anthropic pivot 两步完成，工具调用、reasoning 等字段经历两次映射，可能出现语义损耗（如 Responses 的 namespace tool 经 anthropic 中转后降级为普通 function tool）。
- **非法值兜底**：`formatTransform` 配置为非上述 4 个合法值时，记 `[TRANSFORM] invalid formatTransform ... -> passthrough` 警告日志后按透传处理，不报错。

#### 高级增强特性

除 `formatTransform` 外，网关还内置以下增强能力（多数通过 `upstreams[].extra` map 的字符串参数按 upstream 单独启用）。`extra` map 同时承载可用性检查参数（如 OpenCode-Go 的 `cookie`/`workspaceId`，见上文）。以下 `extra` 参数**不依赖 `formatTransform` 开启**——即使透传（`formatTransform` 为空），只要满足各自的生效条件，仍会生效：

| key | 取值 | 作用 | 生效条件 |
|---|---|---|---|
| `pathPrefix` | 自定义 API 版本前缀，如 `"/api/v3"` | 替换目标 URL path 开头的 `/v1` 或 `/v1beta` 为自定义前缀。用于上游 base URL 使用非标准 API 版本前缀的场景，如火山引擎的 `/api/v3/chat/completions` 而非 `/v1/chat/completions`。同时覆盖透传路径、格式转换路径和列表转换路径。 | 始终生效（无论是否开启 formatTransform，只要构造出的 targetPath 以 `/v1` 或 `/v1beta` 开头即触发替换） |
| `codexBackend` | `"true"` / `"fast"` | 对发往 ChatGPT Codex 后端的 Responses 请求做字段整形（注入 `store:false`/`include`/`stream:true`/兜底字段，剥离 `max_output_tokens`/`temperature`/`top_p`；`fast` 额外注入 `service_tier:"priority"`） | 上游实际发送格式为 `openai_responses`（转换后或 Responses→Responses 透传） |
| `preserveReasoningContent` | `"true"` | Anthropic→OpenAI Chat 转换时把 `thinking` 块提取为 `reasoning_content` 字段（Kimi/DeepSeek/MiMo 等 reasoning vendor 兼容）。**仅当该 assistant 消息含 `tool_use` 块时才写入**；纯 reasoning + text 的消息 thinking 会被静默丢弃 | 仅 formatTransform 开启时生效（需跨格式转换） |
| `reasoningVendor` | `"auto"` 或 `"kimi"`/`"deepseek"`/`"mimo"` 等非空值 | 重写 thinking 历史为占位符，兼容拒绝原始 thinking 块的供应商（`auto` 按 upstream 名/`targetBase` 自动检测） | 客户端格式为 `anthropic`（透传或转换前均可） |
| `stripEffortWhenThinkingDisabled` | `"true"` | `thinking.type != enabled` 时剥离 `reasoning_effort`/`output_config.effort` 参数（DeepSeek Anthropic 兼容端点要求） | 客户端格式为 `anthropic`（透传或转换前均可） |

4 个参数在 attempt 内的作用时点分两类：`reasoningVendor` 与 `stripEffortWhenThinkingDisabled` 属于 **pre-transform pass**（作用于转换前的客户端请求体副本，再将结果写回 `sendBody`；`!doTransform` 时同样生效）；`codexBackend` 属于 **post-transform pass**（作用于转换后的 `sendBody` 或透传路径下直接改写）；`preserveReasoningContent` 仅通过 `TransformOptions` 透传到跨格式转换函数内部，透传路径无效果。

**其他内置增强**（无需配置，默认启用）：

- **cache_control 自动注入**：upstream 配置 `cacheInjection: { enabled: true, ttl: "5m" }` 时，自动在 tools/system/最后 assistant/最后 user 消息末尾注入最多 4 个 `cache_control: {"type":"ephemeral"}` 断点，享受 Anthropic Prompt Caching 折扣。**不依赖 `formatTransform`**，透传 Anthropic→Anthropic 时同样生效。
- **Gemini thoughtSignature 影子存储**：按 `tool_call_id` 维度存储 Gemini 的 `thoughtSignature` 与完整 assistant turn `parts` 数组（含 `thought:true` 块），多轮工具调用时原样回放，避免 Gemini 签名校验失败 400。
- **thinking signature 自动修复**：上游返回 thinking signature 相关 400 错误时，自动剥离 thinking/redacted_thinking 块与残留 `signature` 字段后重试同一 upstream（最多 1 次）。
- **reasoning effort 4 档映射**：`thinking.budget_tokens` 与 `output_config.effort` 映射到 `low`/`medium`/`high`/`xhigh` 四档（`output_config.effort == "max"` 或 `thinking` `adaptive` → `xhigh`；`thinking.type == "enabled"` 但无 `budget_tokens` 时默认 `high`），`xhigh` 在转 OpenAI 时降级为 `high`。
- **redacted_thinking 占位符**：Anthropic 的 `redacted_thinking` 块转 OpenAI/Gemini 时替换为 `[redacted thinking]` 占位文本，保留语义。
- **标准参数透传**：Anthropic↔OpenAI 转换时透传 `frequency_penalty`/`logit_bias`/`logprobs`/`metadata`/`n`/`parallel_tool_calls`/`presence_penalty`/`response_format`/`seed`/`service_tier`/`top_logprobs`/`user` 等 12 个标准参数，不再静默丢弃。
- **document/input_file 支持**：Anthropic `document` 块（PDF）转 Gemini 时变 `inlineData`，转 Responses 时变 `input_file`+`file_data`；**转 OpenAI Chat 时被静默丢弃**（OpenAI Chat 无文档能力）。
- **incomplete_reason 细分**：Responses `incomplete` 状态按 `incomplete_reason` 细分，仅 `max_output_tokens`/`max_tokens` 映射为 `max_tokens`，其他映射为 `end_turn`。

### 状态查询

为避免泄露所有 upstream 配置与真实 token，状态查询拆为两个端点，且均**返回 fakeToken 关联的 upstream**：

- **`GET /status`**：返回一个极简 HTML 查询页（含 token 输入框），前端固定 `POST` 到 `/status/check` 渲染结果。无需鉴权即可访问页面本身，但只有提交有效 token 才能看到对应队列状态。
- **`POST /status/check`**：接收 `{"token":"<fakeToken>"}`，校验该 fakeToken 存在且队列非空，仅返回队列内 upstream 的运行时状态（**不含 `realToken`、不含其他 fakeToken 的队列**），token 无效返回 401。

```bash
curl -X POST http://localhost:9090/status/check \
  -H 'Content-Type: application/json' \
  -d '{"token":"sk-xxxxxx"}'
```

响应体（`upstreams` 数组，仅包含该 fakeToken 队列中的 upstream）：

```json
{
  "upstreams": [
    {
      "name": "gemini",
      "targetBase": "https://generativelanguage.googleapis.com",
      "exhausted": false,
      "availType": "count",
      "count": 10,
      "limit": 250,
      "recoveryCron": "0 0 16 * * *"
    },
    {
      "name": "opencode-go",
      "targetBase": "https://opencode.ai/zen/go",
      "exhausted": true,
      "availType": "usage",
      "count": 0,
      "balance": 0,
      "tiers": [
        {"name": "rolling", "usedPct": 100, "resetInSec": 2592000}
      ],
      "recoveryAt": "2026-07-11T09:09:29Z"
    }
  ]
}
```

| 字段 | 说明 |
|---|---|
| `upstreams` | 该 fakeToken 队列内 upstream 的运行时快照（按队列顺序） |
| `upstreams[].availType` | 可用性类型（`count`/`usage`/`balance`/`exhaust`/`none`，缺省为 `none`） |
| `upstreams[].limit` | count 型的 `limit`（其余类型缺省） |
| `upstreams[].count` | count 型的当前计数（其余类型始终为 0） |
| `upstreams[].balance` | balance 型的余额（其余类型始终为 0） |
| `upstreams[].tiers` | usage 型的各配额层级用量（count/balance 型无此字段） |
| `upstreams[].recoveryCron` | count 型的恢复 cron 表达式（其余类型缺省） |
| `upstreams[].recoveryAt` | usage/balance/exhaust 型的精确恢复时间点（其余类型缺省） |

响应**不包含** `realToken`、`queueFor`、`lastChecked`、顶层 `fakeTokens` 映射等敏感或全局字段——只对持 token 的调用方暴露其自身队列。HTML 页面在浏览器本地用 `fetch` 调用 `/status/check` 完成查询，无外部 JS 依赖。

### 恢复调度机制

> **时区说明**：两种恢复机制均按**服务器本地时区**处理。cron 表达式按本地时区匹配，`RecoveryAt` 以带本地 offset 的 RFC3339 格式存储。可通过设置进程的 `TZ` 环境变量调整时区（如 `TZ=Asia/Shanghai`）。

#### count 型 — Cron 周期匹配

使用 6 字段 cron 表达式（秒 分 时 日 月 周），支持 `*` / `*/N` / `N` / `a-b` / `a,b,c` / `a-b/S`。`RecoveryCron` 直接取配置的 `RefreshCron`。调度器每 1s 匹配当前时间，命中且距上次恢复 ≥ `recoveryMinGap`（60s）时触发重置计数。

解析结果缓存在容量 256 的 LRU 中，避免重启后反复解析相同表达式。

#### usage/balance/exhaust 型 — 精确时间点 RecoveryAt

由 provider 在 exhaust 检查时计算下一次复查时间点：

- **OpenCode-Go 用量型**：在已耗尽（usagePercent ≥ 100）的 rolling/weekly/monthly 层级中，选取最长的 `resetInSec` 作为间隔，设为 `RecoveryAt = now + maxReset`。因为只要任一层级仍耗尽，整体就不可用，必须等最慢的那个层级恢复。
- **DeepSeek 余额型**：余额 ≤ 0 时设为 `now + 30min`。
- **兜底 exhaust**：设为 `now + 30min`。
- 当 provider 返回的 `resetInSec` 异常（≤ 0）时，使用 `minRecoverGap = 60s` 地板保护，防止死循环。

调度器每 1s 遍历所有 exhausted upstream，检查 `now >= RecoveryAt`（零值视为旧文件迁移，立即触发）。触发后按类型分流恢复：
- **`exhaust` 型 / 无 availability 配置**：到达 `RecoveryAt` **直接自动恢复**（清零 `Exhausted` 与 `RecoveryAt`），**不调用 provider**——因为 `fallbackResult` 恒返回 `Exhausted=true`，若复查会形成 60s 死循环。
- **`usage` / `balance` 型**：通过 `availSF.Do()` singleflight 调用 provider 复查，返回新的 exhausted 状态与 `RecoveryAt`；若已恢复则清除 exhausted，upstream 重新参与请求轮转。

### 请求体大小限制

单请求体上限 32MB。使用 `http.MaxBytesReader` 包裹请求体，超限时内部自动写入 413 状态码并返回 `*http.MaxBytesError`。代码通过 `errors.As(rerr, &maxBytesErr)` 判断超限后直接 return，避免二次 WriteHeader 触发 "superfluous" 警告。

## 各文件逻辑详解

### main.go

1. 解析 `-p` / `-port` / `-db` / `-e`(`-export`) / `-i`(`-import`) flag
2. 若指定 `-e` 或 `-i`：执行导出/导入后 `os.Exit(0)`，不启动服务器（管理操作，互斥）
3. 调用 `loadFromDB()` 从 SQLite 加载配置与状态（统一数据源），并运行 `Validate()` 校验——失败即 `log.Fatal` 退出
4. 启动 `runScheduler` goroutine
5. 初始化 `reqSem` 并发信号量（channel semaphore，容量 256），handler 入口 acquire、defer release
6. 启动 HTTP server，监听 `:port`，注册 `GET /status`（HTML 查询页）、`POST /status/check`（按 fakeToken 查询关联 upstream 状态）、`/`（核心代理 handler）；配置 `ReadTimeout=10s` / `IdleTimeout=120s` / `MaxHeaderBytes=1MB`（防御慢速连接攻击；`WriteTimeout=0` 保护流式 SSE）
7. 等待 SIGINT/SIGTERM，触发优雅关闭（等待 scheduler final save 完成后关闭 DB）

### globals.go

- 包级全局变量（`tokenMap`、`stateMap`、`mu sync.RWMutex`、`stateGen atomic.Uint64`、`db`、`dbPath` 等）
- 共享 HTTP transport/客户端：
  - `sharedTransport`（`MaxIdleConns=100`、`MaxIdleConnsPerHost=20`、`IdleConnTimeout=90s`）— 所有代理/provided client 共享连接池，避免每请求新建 Transport
  - `proxyClient`（120s 超时，复用 sharedTransport）— 普通代理请求
  - `streamClient`（无整体超时，复用 sharedTransport）— 流式代理请求
  - provider 可用性检查**不**使用上述任一 client，而在 `providers.go` 内走 `httpGetRaw` 的三阶段重试 client（2s+4s+8s 独立超时，每阶段新建 client 但共用 sharedTransport）
- `reqSem`（channel semaphore，容量 256）：全局并发上限，handler 入口 acquire，超限请求阻塞排队
- `availSF`（`availSingleFlight`）：可用性检查 singleflight，同 upstream 并发触发仅首次执行 provider 调用
- `writeJSON` 统一 JSON 响应写入，捕获并记录编码错误

### state.go

- `openDB`：打开 SQLite `db` 并设 `PRAGMA foreign_keys=ON` + `WAL` + `busy_timeout`、`conn.SetMaxOpenConns(1)`（避免 WAL 下并发写竞争），`CREATE TABLE IF NOT EXISTS` 4 张表 + `idx_fake_tokens_order` 索引；随后用 `PRAGMA table_info` 检查 `upstreams` 列，对旧库执行 `ALTER TABLE ADD COLUMN (format_transform / aliases)` **运行时迁移**。供 `loadFromDB` 与 `importFromJSON` 复用。
- `loadFromDB`：查 4 张表填充 `tokenMap` + `stateMap`；`cleanFakeTokenQueues`（清队列中引用不存在的 upstream）+ `reconcileStateWithConfig`（为 tokenMap 中存在的 upstream 补齐默认 `AvailabilityState`、删除孤立 state 条目）；末尾重置 `stateDirty=false`；调用 `tokenMap.Validate()`，失败 `log.Fatal`。
- `saveState`：单事务写回——先持写锁深拷贝 state 快照（含 Tiers 副本）并记录 `stateGen`，释放锁后遍历快照执行 SQLite I/O（`UPDATE upstreams` 状态列 + 每个 upstream 的 `upstream_tiers` DELETE/INSERT）；提交后再取锁，仅当代际未变才清 dirty。锁外 I/O 避免写库阻塞所有请求。
- `exportToJSON`：复用 `loadFromDB` 载入内存 → marshal `DBDump{tokenMap, stateMap}` → 原子写（tmp+rename，0600）；导出 reconcile 后的规范视图
- `importFromJSON`：① 解析 JSON 后立即 `Validate()`（不触碰 DB）→ ② 若 `gateway.db` 存在则先备份为 `gateway.db.bak`（0600）→ ③ 单事务 DELETE 4 表（先子后父）+ INSERT 全量覆盖；④ 防御：队列重复 upstream 用 Go `seen` map 跳过（`INSERT OR IGNORE` 仅作 DB 兜底从不触发）、引用不存在 upstream 跳过避免 FK 违约、orphan state 警告忽略；任一步失败 `Rollback`（前面已有 `.bak` 备份）。

### cron.go

- `parseCron` / `parseCronField`：6 字段 cron 解析，生成 `CronSchedule`（`[6]map[int]bool`）
- `parseCronCached`：LRU 缓存封装
- `cronLRU`：双向链表 + map，容量 256，最近访问移头部，超容淘汰尾部

### providers.go

- `checkAvailability`：入口分发，按 `availCount` / `availBalance` / `availUsage` / `availExhaust` / `availPassthrough` 调用对应逻辑（无配置或未知类型默认走 none 型）
- `checkDeepSeekBalance`：GET `/user/balance`，解析 `total_balance`；余额耗尽时返回 `RecoveryAt = now + 30min`
- `checkOpenCodeGoUsage`：GET `/_server`，解析 rolling/weekly/monthly 三级用量；在所有已耗尽层级（usagePercent ≥ 100）中取最长 `resetInSec`，返回 `RecoveryAt = now + maxReset`
  - rolling 若不匹配直接 fallback（opencode API 必返回 rolling）
  - weekly/monthly 可能缺失，不影响逻辑
- 其他 provider 仅框架，按类型返回兜底：
  - **balance 型**（无具体实现）：`Kimi`、`OpenRouter`
  - **usage 型**（无具体实现）：`Claude`、`Codex`、`Gemini`、`ZAI`、`MiniMax`
  - 均返回 `fallbackResult`（`Exhausted=true`，`RecoveryAt = now + 30min`）
- `httpGetJSON` / `httpGetText` 不用共享 `defaultClient`，经 `httpGetRaw` 的**三阶段重试**：每阶段独立超时 client（2s + 4s + 8s），仅对网络错误/超时、5xx、429 触发重试；2xx 立即返回，其他 4xx（如 401/403）视为确定失败不重试。每阶段 client 复用 `sharedTransport` 保持连接池

### scheduler.go

- `runScheduler`：主循环 `select` 三个 ticker：`ticker`（1s 恢复检查）、`saveTicker`（5min 状态保存）、`shadowCleanupTicker`（10min Gemini shadow 过期条目惰性清理，见 `gemini_shadow.go`）；通过 `done` channel 通知 main 已完成 final save
- `checkRecovery`：遍历 all upstream，按类型分流：
  - **count 型**：`RecoveryCron` cron 周期匹配
  - **usage/balance/exhaust 型**：`now >= RecoveryAt` 时间点触发（零值为旧文件迁移，立即触发）
  - 统一受 `recoveryMinGap`（60s）约束，避免短时间重复触发
- `recoverUpstream`：按类型分流恢复——
  - **count 型**：直接重置 `Count=0`、清除 `exhausted`，**不调用 provider**（count 由网关自行计数 + cron 周期刷新驱动；`Count==0 && !Exhausted` 时为 no-op，不触发 DB 写）。
  - **`exhaust` 型 / 无 `availability` 配置**：到达 `RecoveryAt` **直接自动恢复**（清零 `Exhausted` 与 `RecoveryAt`），**不调用 provider**——`fallbackResult` 恒返回 `Exhausted=true`，复查会形成死循环。
  - **`usage` / `balance` 型**：通过 `availSF.Do()` singleflight 去重调用 provider 复查（避免与 handler 路径并发重复检查），`applyAvailabilityResult` 写入新 `Exhausted` 与 `RecoveryAt`；若已恢复则清除 `exhausted`，upstream 重新参与轮转。
  - 统一在恢复后更新 `LastRecovery` 并 `markDirty`（count 型 `LastRecovery` 仍更新以维持防抖但不持久化）。

### gateway.go

- handler：核心请求处理函数，以 `reqSem <- struct{}{}` 获取并发令牌（defer 释放），完成后返回
- 队列操作：`pickFirstAvailableUpstream`（一次写锁内原子扫描队列，跳过所有 exhausted 前置 upstream 至队尾，返回首个可用 upstream 与配置快照）/ `rotateUpstreamToEnd` / `getUpstreamQueueLen` / `hasUpstreamQueue`
- 状态查询：`incrementCount`（count 型自增）、`applyAvailabilityResult`（写入检查结果；按类型分流：count 写 `RecoveryCron`、其余写 `RecoveryAt`，并清理另一调度依据的残留）
- 可用性错误（401/402/403/429）路径通过 `availSF.Do()` singleflight 去重 checkAvailability，避免并发请求重复调用 provider
- 流式响应：idle 监控 goroutine + `maxStreamLife` context 超时硬上限，双保险
- 格式转换集成：在 attempt 循环内按 `inFormat`/`outFormat` 决定是否转换（`needsTransform`）。每次 attempt 先按选中 upstream 的 `aliases`（per-upstream）重写请求模型名，作用于本次 attempt 的局部 `sendBody`/`sendModel`/`basePath`（不污染跨 attempt 复用的 `bodyBytes`/`r.URL.Path`/`modelStr`，避免重试到不同名称上游时串污染）。pre-transform pass 对客户端请求体副本应用 `reasoningVendor` 重写与 `stripEffortWhenThinkingDisabled` 剥离（透传路径下同样执行）；post-transform pass 对转换后（或透传）的 `sendBody` 应用 `codexBackend` 整形。`preserveReasoningContent` 通过 `TransformOptions` 透传到跨格式转换函数。
- 模型列表旁路：`detectListFormat` 识别列表请求并按客户端列表格式转换（6 向直转，不经 anthropic pivot），列表响应里反向展开 alias 条目；列表错误响应同样按客户端列表格式重建
- thinking signature 重试：上游返回 400 且 `shouldRectifyThinkingSignature` 命中时，调用 `rectifyAnthropicRequest` 清理 thinking 块后重试同一 upstream（最多 1 次）
- `writeJSON` 统一 JSON 响应写入（`globals.go` 中定义）
- 辅助函数：`maskURL` / `maskHeadersStr` / `forwardStreamHeaders` / `removeHopHeaders` / `maskFakeToken` 用于日志脱敏和头处理

### gemini_shadow.go

- 全局 `geminiShadowStore`：`map[string]geminiShadowEntry`，按 `tool_call_id` 维度存储 Gemini 的 `thoughtSignature`（签名字符串）与完整 assistant turn `parts` 数组（含 `thought:true` 块）
- `storeGeminiThoughtSignature` / `lookupGeminiThoughtSignature`：签名存取（多轮工具调用回放签名，避免 Gemini 400）
- `storeGeminiAssistantTurn` / `lookupGeminiAssistantParts`：完整 parts 存取（回放原始 Gemini 形态的 thinking parts）
- 条目 1h TTL（`geminiShadowTTL`），惰性清理：读取时发现过期条目即删除
- 在 `geminiToAnthropicResponse`（非流式）与 `geminiToAnthropicStream`（流式首个 tool call）写入；在 `convertAnthropicMessagesToGeminiContents`（assistant turn 含 `tool_use` 块时）回放
- 局限：无 session 概念，用 `tool_call_id` 全局唯一作键；多副本部署不跨实例共享

### reasoning_vendor.go

- `isReasoningVendorIdentifier`：大小写不敏感匹配 `moonshot`/`kimi`/`deepseek`/`mimo`/`xiaomimimo` 关键词
- `normalizeThinkingHistoryForVendor`：对含 `tool_use` 块的 assistant 消息，剥离 thinking `signature`、空 thinking 文本替换为占位符 `"tool call"`、`redacted_thinking` 改写为 thinking 块、无 thinking 时插入占位 thinking 块
- `stripEffortIfThinkingDisabled`：`thinking.type != enabled` 时移除 `reasoning_effort` 与 `output_config.effort`（`output_config` 仅含 effort 时整体移除）
- byte 级封装 `normalizeThinkingHistoryForVendorInBytes` / `stripEffortIfThinkingDisabledInBytes` 供 `gateway.go` 在 pre-transform pass 调用

### cache_injector.go

- `injectCacheControl`：在 Anthropic 请求体注入最多 4 个 `cache_control: {"type":"ephemeral"}` 断点（tools 末尾、system 末尾、最后 assistant 消息末尾非 thinking block、最后 user 消息末尾）
- 已有断点不覆盖；`ttl` 为空或 `"5m"` 时不带 ttl 字段，其他值带 `"ttl"` 字段
- byte 级封装 `injectCacheControlIntoBytes` 供 `gateway.go` 在发送 body 为 Anthropic 格式时调用（含转换到 anthropic 与透传 anthropic→anthropic 两条路径）。`formatTransform` 为空时，若客户端格式为 anthropic 且 cacheInjection 启用，仍会注入断点。

### thinking_rectifier.go

- `thinkingSignatureErrorPatterns`：7 个正则匹配 Anthropic thinking signature 错误消息（`signature.*not.*valid`、`must start with a thinking block` 等）
- `shouldRectifyThinkingSignature`：检测错误响应体是否为 thinking signature 问题
- `rectifyAnthropicRequest`：移除所有 thinking/redacted_thinking 块、剥离非 thinking 块的 `signature` 字段、若最后一条 assistant 消息移除 thinking 后则移除顶层 `thinking` 配置

### transform.go（模型列表转换部分）

- `detectListFormat` + `targetListEndpointPath`：列表端点 path + auth header 判定客户端格式与对应上游端点路径（见上文「模型列表请求转换」）。
- 中性结构 `modelsList`/`modelsListEntry`：承载 4 种格式共有字段（ID/Created/CreatedAt/OwnedBy/DisplayName/Version/InputTokenLimit/OutputTokenLimit），作为 6 向直转的中间表示。
- 4 个解析器 `parseOpenAIModelsList`/`parseAnthropicModelsList`/`parseGeminiModelsList`（openai_chat 与 openai_responses 共用同一解析器）+ 3 个 builder `buildOpenAIModelsList`/`buildAnthropicModelsList`/`buildGeminiModelsList`，dispatch 由 `parseModelsListByFormat`/`buildModelsListByFormat` 完成。
- `reverseAliasesMap` + `applyAliasesReverseToList`：按 per-upstream `aliases` 的 value→key 反向展开模型列表 entry。两阶段实现使列表与请求路由一致：
  1. 先删除"被覆盖"条目——若某 entry 的 ID 等于某个 alias key（该客户端名已被重定向到另一真实模型），删除其原条目，避免显示与路由行为不符的字段。
  2. 再为每个指向真实名的 alias key 追加一条克隆 entry（字段全相同，仅 ID 改为 alias key）。
  alias 与真实名相同时（`k==v`）跳过；空字符串自指无效果。
- `applyAliasesReverseToListInPlace` + `ApplyAliasesReverseToListInPlaceBytes`：**直连场景**（无 `formatTransform`）的就地 JSON 实现——不经中性结构中转，直接改写 `data[]`/`models[]` 数组，保留供应商特有字段（中性结构未承载的字段不丢失）。`gemini` 的 `name` 字段以 `"models/x"` 尾段作为 ID 进行比较与还原。
- `TransformModelsListResponse`：**formatTransform 场景**的列表响应转换主入口，调用 parse → `applyAliasesReverseToList` → build → marshal。fast-path 规则：无 alias 且同格式（`needsTransform=false`）或 `openai_chat↔openai_responses` 间原样透传；有 alias 或需跨格式时统一走 parse→build（含 alias 反向展开）。
- 网关列表分支选择（`gateway.go`）：列表请求按 `outFormat` 区分两条路径——`outFormat != ""` 走 `TransformModelsListResponse`（中性结构 + alias）；`outFormat == ""` 但配置了 alias 时走 `ApplyAliasesReverseToListInPlaceBytes`（就地 JSON + alias），无 auth 头重置、错误响应原样透传。两条路径均执行"删除覆盖条目 + 追加 alias 克隆"，使模型列表显示与请求路由的 alias 行为完全一致。

## 许可证

本项目使用MIT许可证。
