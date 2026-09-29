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

## v1.4 [3c5c55d]-[fd3050f] — 2026-07-10 ~ 2026-08-17

### Added

- **多端口监听**（`0ab66a1`）：将单一 `-p`/`-port` int flag 替换为可重复指定的 `addrList` flag.Value，接受纯端口（`9090`）或 `host:port`（`127.0.0.1:9091`、`localhost:9092`、`:9093`）两种形式。
  - 新增 `normalizeListenAddr` 校验/规整每个值（端口范围 1-65535，经 `net.SplitHostPort`）；host 合法性留到 `net.Listen` 绑定期裁决，`localhost` 等主机名可用。
  - 所有 listener 共享同一 mux 与全局状态（tokenMap/stateMap/DB/scheduler）；绑定全部成功后才开始 serve，任一端口冲突即 fail-fast，杜绝半启动。
  - 每个 listener 独立 goroutine serve；SIGINT/SIGTERM 时在共享 `shutdownCtx` 预算内并发关闭，全部退出后再关闭 DB。
  - 未传 `-p` 时默认 `:9090` 不变，兼容旧版单端口用法。

- **IP 账号认证**（`fa6978c`、`3f291a5`）：新增可选的基于 IP 的账号认证机制，由 `-auth` flag 启用。
  - 启用后所有 API 请求（`/` 路由）要求客户端 IP 已通过 `/login` 登录，否则统一返回 `401 Unauthorized`；未启用时行为不变，接收所有 IP 来源请求。
  - `/login` 提供登录表单与会话管理面板（列表展示该账户已登录的 IP），`/login/logout` 登出；`-account` 交互式添加账户（密码输入隐藏明文，bcrypt 哈希存储）。
  - 新增 `accounts` 与 `ip_sessions` 表（含 `login_at`，外键级联删除）；启动时 `loadAuthFromDB` 将会话加载到内存（`ipSessions` + `authMu` 读写锁）；`-auth` 启用但库中无账号时打印警告并退出。
  - `-e`/`-i` 导入导出扩展支持 accounts 与 ip_sessions。
  - `/status` 端点（`3f291a5`）在 `-auth` 启用时同样纳入 `authMiddleware` 保护；`/status/check` 与 `/login` 保持不受限。

- **Per-upstream 代理 + per-model 覆盖**（`121ffb2`）：新增 `UpstreamConfig.Proxy`（该 upstream 所有出站请求统一代理，含 provider 可用性检查）与 `ModelProxies`（per-model 覆盖，key 为 alias 替换/格式转换后实际发往上游的模型名 `sendModel`，而非客户端请求名）。
  - 生效规则（优先级递减）：`modelProxies[sendModel]` 命中 → 覆盖 `Proxy`（value 空串 = 显式直连豁免）；未命中 → 回退 `Proxy`；未配置 → 直连，与旧版行为完全一致。
  - 新增 `src/proxy.go`：按代理 URL 懒加载缓存 `*http.Transport` + plain/stream client 对（超时对齐 `proxyClient`/`streamClient`），同一 URL 只建一个连接池，经 `getProxyTransport` 与 provider 检查路径共享。
  - 支持 `http://` / `https://` / `socks5://`（URL 内嵌 user:pass 认证；socks5 由标准库在代理侧解析主机名）。
  - `gateway.go` 在 alias/transform 后按最终 `sendModel` 逐次解析代理；命中时记 `[PROXY]` 日志且脱敏凭据。
  - `providers.go`：`httpGetRaw/JSON/Text` 新增 Via 变体接收 transport；DeepSeek/OpenCode-Go 可用性检查经 upstream `Proxy` 发出，避免 targetBase 需代理可达时检查误判。
  - `state.go`：`upstreams` 表新增 `proxy` / `model_proxies` 列，旧库启动 `ALTER TABLE ADD COLUMN` 自动迁移；`-e`/`-i` 往返。
  - 校验 fail-fast：`proxy` / `modelProxies` 任何 value 非法（无法解析、scheme 非三种、host 为空）→ 启动 `log.Fatal` / 导入拒绝且不触碰 DB；`/status/check` 不回显代理配置（可能含凭据）。

- **跨平台构建脚本**（`d6465c9`）：新增 `build.bat`（Windows）与 `build.sh`（Linux/macOS），自 `src/` 构建剥离符号的 `api-gateway` 二进制（`-ldflags="-s -w"`）。

### Fixed

- **IPv4/IPv6 字面量监听网络选择**（`06ab9f2`）：Go `net.Listen("tcp", addr)` 对通配地址（`0.0.0.0`、`::`、空 host）会提升为 dual-stack（AF_INET6 + V6ONLY=0），导致 `0.0.0.0:9090` 实际同时绑定 IPv4 与 IPv6。新增 `listenNetworkFor(addr)` 按字面量 host 选择网络：IPv4 字面量 → `tcp4`（仅 IPv4）；IPv6 字面量 → `tcp6`；空 host（`:port`）→ `tcp` 保持原 dual-stack 默认。`0.0.0.0:9090` 由此真正只监听 IPv4，默认 `:9090` 行为不变（参见 golang/go #7411、#17615、#48723）。

### Chore

- `.gitignore` 移除测试配置 `tools/` 与 `.trae/`（`3c5c55d`，并清除 CHANGELOG 中对应旧条目）。
- `.gitignore` 补充 `api-gateway`（无 `.exe` 后缀的 Linux/macOS 构建产物）与 `gateway.db.bak`（`fd3050f`）。

### Docs

- README flag 表与 quick-start 补充多端口示例（`0ab66a1`）。
- README 新增 IP 账号认证章节、`-auth`/`-account` flag 说明与启动流程、全局状态说明（`fa6978c`）。
- README 新增代理配置章节：`proxy`/`modelProxies` 示例 JSON、四层生效规则、可用性检查/列表请求走代理、校验 fail-fast、日志脱敏与 DB 持久化说明（`121ffb2`）。

## v1.5 [fd3050f]-[db426bb] — 2026-08-17 ~ 2026-09-29

### Added

- **HTTPS 监听**（`db426bb`）：`-p` / `-port` 的任意值可加 `https://` 前缀声明为 HTTPS 端口（如 `-p https://:9443`），支持与明文端口混用；`http://` 前缀为明文的显式写法（与不加前缀等价），scheme 大小写不敏感。新增 `-cert` / `-key` flag 提供 PEM 证书链与私钥，证书由用户自备（不生成、不申请）。
  - 端口解析链重构为 `parseListenSpec` → `splitListenScheme` → `normalizeListenAddr`，产出 `listenSpec{addr, tls}`；`addrList` 元素类型由 `string` 升为 `listenSpec`，去重 key 改为「协议+地址」，故同一地址的明文与 TLS 视为两个不冲突的端点。
  - 经 `http.Server.ServeTLS(ln, "", "")` 启动（证书由 `GetCertificate` 回调动态提供），ALPN 自动协商 `h2`/`http/1.1`；**未**自包 `tls.NewListener` + `Serve`，因为后者不会注册 HTTP/2 的 `TLSNextProto`，客户端会停在 HTTP/1.1。
  - 新增 `src/tls.go`：`certReloader` 按「路径 + mtime + size」指纹在每次握手惰性重载证书，使 certbot/acme.sh 类工具「写临时文件 + rename」替换证书后**无需重启进程**即生效；重载失败沿用上一份成功证书（仅记日志）而非中断服务。`logCertInfo` 打印 subject/SAN/有效期，剩余 < 14 天或已过期额外告警。锁范围刻意收窄（指纹计算与读盘解析均在锁外），因该回调每次握手都会触发。
  - 证书在 bind 阶段加载，缺失/不匹配/损坏均在启动时 `log.Fatal`，与现有多端口「全部绑定成功才 serve」的 fail-fast 策略一致；存在 https 端口但缺 `-cert`/`-key` 亦启动即退出，反之（给了证书但无 https 端口）仅告警。
  - **每个 https 端口持有独立的 `*tls.Config`**（证书加载器共用）：`net/http` 初始化时会无锁 `append` 到 `TLSConfig.NextProtos`（`http2ConfigureServer` 在 `cloneTLSConfig` 之前执行），共享同一实例在多端口并发启动时构成数据竞争；已加载的 `*tls.Certificate` 不可变故可共享。
  - 显式 `MinVersion = TLS1.2`，拒绝 TLS 1.0/1.1；`NextProtos` 显式预设。
  - 就绪日志由 `Gateway running on %s` 改为带协议的形式（`http://:9090` / `https://:9443`）。
  - 仅使用标准库 `crypto/tls` / `crypto/x509`，**未引入任何新依赖**。

### Docs

- `README.md` 新增「HTTPS 监听」章节：用法、证书格式、HTTP/2、per-port 配置、热重载、最低版本、启动日志、与 `-auth` 的关系，以及不支持重定向/SNI/mTLS 的说明（`db426bb`）。
- `README.md` flag 表补充 `-cert`/`-key` 与 `-p` 的 `https://` 前缀；多端口示例增补混合监听；`main.go` 文件说明重写并新增 `tls.go` 说明（`db426bb`）。
- `README.md` 补充警告——若用 nginx/caddy 前置终止 TLS，`-auth` 仅取 `RemoteAddr` 且**不解析 `X-Forwarded-For`**，所有请求会被视为来自代理 IP 从而共享同一登录会话（`db426bb`）。
- `.gitignore` 新增 `*.crt` / `*.key` / `*.pem`（证书含明文私钥，不得入库）（`db426bb`）。