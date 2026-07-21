package main

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
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

	var hash string
	err := db.QueryRow(`SELECT password_hash FROM accounts WHERE username=?`, username).Scan(&hash)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "用户名或密码错误"})
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "用户名或密码错误"})
		return
	}

	ip := clientIP(r)
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
// -account 交互式添加账户
// ============================================================================

func createAccountFlow() {
	reader := bufio.NewReader(os.Stdin)

	fmt.Print("用户名: ")
	username, _ := reader.ReadString('\n')
	username = strings.TrimSpace(username)
	if username == "" {
		log.Fatalf("用户名不能为空")
	}

	fmt.Print("密码: ")
	pwBytes, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Println()
	if err != nil {
		log.Fatalf("读取密码失败: %v", err)
	}
	password := string(pwBytes)
	if password == "" {
		log.Fatalf("密码不能为空")
	}

	fmt.Print("确认密码: ")
	confirmBytes, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Println()
	if err != nil {
		log.Fatalf("读取密码失败: %v", err)
	}
	if string(confirmBytes) != password {
		log.Fatalf("两次输入的密码不一致")
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		log.Fatalf("生成密码哈希失败: %v", err)
	}

	conn, err := openDB(dbPath)
	if err != nil {
		log.Fatalf("打开数据库失败: %v", err)
	}
	defer conn.Close()

	if _, err := conn.Exec(`INSERT INTO accounts (username, password_hash) VALUES (?,?)`, username, string(hash)); err != nil {
		log.Fatalf("添加账户失败: %v", err)
	}
	log.Printf("账户 %q 添加成功", username)
}
