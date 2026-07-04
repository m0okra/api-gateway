# api-gateway

Go + SQLite 轻量级 API 反向代理网关，兼容三大主流AI API（OpenAI/Anthropic/Google Gemini），支持多 token 轮转、可用性检查、自动恢复与状态持久化。

## 目录结构

```
src/
├── main.go            入口：flag 解析、加载配置、启动 HTTP 服务器、优雅关闭
├── globals.go         全局变量：配置状态、共享 HTTP 客户端、writeJSON 工具
├── constants.go       常量定义：超时阈值、可用性类型共用名、并发与超时保护常量
├── types.go           数据结构：TokenMapConfig/UpstreamConfig/AvailabilityConfig/...
├── singleflight.go    可用性检查 singleflight（自实现，仅用标准库）
├── cron.go            轻量 Cron 解析调度 + LRU 缓存
├── providers.go       可用性 Provider：DeepSeek 余额、OpenCode-Go 用量等检查实现
├── state.go           状态管理：从 SQLite 加载/保存（锁外 I/O 快照写）、一致性协调
├── scheduler.go       定时调度：exhaust 恢复检查 + 状态持久化触发
├── aliases.go         模型别名：从 aliases.json 加载别名映射（不存在则禁用）
├── transform.go       API 格式转换：4 格式 × 请求/响应转换器（openai/anthropic/gemini 互转，anthropic pivot）
├── transform_stream.go SSE 流式转换状态机（6 方向 + 链式，经 anthropic pivot）
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
| `-p` / `-port` | 运行端口，默认 `9090` |
| `-db` | SQLite 数据库文件路径，默认 `gateway.db` |
| `-aliases` | 模型别名配置文件路径，默认 `aliases.json`（文件不存在则禁用别名功能） |
| `-e <file>` / `-export <file>` | 将 `-db` 库全量导出为 JSON 文件后退出（不启动服务器） |
| `-i <file>` / `-import <file>` | 将 JSON 文件全量导入 `-db` 库后退出（不启动服务器，全量覆盖） |

`-e` 与 `-i` 互斥。

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
- 表结构：`upstreams`（配置列 + 运行时状态列同行）、`upstream_tiers`（usage 型层级配置+状态）、`upstream_extra`（Extra map）、`fake_tokens`（fakeToken→有序 upstream 队列，priority=队列下标）。
- 恢复调度依据按类型二选一：
  - **count 型**：`RecoveryCron`（cron 表达式，对应配置的 `RefreshCron`）
  - **usage/balance/exhaust 型**：`RecoveryAt`（精确时间点 `time.Time`，由 provider 在 exhaust 时设定）
- 内存中维护 `stateDirty` 标志与 `stateGen` 代际计数器（`atomic.Uint64`），每 5min 检查并写入。
- `saveState()` 优化为锁内快照 → 锁外 I/O：在写锁内深拷贝 state 快照并记录代际后立即释放锁，SQLite 事务在锁外执行；提交后再次取锁，仅当代际未变（无新写入）才清 dirty，避免写库期间（可能数百 ms）阻塞所有请求。
- 调度器退出前 final save 确保持久化一致性；main 通过 `schedDone` channel 等待 final save 完成后再 `db.Close()`，避免事务被截断。
- 启动时清理 FakeTokens 队列中 Upstreames 不存在的 upstream（配置一致性保护）。
- 运行时 upstream 队列轮转顺序不持久化（重启恢复 `fake_tokens.priority` 定义的配置顺序）。
- 旧文件迁移：usage/balance/exhaust 型旧 `RecoveryCron` 字段在重启后被忽略，`RecoveryAt` 为零值触发立即复查，首次写入后清空旧字段。

### 配置导入导出

两个一次性管理 flag，执行后立即退出，不启动 HTTP 服务器：

- `-e <file>` / `--export <file>`：将 `-db` 库全量导出为单个 JSON 文件
- `-i <file>` / `--import <file>`：将 JSON 文件全量导入 `-db` 库（**全量覆盖**，DB 中原有数据被清空替换）

两者互斥。JSON 文件格式为 `DBDump`：`{ "tokenMap": {fakeTokens, upstreams}, "state": {upstream: AvailabilityState} }`，结构与运行时内存模型一致，`time.Time` 走 RFC3339Nano 字符串，人类可读可编辑。

导入防御：fakeToken 队列中重复 upstream 跳过保留首次、引用不存在的 upstream 跳过避免 FK 违约、state 中有但 upstreams 中无的孤儿条目警告忽略；全程单事务，任一步失败回滚保持 DB 原状。

典型用途：备份/恢复 `gateway.db`、手工编辑配置后导入、跨环境迁移。导出文件含明文 `realToken`，需自行保护文件权限。

### 模型别名

通过 `aliases.json` 文件提供请求模型名 → 上游真实模型名的映射，用于在转发前重写请求中的模型名。**默认不启用**：仅当 `-aliases` 指定的文件（默认 `aliases.json`，相对工作目录）存在时启用；文件不存在则别名功能保持禁用，请求原样转发。

文件格式为 `map[string]string`，key 与 value 均为字符串：

```json
{
  "gpt-4-turbo": "gpt-4",
  "claude-3-opus": "claude-3-opus-20240229",
  "gemini-pro": "gemini-1.5-pro"
}
```

替换规则：

- 仅当请求中提取到模型名（body 的 `model` 字段，或 Gemini 风格 URL path `/models/{name}`）且该模型名命中 `aliases.json` 的 key 时，替换为对应 value。
- 同时覆盖三种 API 风格：
  - OpenAI/Anthropic 风格：重写请求体 JSON 的 `model` 字段（重新序列化 body，`Content-Length` 由 transport 自动重算）。
  - Gemini 风格：重写 URL path 中的模型名段（如 `/v1beta/models/gemini-pro:generateContent` → `/v1beta/models/gemini-1.5-pro:generateContent`）。
- **不处理模型列表请求**（如 `GET /v1/models`）：此类请求无模型名，天然跳过替换。
- body 非 JSON 或 `model` 字段非字符串时静默跳过 body 重写（不影响 URL path 重写）。
- 仅在启动时一次性加载，运行期只读；修改 `aliases.json` 后需重启生效。文件解析失败时记录错误日志并禁用别名功能（不阻断启动）。

### API 格式转换（formatTransform）

每个 upstream 可选配置 `formatTransform` 字段，使网关在转发前将客户端请求体转换为目标上游的 API 格式，并在响应返回时反向转换回客户端格式。**不配置或留空时完全透传**（字节级零改动，向后兼容）。

#### 可选值

| 值 | 目标格式 | 说明 |
|---|---|---|
| `openai` | OpenAI Chat Completions | 上游走 `/v1/chat/completions` |
| `openai_responses` | OpenAI Responses API | 上游走 `/v1/responses` |
| `anthropic` | Anthropic Messages | 上游走 `/v1/messages` |
| `gemini` | Google Gemini | 上游走 `/v1beta/models/{model}:generateContent`（流式 `:streamGenerateContent?alt=sse`） |
| 不指定 | — | 透传所有请求/响应 |

#### 转换规则

- **目标格式** = upstream 配置的 `formatTransform`。
- **客户端格式** 按请求 URL path 自动检测：`/v1/chat/completions`→openai、`/v1/responses`→openai_responses、`/v1/messages`→anthropic、`/v1beta/models/{model}:...`→gemini，其他→unknown（透传）。
- **同族透传**：`openai` 与 `openai_responses` 互转视为同族，原样透传（两者请求/响应结构差异在转换层忽略，按需求"透传其他"）。
- **链式转换**：跨族转换（如 openai↔gemini）经 anthropic 作为 pivot 中间格式两步完成，对调用方透明。
- **目标 path 重写**：转换路径下，目标 URL path 按目标格式规范端点替换；透传路径保留原始 `r.URL.Path`。
- **认证头重写**：转换路径下，按目标格式注入认证（gemini→`X-Goog-Api-Key`/`?key=`；anthropic→`x-api-key`+`anthropic-version`；openai→`Authorization: Bearer`）；透传路径沿用原 token 注入优先级（`X-Goog-Api-Key`/`?key=`/`X-Api-Key`/`Authorization`）。

#### 流式支持

6 个跨族方向全部支持 SSE 流式转换（openai_chat↔anthropic、openai_responses↔anthropic、gemini↔anthropic，以及经 anthropic pivot 的 openai↔gemini 链式）。流式转换器实现为状态机，按 SSE 事件块增量转换，保持上游→客户端的低延迟。Gemini 流式输出采用累积快照 diff 语义（每个 chunk 携带截至当前的完整内容）。

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

DB 直接编辑：`upstreams` 表 `format_transform` 列存配置值字符串（`openai`/`openai_responses`/`anthropic`/`gemini`/空）。`/status` 端点的响应中 `upstreams[].formatTransform` 字段会回显当前配置。

#### 限制说明

- **错误响应转换**：4xx/5xx 错误响应体在转换路径下会经 `TransformErrorResponse` 转为客户端格式后返回（包括可用性错误 401/402/403/429）。各厂商错误 JSON 结构差异较大，转换尽量保留 `error.message`/`type` 等通用字段，无法映射的字段按目标格式兜底。仅对 `resp.StatusCode < 300` 的成功响应走 `TransformResponse`。
- **请求转换失败 → 继续尝试下一个 upstream**：客户端请求体无法解析为目标格式时，不直接 400 中断，而是记录 `[TRANSFORM] request convert failed (will try next upstream)` 日志后继续尝试队列中的下一个 upstream（透传 upstream 不进入转换路径，可正常处理）。全部 upstream 均失败后由循环外兜底返回 `503`。
- **响应转换失败 → 回退原 body**：响应转换出错时记日志并原样返回上游响应（非流式）；流式转换 Feed 出错则中断流并记日志。
- **Gemini 工具调用 ID**：Gemini `functionCall` 无独立 ID 字段，转换到 anthropic/openai 时用无状态启发式合成 ID（基于调用顺序），可能与上游真实 ID 不一致；反向（anthropic/openai→gemini）时 ID 丢失。
- **OpenAI 同族透传**：`openai` ↔ `openai_responses` 不做结构转换，仅透传。若客户端用 chat completions 格式请求一个 `formatTransform: "openai_responses"` 的 upstream，请求/响应原样转发，需客户端自行兼容。
- **非法值兜底**：`formatTransform` 配置为非上述 4 个合法值时，记 `[TRANSFORM] invalid formatTransform ... -> passthrough` 警告日志后按透传处理，不报错。

### 状态查询（/status）

`GET /status` 返回当前网关内存运行时状态的脱敏快照，供运维排查使用：

```bash
curl http://localhost:9090/status
```

响应体：

```json
{
  "upstreams": [
    {
      "name": "deepseek-aaa",
      "targetBase": "https://api.deepseek.com",
      "realToken": "sk-a********aaaa",
      "availType": "balance",
      "exhausted": false,
      "balance": 12.34,
      "recoveryAt": "0001-01-01T00:00:00Z",
      "queueFor": ["sk...aa"]
    },
    {
      "name": "gemini",
      "targetBase": "https://generativelanguage.googleapis.com",
      "realToken": "AIb********bbbb",
      "availType": "count",
      "exhausted": false,
      "count": 10,
      "recoveryCron": "0 0 16 * * *",
      "lastChecked": "2026-06-30T11:53:01.91Z",
      "queueFor": ["sk...bb"]
    },
    {
      "name": "opencode-go",
      "targetBase": "https://opencode.ai/zen/go",
      "realToken": "sk-c********cccc",
      "availType": "usage",
      "exhausted": true,
      "tiers": [
        {"name": "rolling", "usedPct": 100, "resetInSec": 2592000}
      ],
      "recoveryAt": "2026-07-11T09:09:29Z",
      "queueFor": ["sk...cc"]
    }
  ],
  "fakeTokens": {
    "sk...aa": ["deepseek-aaa", "gemini"],
    "sk...cc": ["opencode-go"]
  }
}
```

| 字段 | 说明 |
|---|---|
| `upstreams` | 每个 upstream 的运行时快照（配置+状态） |
| `upstreams[].realToken` | 真实 API Key 脱敏视图（首尾各 4 字符 + `********`） |
| `upstreams[].queueFor` | 该 upstream 出现在哪些 fakeToken 队列中（脱敏），便于排障 |
| `upstreams[].tiers` | usage 型的各配额层级用量（count/balance 型无此字段） |
| `fakeTokens` | 当前 fakeToken → upstream 队列映射，fakeToken 名脱敏 |

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

调度器每 1s 遍历所有 exhausted upstream，检查 `now >= RecoveryAt`（零值视为旧文件迁移，立即触发）。触发后调用 provider 复查，返回新的 exhausted 状态和 `RecoveryAt`；若已恢复则清除 exhausted，upstream 重新参与请求轮转。

### 请求体大小限制

单请求体上限 32MB。使用 `http.MaxBytesReader` 包裹请求体，超限时内部自动写入 413 状态码并返回 `*http.MaxBytesError`。代码通过 `errors.As(rerr, &maxBytesErr)` 判断超限后直接 return，避免二次 WriteHeader 触发 "superfluous" 警告。

## 各文件逻辑详解

### main.go

1. 解析 `-p` / `-port` / `-db` / `-e`(`-export`) / `-i`(`-import`) flag
2. 若指定 `-e` 或 `-i`：执行导出/导入后 `os.Exit(0)`，不启动服务器（管理操作，互斥）
3. 调用 `loadFromDB()` 从 SQLite 加载配置与状态（统一数据源）
4. 启动 `runScheduler` goroutine
5. 初始化 `reqSem` 并发信号量（channel semaphore，容量 256），handler 入口 acquire、defer release
6. 启动 HTTP server，监听 `:port`，配置 `ReadTimeout=10s` / `IdleTimeout=120s` / `MaxHeaderBytes=1MB`（防御慢速连接攻击；`WriteTimeout=0` 保护流式 SSE）
7. 等待 SIGINT/SIGTERM，触发优雅关闭（等待 scheduler final save 完成后关闭 DB）

### globals.go

- 包级全局变量（`tokenMap`、`stateMap`、`mu sync.RWMutex`、`stateGen atomic.Uint64`、`db`、`dbPath` 等）
- 共享 HTTP 客户端（复用 Transport，避免每次创建新连接）：
  - `defaultClient`（15s 超时）— provider 可用性检查
  - `proxyClient`（120s 超时）— 普通代理请求
  - `streamClient`（无整体超时）— 流式代理请求
- `reqSem`（channel semaphore，容量 256）：全局并发上限，handler 入口 acquire，超限请求阻塞排队
- `availSF`（`availSingleFlight`）：可用性检查 singleflight，同 upstream 并发触发仅首次执行 provider 调用
- `writeJSON` 统一 JSON 响应写入，捕获并记录编码错误

### state.go

- `openDB`：打开 SQLite（`PRAGMA foreign_keys=ON` + `WAL` + `busy_timeout`）并建表 IF NOT EXISTS，供 `loadFromDB` 与 `importFromJSON` 复用
- `loadFromDB`：查 4 张表填充 `tokenMap` + `stateMap`、`cleanFakeTokenQueues` + `reconcileStateWithConfig`
- `saveState`：单事务写回——先持写锁深拷贝 state 快照（含 Tiers 副本）并记录 `stateGen`，释放锁后遍历快照执行 SQLite I/O（`UPDATE upstreams` 状态列 + 每个 upstream 的 `upstream_tiers` DELETE/INSERT）；提交后再取锁，仅当代际未变才清 dirty。锁外 I/O 避免写库阻塞所有请求。
- `exportToJSON`：复用 `loadFromDB` 载入内存 → marshal `DBDump{tokenMap, stateMap}` → 原子写（tmp+rename，0600）；导出 reconcile 后的规范视图
- `importFromJSON`：读 JSON → 单事务 DELETE 4 表（先子后父）+ INSERT 全量覆盖；防御：队列重复 upstream 用 `INSERT OR IGNORE` 跳过、引用不存在 upstream 跳过、orphan state 警告；任一步失败 Rollback

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
- 其他 provider（Kimi / OpenRouter / Claude / Codex / Gemini / ZAI / MiniMax）仅框架返回 exhaust（始终耗尽+30min 复查）
- `httpGetJSON` / `httpGetText` 使用共享 `defaultClient`

### scheduler.go

- `runScheduler`：主循环 select ticker（1s 恢复检查）与 saveTicker（5min 状态保存）；通过 `done` channel 通知 main 已完成 final save
- `checkRecovery`：遍历 all upstream，按类型分流：
  - **count 型**：`RecoveryCron` cron 周期匹配
  - **usage/balance/exhaust 型**：`now >= RecoveryAt` 时间点触发（零值为旧文件迁移，立即触发）
  - 统一受 `recoveryMinGap`（60s）约束，避免短时间重复触发
- `recoverUpstream`：count 型直接重置计数并清除 exhausted；其余类型通过 `availSF.Do()` singleflight 去重调用 provider 复查（避免与 handler 路径并发重复检查），`applyAvailabilityResult` 写入新 `RecoveryAt`

### aliases.go

- `loadAliases`：启动期一次性加载 `-aliases` 指定的 JSON 文件为 `map[string]string` 并赋值全局 `aliases`。文件不存在 → `aliases` 保持 nil（禁用，非错误）；解析失败 → 记录错误日志且 `aliases` 保持 nil（禁用，不阻断启动）；加载成功 → `aliases` 非 nil（含空 map）视为已启用。运行期只读，无锁。

### gateway.go

- handler：核心请求处理函数，以 `reqSem <- struct{}{}` 获取并发令牌（defer 释放），完成后返回
- 队列操作：`pickFirstAvailableUpstream`（一次写锁内原子扫描队列，跳过所有 exhausted 前置 upstream 至队尾，返回首个可用 upstream 与配置快照）/ `rotateUpstreamToEnd` / `getUpstreamQueueLen` / `hasUpstreamQueue`
- 状态查询：`incrementCount`（count 型自增）、`applyAvailabilityResult`（写入检查结果；按类型分流：count 写 `RecoveryCron`、其余写 `RecoveryAt`，并清理另一调度依据的残留）
- 可用性错误（401/402/403/429）路径通过 `availSF.Do()` singleflight 去重 checkAvailability，避免并发请求重复调用 provider
- 流式响应：idle 监控 goroutine + `maxStreamLife` context 超时硬上限，双保险
- `writeJSON` 统一 JSON 响应写入（`globals.go` 中定义）
- 辅助函数：`maskURL` / `maskHeadersStr` / `forwardStreamHeaders` / `removeHopHeaders` / `maskFakeToken` 用于日志脱敏和头处理

## 许可证

本项目使用MIT许可证。
