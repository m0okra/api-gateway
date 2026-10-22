package main

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
	"golang.org/x/term"
)

// ============================================================================
// IP 账号认证（-auth 启用）
// ============================================================================

var (
	authEnabled bool
	authMu      sync.RWMutex
	ipSessions  map[string]string // ip -> username
)

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// loadAuthFromDB 从 DB 加载 ip_sessions 到内存，并返回账号数量。
func loadAuthFromDB(conn *sql.DB) (accountCount int, err error) {
	ipSessions = make(map[string]string)

	if err := conn.QueryRow(`SELECT COUNT(*) FROM accounts`).Scan(&accountCount); err != nil {
		return 0, err
	}

	rows, err := conn.Query(`SELECT ip, username FROM ip_sessions`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var ip, username string
		if err := rows.Scan(&ip, &username); err != nil {
			return 0, err
		}
		ipSessions[ip] = username
	}
	return accountCount, rows.Err()
}

func authMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if authEnabled {
			ip := clientIP(r)
			authMu.RLock()
			_, ok := ipSessions[ip]
			authMu.RUnlock()
			if !ok {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "Unauthorized"})
				return
			}
		}
		next(w, r)
	}
}

// authMiddlewareRedirect 与 authMiddleware 相同，但未登录时 302 到 /login 而非 401 JSON。
// 仅用于面向人浏览的 HTML 页面路由（/status）；API 路由必须用 authMiddleware，
// 保证错误响应始终是可被程序解析的 JSON。
func authMiddlewareRedirect(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if authEnabled {
			ip := clientIP(r)
			authMu.RLock()
			_, ok := ipSessions[ip]
			authMu.RUnlock()
			if !ok {
				http.Redirect(w, r, "/login", http.StatusFound)
				return
			}
		}
		next(w, r)
	}
}

// authDisabledHandler 用于 -auth 未启用时显式注册认证端点：返回 404 JSON。
// 不注册而让请求落到 catch-all "/" 会让登录页访问者收到「token 缺失」这类
// 与认证无关的错误，非常迷惑。
func authDisabledHandler(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusNotFound, map[string]string{"error": "Not found (-auth not enabled)"})
}

// ============================================================================
// /login 端点
// ============================================================================

func loginHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		loginPageHandler(w, r)
	case http.MethodPost:
		loginSubmitHandler(w, r)
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "Method not allowed"})
	}
}

func loginPageHandler(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	authMu.RLock()
	username, loggedIn := ipSessions[ip]
	authMu.RUnlock()

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if !loggedIn {
		w.Write([]byte(loginFormHTML))
		return
	}

	// 已登录：查询该账户所有 IP 会话
	authMu.RLock()
	var ips []string
	for sessIP, u := range ipSessions {
		if u == username {
			ips = append(ips, sessIP)
		}
	}
	authMu.RUnlock()

	data := loginDashboardData{CurrentIP: ip, Username: username, IPs: ips}
	w.Write([]byte(renderDashboard(data)))
}

type loginDashboardData struct {
	CurrentIP string
	Username  string
	IPs       []string
}

func renderDashboard(d loginDashboardData) string {
	rows := ""
	for _, ip := range d.IPs {
		current := ""
		if ip == d.CurrentIP {
			current = " (当前)"
		}
		rows += `<tr><td>` + escapeHTML(ip) + current + `</td><td><button onclick="logout('` + escapeHTML(ip) + `')">退出</button></td></tr>`
	}
	return `<!DOCTYPE html>
<html lang="zh">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Login - Dashboard</title>
<style>
*{box-sizing:border-box;margin:0;padding:0}
body{font-family:system-ui,sans-serif;background:#f5f5f5;display:flex;justify-content:center;align-items:center;min-height:100vh}
.card{background:#fff;border-radius:8px;padding:32px;width:100%;max-width:480px;box-shadow:0 2px 8px rgba(0,0,0,.1)}
h1{font-size:1.2rem;margin-bottom:16px;text-align:center}
.info{margin-bottom:16px;font-size:.9rem;color:#333}
.info span{font-weight:600}
table{width:100%;border-collapse:collapse;font-size:.85rem;margin-bottom:16px}
th,td{text-align:left;padding:6px 8px;border-bottom:1px solid #eee}
th{background:#fafafa;font-weight:600}
button{padding:6px 14px;border:none;border-radius:4px;background:#333;color:#fff;cursor:pointer;font-size:.8rem}
button:hover{background:#555}
.btn-danger{background:#c00}
.btn-danger:hover{background:#a00}
</style>
</head>
<body>
<div class="card">
<h1>IP 会话管理</h1>
<p class="info">当前 IP：<span>` + escapeHTML(d.CurrentIP) + `</span></p>
<p class="info">账户：<span>` + escapeHTML(d.Username) + `</span></p>
<table><tr><th>IP 地址</th><th>操作</th></tr>` + rows + `</table>
<button class="btn-danger" onclick="logoutAll()">退出所有 IP</button>
</div>
<script>
async function logout(ip){
  await fetch('/login/logout',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({ip})});
  location.reload();
}
async function logoutAll(){
  await fetch('/login/logout',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({all:true})});
  location.reload();
}
</script>
</body>
</html>`
}

func loginSubmitHandler(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid form"})
		return
	}
	username := r.FormValue("username")
	password := r.FormValue("password")
	if username == "" || password == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请提供用户名和密码"})
		return
	}

	ip := clientIP(r)

	// 爆破防护：锁定期内直接短路，不执行 bcrypt（否则防护治不了 CPU 燃烧）。
	// per-IP 命中 → 429 + Retry-After（告诉对方等待时长，避免无谓重试）；
	// per-账号命中 → 仍返回 401「用户名或密码错误」，不泄露哪些账号被锁。
	if remain := loginLimiter.ipLockRemaining(ip); remain > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(remain.Seconds()))))
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "尝试过于频繁，请稍后再试"})
		return
	}
	if loginLimiter.userLockRemaining(username) > 0 {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "用户名或密码错误"})
		return
	}

	var hash string
	err := db.QueryRow(`SELECT password_hash FROM accounts WHERE username=?`, username).Scan(&hash)
	if err != nil {
		loginLimiter.recordFailure(ip, username)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "用户名或密码错误"})
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) != nil {
		loginLimiter.recordFailure(ip, username)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "用户名或密码错误"})
		return
	}

	// 登录成功：清空两个维度的失败计数（含 per-IP 翻倍等级）
	loginLimiter.recordSuccess(ip, username)

	now := time.Now().Format(time.RFC3339)
	if _, err := db.Exec(`INSERT OR REPLACE INTO ip_sessions (ip, username, login_at) VALUES (?,?,?)`, ip, username, now); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Internal error"})
		return
	}

	authMu.Lock()
	ipSessions[ip] = username
	authMu.Unlock()

	http.Redirect(w, r, "/login", http.StatusFound)
}

// ============================================================================
// /login/logout 端点
// ============================================================================

type logoutRequest struct {
	IP  string `json:"ip"`
	All bool   `json:"all"`
}

func logoutHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "Method not allowed"})
		return
	}

	var req logoutRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid request"})
		return
	}

	currentIP := clientIP(r)

	authMu.RLock()
	currentUser := ipSessions[currentIP]
	authMu.RUnlock()

	if currentUser == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "Unauthorized"})
		return
	}

	if req.All {
		authMu.Lock()
		var toDelete []string
		for ip, u := range ipSessions {
			if u == currentUser {
				toDelete = append(toDelete, ip)
			}
		}
		for _, ip := range toDelete {
			delete(ipSessions, ip)
		}
		authMu.Unlock()

		if _, err := db.Exec(`DELETE FROM ip_sessions WHERE username=?`, currentUser); err != nil {
			log.Printf("[auth] logout all DB error: %v", err)
		}
	} else if req.IP != "" {
		authMu.RLock()
		targetUser := ipSessions[req.IP]
		authMu.RUnlock()

		if targetUser != currentUser {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "无权操作该 IP"})
			return
		}

		authMu.Lock()
		delete(ipSessions, req.IP)
		authMu.Unlock()

		if _, err := db.Exec(`DELETE FROM ip_sessions WHERE ip=?`, req.IP); err != nil {
			log.Printf("[auth] logout DB error: %v", err)
		}
	} else {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请指定 ip 或 all"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// ============================================================================
// 导出辅助
// ============================================================================

func dumpAccounts(conn *sql.DB) ([]AccountDump, error) {
	rows, err := conn.Query(`SELECT username, password_hash FROM accounts`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AccountDump
	for rows.Next() {
		var a AccountDump
		if err := rows.Scan(&a.Username, &a.PasswordHash); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func dumpIPSessions(conn *sql.DB) ([]IPSessionDump, error) {
	rows, err := conn.Query(`SELECT ip, username, login_at FROM ip_sessions`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []IPSessionDump
	for rows.Next() {
		var s IPSessionDump
		if err := rows.Scan(&s.IP, &s.Username, &s.LoginAt); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ============================================================================
// HTML 模板
// ============================================================================

func escapeHTML(s string) string {
	out := ""
	for _, c := range s {
		switch c {
		case '&':
			out += "&amp;"
		case '<':
			out += "&lt;"
		case '>':
			out += "&gt;"
		case '"':
			out += "&quot;"
		case '\'':
			out += "&#39;"
		default:
			out += string(c)
		}
	}
	return out
}

const loginFormHTML = `<!DOCTYPE html>
<html lang="zh">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Login</title>
<style>
*{box-sizing:border-box;margin:0;padding:0}
body{font-family:system-ui,sans-serif;background:#f5f5f5;display:flex;justify-content:center;align-items:center;min-height:100vh}
.card{background:#fff;border-radius:8px;padding:32px;width:100%;max-width:380px;box-shadow:0 2px 8px rgba(0,0,0,.1)}
h1{font-size:1.2rem;margin-bottom:20px;text-align:center}
.field{margin-bottom:14px}
label{display:block;font-size:.85rem;margin-bottom:4px;color:#555}
input{width:100%;padding:8px 12px;border:1px solid #ccc;border-radius:4px;font-size:.9rem}
button{width:100%;padding:10px;border:none;border-radius:4px;background:#333;color:#fff;cursor:pointer;font-size:.9rem;margin-top:6px}
button:hover{background:#555}
.error{color:#c00;margin-bottom:12px;font-size:.85rem;text-align:center}
</style>
</head>
<body>
<div class="card">
<h1>IP 认证登录</h1>
<div id="err"></div>
<form method="POST" action="/login">
<div class="field"><label>用户名</label><input name="username" autocomplete="username" required></div>
<div class="field"><label>密码</label><input name="password" type="password" autocomplete="current-password" required></div>
<button type="submit">登录</button>
</form>
</div>
</body>
</html>`

// ============================================================================
// 账户管理 CLI（-account / -del-account / -list-accounts / -passwd）
//
// 所有子命令均为「独立进程改库」，不影响已运行的网关：
// authMiddleware 只查内存 ipSessions，从不回查 accounts 表。
// 因此删除/改密后需重启运行中的网关才生效。
// ============================================================================

// requireExistingDB 校验 -db 指向的库文件已存在。
// list/del/passwd 针对的是已存在的账户库，路径打错应报错而不是
// 让 openDB 静默造出一个空白库（openDB 会 CREATE TABLE IF NOT EXISTS）。
func requireExistingDB() {
	if _, err := os.Stat(dbPath); err != nil {
		log.Fatalf("database %q does not exist; refusing to create it for an account management command", dbPath)
	}
}

// readPasswordConfirmed 读取两次隐藏回显的密码并要求一致。
func readPasswordConfirmed() string {
	fmt.Print("Password: ")
	pwBytes, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Println()
	if err != nil {
		log.Fatalf("failed to read password: %v", err)
	}
	password := string(pwBytes)
	if password == "" {
		log.Fatalf("password must not be empty")
	}

	fmt.Print("Confirm password: ")
	confirmBytes, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Println()
	if err != nil {
		log.Fatalf("failed to read password: %v", err)
	}
	if string(confirmBytes) != password {
		log.Fatalf("passwords do not match")
	}
	return password
}

// confirmPrompt 从 stdin 读一行，仅当为 y/yes（大小写不敏感）时返回 true。
func confirmPrompt(prompt string) bool {
	fmt.Print(prompt)
	reader := bufio.NewReader(os.Stdin)
	line, err := reader.ReadString('\n')
	if err != nil && line == "" {
		return false // stdin 已关闭（EOF）：视为拒绝
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes"
}

// warnRestartRequired 提示 CLI 改库对运行中进程的影响。
func warnRestartRequired() {
	fmt.Println("NOTE: a running gateway holds sessions in memory; restart it for this change to take effect.")
}

// createAccountFlow 交互式添加账户（-account）。
// 与其它管理命令不同，本命令允许自动建库（首次使用需先有库才能加账户）。
func createAccountFlow() {
	reader := bufio.NewReader(os.Stdin)

	fmt.Print("Username: ")
	username, _ := reader.ReadString('\n')
	username = strings.TrimSpace(username)
	if username == "" {
		log.Fatalf("username must not be empty")
	}

	password := readPasswordConfirmed()

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		log.Fatalf("failed to hash password: %v", err)
	}

	conn, err := openDB(dbPath)
	if err != nil {
		log.Fatalf("failed to open database: %v", err)
	}
	defer conn.Close()

	if _, err := conn.Exec(`INSERT INTO accounts (username, password_hash) VALUES (?,?)`, username, string(hash)); err != nil {
		log.Fatalf("failed to add account: %v", err)
	}

	log.Printf("account %q added", username)
}

// deleteAccountFlow 删除账户及其全部登录会话（-del-account <username>）。
// 会话由 ip_sessions.username 的 ON DELETE CASCADE 连带删除（openDB 已开 PRAGMA foreign_keys=ON）。
func deleteAccountFlow(username string) {
	requireExistingDB()
	if strings.TrimSpace(username) == "" {
		log.Fatalf("-del-account requires a username")
	}

	conn, err := openDB(dbPath)
	if err != nil {
		log.Fatalf("failed to open database: %v", err)
	}
	defer conn.Close()

	var exists int
	err = conn.QueryRow(`SELECT COUNT(*) FROM accounts WHERE username=?`, username).Scan(&exists)
	if err != nil {
		log.Fatalf("failed to query account: %v", err)
	}
	if exists == 0 {
		log.Fatalf("account %q not found", username)
	}

	var sessions int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM ip_sessions WHERE username=?`, username).Scan(&sessions); err != nil {
		log.Fatalf("failed to query sessions: %v", err)
	}

	if !confirmPrompt(fmt.Sprintf(
		"Delete account %q and its %d active session(s)? This cannot be undone. [y/N]: ", username, sessions)) {
		fmt.Println("Aborted.")
		return
	}

	if _, err := conn.Exec(`DELETE FROM accounts WHERE username=?`, username); err != nil {
		log.Fatalf("failed to delete account: %v", err)
	}

	log.Printf("account %q deleted (%d session(s) removed)", username, sessions)
	warnRestartRequired()
}

// listAccountsFlow 以纯文本表格列出账号与已登录 IP 数（-list-accounts）。
func listAccountsFlow() {
	requireExistingDB()

	conn, err := openDB(dbPath)
	if err != nil {
		log.Fatalf("failed to open database: %v", err)
	}
	defer conn.Close()

	rows, err := conn.Query(`
		SELECT a.username,
		       (SELECT COUNT(*) FROM ip_sessions s WHERE s.username = a.username) AS sessions
		FROM accounts a
		ORDER BY a.username`)
	if err != nil {
		log.Fatalf("failed to query accounts: %v", err)
	}
	defer rows.Close()

	type entry struct {
		username string
		sessions int
	}
	var entries []entry
	nameWidth := len("USERNAME")
	for rows.Next() {
		var e entry
		if err := rows.Scan(&e.username, &e.sessions); err != nil {
			log.Fatalf("failed to scan account: %v", err)
		}
		entries = append(entries, e)
		if len(e.username) > nameWidth {
			nameWidth = len(e.username)
		}
	}
	if err := rows.Err(); err != nil {
		log.Fatalf("failed to iterate accounts: %v", err)
	}

	if len(entries) == 0 {
		fmt.Println("No accounts.")
		return
	}

	fmt.Printf("%-*s  %s\n", nameWidth, "USERNAME", "SESSIONS")
	for _, e := range entries {
		fmt.Printf("%-*s  %d\n", nameWidth, e.username, e.sessions)
	}
	fmt.Printf("\n%d account(s)\n", len(entries))
}

// changePasswordFlow 重置指定账户的密码（-passwd <username>），交互式输入两次。
// 按约定不动 ip_sessions：改密码只改密码，已登录 IP 会话保留。
func changePasswordFlow(username string) {
	requireExistingDB()
	if strings.TrimSpace(username) == "" {
		log.Fatalf("-passwd requires a username")
	}

	conn, err := openDB(dbPath)
	if err != nil {
		log.Fatalf("failed to open database: %v", err)
	}
	defer conn.Close()

	var exists int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM accounts WHERE username=?`, username).Scan(&exists); err != nil {
		log.Fatalf("failed to query account: %v", err)
	}
	if exists == 0 {
		log.Fatalf("account %q not found", username)
	}

	password := readPasswordConfirmed()

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		log.Fatalf("failed to hash password: %v", err)
	}

	if _, err := conn.Exec(`UPDATE accounts SET password_hash=? WHERE username=?`, string(hash), username); err != nil {
		log.Fatalf("failed to update password: %v", err)
	}

	log.Printf("password for account %q updated", username)
	warnRestartRequired()
}
