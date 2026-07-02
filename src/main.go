package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

// ============================================================================
// main
// ============================================================================

func main() {
	var port int
	var exportPath, importPath string
	flag.IntVar(&port, "p", 9090, "运行端口")
	flag.IntVar(&port, "port", 9090, "运行端口")
	flag.StringVar(&dbPath, "db", "gateway.db", "SQLite 数据库文件路径")
	flag.StringVar(&exportPath, "e", "", "导出：将 -db 库全量导出为该 JSON 文件后退出")
	flag.StringVar(&exportPath, "export", "", "导出：将 -db 库全量导出为该 JSON 文件后退出")
	flag.StringVar(&importPath, "i", "", "导入：将该 JSON 文件全量导入 -db 库后退出")
	flag.StringVar(&importPath, "import", "", "导入：将该 JSON 文件全量导入 -db 库后退出")
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

	if port < 1 || port > 65535 {
		log.Fatalf("Invalid port: %d (must be 1-65535)", port)
	}

	// 1. 从 SQLite 加载配置与状态（统一数据源）
	dbExisted := true
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		dbExisted = false
	}
	if err := loadFromDB(); err != nil {
		log.Fatalf("Failed to load from DB: %v", err)
	}
	log.Printf("TokenMap loaded from DB (fakeTokens=%d, aliases=%d)",
		len(tokenMap.FakeTokens), len(tokenMap.Aliases))
	if !dbExisted || len(tokenMap.Aliases) == 0 {
		log.Printf("请使用 -i example.json 导入配置，或直接用 sqlite3 CLI 编辑 gateway.db 后重启。")
	}

	// 2. 启动调度goroutine（exhaust恢复 + 状态保存）
	schedStop := make(chan struct{})
	schedDone := make(chan struct{})
	go runScheduler(schedStop, schedDone)

	// 3. 初始化并发信号量（channel semaphore，容量 = maxConcurrentReqs）
	reqSem = make(chan struct{}, maxConcurrentReqs)

	// 4. 启动HTTP服务器
	//    WriteTimeout 保持 0：流式 SSE 响应可能持续超过 5min，设写超时会中断合法流。
	//    ReadTimeout/IdleTimeout/MaxHeaderBytes 用于防御慢速连接与超大头部攻击。
	mux := http.NewServeMux()
	mux.HandleFunc("/status", statusHandler)
	mux.HandleFunc("/", handler)

	server := &http.Server{
		Addr:           ":" + strconv.Itoa(port),
		Handler:        mux,
		ReadTimeout:    serverReadTimeout,
		WriteTimeout:   0,
		IdleTimeout:    serverIdleTimeout,
		MaxHeaderBytes: serverMaxHeaderBytes,
	}

	go func() {
		log.Printf("Gateway running on port %d", port)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Server error: %v", err)
		}
	}()

	// 4. 优雅关闭
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	log.Println("Shutting down...")
	close(schedStop) // 停止调度器（退出前会保存脏状态）
	<-schedDone      // 等待调度器完成 final save，避免 db.Close() 先于事务提交
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	server.Shutdown(ctx)
	if db != nil {
		db.Close()
	}
}
