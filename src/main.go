package main

import (
	"context"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"
)

// addrList 为可重复 -p/-port flag 的自定义 Value 类型，
// 元素统一规整为 ":port" 或 "host:port" 形式，供 http.Server.Addr 直接使用。
type addrList []string

// String 实现 flag.Value 接口。
func (a *addrList) String() string {
	if a == nil || len(*a) == 0 {
		return ""
	}
	out := ""
	for i, s := range *a {
		if i > 0 {
			out += ","
		}
		out += s
	}
	return out
}

// Set 解析单次 -p 值并 append。
// 支持两种格式：
//   - 纯端口 "9090" → 规整为 ":9090"
//   - "host:port"（如 "127.0.0.1:9090"、":9090"、"0.0.0.0:9091"）
//
// 非法值直接 log.Fatalf，与项目 fail-fast 风格一致。
func (a *addrList) Set(v string) error {
	addr, err := normalizeListenAddr(v)
	if err != nil {
		log.Fatalf("%v", err)
	}
	*a = append(*a, addr)
	return nil
}

// normalizeListenAddr 把命令行原始值规整为 net.Listen 可直接使用的 "host:port" 形式。
// 纯端口 → ":port"；"host:port" 原样保留并校验。
func normalizeListenAddr(raw string) (string, error) {
	if raw == "" {
		return "", &listenAddrError{raw: raw, reason: "empty address"}
	}
	// 不含 ':' 且全为数字 → 视为纯端口
	if !containsColon(raw) {
		if _, err := strconv.Atoi(raw); err != nil {
			return "", &listenAddrError{raw: raw, reason: "invalid port (numeric expected)"}
		}
		port, _ := strconv.Atoi(raw)
		if port < 1 || port > 65535 {
			return "", &listenAddrError{raw: raw, reason: "port out of range 1-65535"}
		}
		return ":" + raw, nil
	}
	// 含 ':' → 走 net.SplitHostPort 校验（兼容 ":9090"、"127.0.0.1:9090"、"[::1]:9090"）
	_, portStr, err := net.SplitHostPort(raw)
	if err != nil {
		return "", &listenAddrError{raw: raw, reason: err.Error()}
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return "", &listenAddrError{raw: raw, reason: "invalid port: " + portStr}
	}
	if port < 1 || port > 65535 {
		return "", &listenAddrError{raw: raw, reason: "port out of range 1-65535"}
	}
	// host 类型不做预校验，交由 net.Listen 在绑定阶段裁决。
	return raw, nil
}

// containsColon 判断字符串是否包含冒号；抽取为独立小函数以便内联优化。
func containsColon(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == ':' {
			return true
		}
	}
	return false
}

// listenAddrError 提供可读的失败原因描述。
type listenAddrError struct {
	raw, reason string
}

func (e *listenAddrError) Error() string {
	return "invalid listen address " + strconv.Quote(e.raw) + ": " + e.reason
}

// ============================================================================

func main() {
	var addrs addrList
	var exportPath, importPath string
	var addAccount bool
	// -p / -port 共享同一 addrList，可重复指定，向后兼容单端口用法（未传则默认 :9090）
	flag.Var(&addrs, "p", "运行端口，可重复指定（纯端口或 host:port），未传默认 :9090")
	flag.Var(&addrs, "port", "运行端口，可重复指定（纯端口或 host:port），未传默认 :9090")
	flag.StringVar(&dbPath, "db", "gateway.db", "SQLite 数据库文件路径")
	flag.StringVar(&exportPath, "e", "", "导出：将 -db 库全量导出为该 JSON 文件后退出")
	flag.StringVar(&exportPath, "export", "", "导出：将 -db 库全量导出为该 JSON 文件后退出")
	flag.StringVar(&importPath, "i", "", "导入：将该 JSON 文件全量导入 -db 库后退出")
	flag.StringVar(&importPath, "import", "", "导入：将该 JSON 文件全量导入 -db 库后退出")
	flag.BoolVar(&authEnabled, "auth", false, "启用 IP 账号认证")
	flag.BoolVar(&addAccount, "account", false, "交互式添加账户后退出")
	flag.Parse()

	// 管理操作分支：导出/导入互斥，执行后立即退出，不启动 HTTP 服务器
	if exportPath != "" && importPath != "" {
		log.Fatalf("-e/-export 与 -i/-import 互斥，请单独使用")
	}
	if exportPath != "" {
		if err := exportToJSON(exportPath); err != nil {
			log.Fatalf("Export failed: %v", err)
		}
		return
	}
	if importPath != "" {
		if err := importFromJSON(importPath); err != nil {
			log.Fatalf("Import failed: %v", err)
		}
		return
	}
	if addAccount {
		createAccountFlow()
		return
	}

	// 未传任何 -p/-port 时回退到默认 :9090，保持向后兼容
	if len(addrs) == 0 {
		addrs = addrList{":9090"}
	}
	// 去重检测：同一 addr 重复传入视为配置错误
	seen := make(map[string]bool, len(addrs))
	for _, a := range addrs {
		if seen[a] {
			log.Fatalf("Duplicate listen address: %s", a)
		}
		seen[a] = true
	}

	// 1. 从 SQLite 加载配置与状态（统一数据源）
	dbExisted := true
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		dbExisted = false
	}
	if err := loadFromDB(); err != nil {
		log.Fatalf("Failed to load from DB: %v", err)
	}
	log.Printf("TokenMap loaded from DB (fakeTokens=%d, upstreams=%d)",
		len(tokenMap.FakeTokens), len(tokenMap.Upstreams))
	if !dbExisted || len(tokenMap.Upstreams) == 0 {
		log.Printf("请使用 -i example.json 导入配置，或直接用 sqlite3 CLI 编辑 gateway.db 后重启。")
	}

	// 1.5 IP 账号认证初始化
	if authEnabled {
		accountCount, err := loadAuthFromDB(db)
		if err != nil {
			log.Fatalf("Failed to load auth sessions: %v", err)
		}
		if accountCount == 0 {
			log.Fatalf("警告：-auth 已启用但数据库中没有任何账号，请先在 accounts 表中添加账号后重启")
		}
		log.Printf("Auth enabled (accounts=%d, active sessions=%d)", accountCount, len(ipSessions))
	}

	// 2. 启动调度goroutine（exhaust恢复 + 状态保存）
	schedStop := make(chan struct{})
	schedDone := make(chan struct{})
	// shutdownCtx 在收到停机信号后由 time.AfterFunc 延迟 cancel，

	// 供 scheduler final save 与 server.Shutdown 共享超时预算。
	shutdownCtx, cancelShutdown := context.WithCancel(context.Background())
	defer cancelShutdown()
	go runScheduler(shutdownCtx, schedStop, schedDone)

	// 3. 初始化并发信号量（channel semaphore，容量 = maxConcurrentReqs）
	reqSem = make(chan struct{}, maxConcurrentReqs)

	// 4. 启动 HTTP 服务器（多端口共享同一 mux 与全局状态）
	//    WriteTimeout 保持 0：流式 SSE 响应可能持续超过 5min，设写超时会中断合法流。
	//    ReadTimeout/IdleTimeout/MaxHeaderBytes 用于防御慢速连接与超大头部攻击。
	mux := http.NewServeMux()
	mux.HandleFunc("/status", authMiddleware(statusHandler))
	mux.HandleFunc("/status/check", statusCheckHandler)
	mux.HandleFunc("/login", loginHandler)
	mux.HandleFunc("/login/logout", logoutHandler)
	mux.HandleFunc("/", authMiddleware(handler))

	// 同步绑定所有 listener：任一端口冲突立即 log.Fatalf，避免半启动状态。
	// 全部绑定成功后再发就绪日志，保证对外可见时所有端口均已就绪。
	type srvEntry struct {
		server   *http.Server
		listener net.Listener
		addr     string
	}
	entries := make([]srvEntry, 0, len(addrs))
	for _, addr := range addrs {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			// 已绑定的 listener 在进程退出时由 OS 回收，无需显式关闭
			log.Fatalf("Failed to listen on %s: %v", addr, err)
		}
		srv := &http.Server{
			Handler:        mux,
			ReadTimeout:    serverReadTimeout,
			WriteTimeout:   0,
			IdleTimeout:    serverIdleTimeout,
			MaxHeaderBytes: serverMaxHeaderBytes,
		}
		entries = append(entries, srvEntry{server: srv, listener: ln, addr: addr})
	}

	for _, e := range entries {
		go func(e srvEntry) {
			log.Printf("Gateway running on %s", e.addr)
			if err := e.server.Serve(e.listener); err != nil && err != http.ErrServerClosed {
				log.Fatalf("Server error on %s: %v", e.addr, err)
			}
		}(e)
	}

	// 5. 优雅关闭
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	log.Println("Shutting down...")
	// 收到信号后启动超时定时器：saveStateTimeout 后强制 cancel shutdownCtx，
	// 保证 scheduler final save 与 server.Shutdown 都不会无限阻塞。
	timer := time.AfterFunc(saveStateTimeout, cancelShutdown)
	defer timer.Stop()
	close(schedStop)           // 停止调度器（退出前会用 shutdownCtx 做 final save）
	<-schedDone                // 等待调度器完成 final save，避免 db.Close() 先于事务提交

	// 并发关闭所有 listener，复用同一 shutdownCtx 超时预算
	var wg sync.WaitGroup
	for _, e := range entries {
		wg.Add(1)
		go func(e srvEntry) {
			defer wg.Done()
			_ = e.server.Shutdown(shutdownCtx)
		}(e)
	}
	wg.Wait()

	if db != nil {
		db.Close()
	}
}
