package main

import (
	"container/list"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
)

// ============================================================================
// 登录爆破防护：per-IP + per-账号 双维度内存限流
//
// 设计要点：
//   - per-IP：连续失败达 maxFailsIp 后锁定 lockIpSec；每一次重新触发翻倍
//     （5m→10m→20m→40m…），封顶 lockCapSec。该 IP 登录成功即清空全部记录（含翻倍等级）。
//   - per-账号：连续失败达 maxFailsUser 后固定锁定 lockUserSec，不翻倍。
//     只有该账号自己登录成功才清空（别人成功登录不影响）。
//   - 「连续失败」的时间窗口 = 该维度的基础锁定时长；窗口外的失败不累积。
//   - maxFails* 为 0 表示关闭该维度：不记录、永不锁定。
//   - 纯内存，重启清零；每维度 LRU 上限 loginLimiterCap，防止伪造源 IP 或
//     随机用户名的扫描把内存撑爆。
//   - 时间由 now 字段注入，测试可替换为假时钟。
// ============================================================================

// LoginPolicy 是登录限流策略，实际生效值持久化在 login_policy 单行表，
// 可经 -e / -i 导出导入。编译期默认值见 constants.go。
type LoginPolicy struct {
	// MaxFailsIp per-IP 维度在窗口内的失败次数阈值；0 = 关闭该维度
	MaxFailsIp int `json:"maxFailsIp"`
	// LockIpSec per-IP 基础锁定时长（秒），重复触发按倍数增长
	LockIpSec int `json:"lockIpSec"`
	// MaxFailsUser per-账号维度在窗口内的失败次数阈值；0 = 关闭该维度
	MaxFailsUser int `json:"maxFailsUser"`
	// LockUserSec per-账号锁定时长（秒），不增长
	LockUserSec int `json:"lockUserSec"`
	// LockCapSec per-IP 翻倍封顶值（秒）
	LockCapSec int `json:"lockCapSec"`
}

// defaultLoginPolicy 返回编译期默认策略。
func defaultLoginPolicy() LoginPolicy {
	return LoginPolicy{
		MaxFailsIp:   defaultLoginMaxFailsIP,
		LockIpSec:    defaultLoginLockIPSec,
		MaxFailsUser: defaultLoginMaxFailsUser,
		LockUserSec:  defaultLoginLockUserSec,
		LockCapSec:   defaultLoginLockCapSec,
	}
}

// Validate 校验限流策略，收集全部错误一次性返回。
//   - maxFails* 允许为 0（= 关闭该维度），负数非法
//   - lock*Sec 必须 ≥ 1（0 会让锁定立即过期，等于限流失效）
//   - lockCapSec 必须 ≥ max(lockIpSec, lockUserSec)，否则翻倍逻辑退化成常量
func (p LoginPolicy) Validate() error {
	var errs []string
	if p.MaxFailsIp < 0 {
		errs = append(errs, fmt.Sprintf("maxFailsIp must be >= 0 (0 disables the per-IP dimension), got %d", p.MaxFailsIp))
	}
	if p.MaxFailsUser < 0 {
		errs = append(errs, fmt.Sprintf("maxFailsUser must be >= 0 (0 disables the per-account dimension), got %d", p.MaxFailsUser))
	}
	if p.LockIpSec < 1 {
		errs = append(errs, fmt.Sprintf("lockIpSec must be >= 1 second, got %d", p.LockIpSec))
	}
	if p.LockUserSec < 1 {
		errs = append(errs, fmt.Sprintf("lockUserSec must be >= 1 second, got %d", p.LockUserSec))
	}
	if p.LockCapSec < p.LockIpSec || p.LockCapSec < p.LockUserSec {
		errs = append(errs, fmt.Sprintf(
			"lockCapSec (%d) must be >= max(lockIpSec=%d, lockUserSec=%d)", p.LockCapSec, p.LockIpSec, p.LockUserSec))
	}
	if len(errs) > 0 {
		return fmt.Errorf("login policy validation failed:\n  - %s", strings.Join(errs, "\n  - "))
	}
	return nil
}

// ============================================================================
// 限流内存表：单条记录 + 带容量上限的 LRU
// 不加锁——所有访问都在 loginRateLimiter.mu 之内。
// ============================================================================

type attemptEntry struct {
	key         string
	fails       int       // 当前窗口内累积的失败次数
	lastFail    time.Time // 最近一次失败时间
	lockedUntil time.Time // 锁定到期时间；零值 = 未锁定
	lockLevel   int       // per-IP 翻倍等级（0 = 下次用基础锁定时长）
}

type attemptLRU struct {
	cap   int
	items map[string]*list.Element
	order *list.List // front = 最近访问
}

func newAttemptLRU(capacity int) *attemptLRU {
	return &attemptLRU{
		cap:   capacity,
		items: make(map[string]*list.Element, capacity),
		order: list.New(),
	}
}

// get 取记录并提升到 MRU；未命中返回 nil。
func (t *attemptLRU) get(key string) *attemptEntry {
	el, ok := t.items[key]
	if !ok {
		return nil
	}
	t.order.MoveToFront(el)
	return el.Value.(*attemptEntry)
}

// put 写入记录；新条目超容量则淘汰 LRU 尾部。
func (t *attemptLRU) put(e *attemptEntry) {
	if el, ok := t.items[e.key]; ok {
		el.Value = e
		t.order.MoveToFront(el)
		return
	}
	t.items[e.key] = t.order.PushFront(e)
	if t.order.Len() > t.cap {
		if victim := t.order.Back(); victim != nil {
			t.order.Remove(victim)
			delete(t.items, victim.Value.(*attemptEntry).key)
		}
	}
}

func (t *attemptLRU) remove(key string) {
	if el, ok := t.items[key]; ok {
		t.order.Remove(el)
		delete(t.items, key)
	}
}

func (t *attemptLRU) len() int { return t.order.Len() }

// sweep 删除「未锁定且空闲超过 idle」的条目，返回删除数量。
// 锁定中的条目不删——否则翻倍等级会随 GC 意外丢失。
func (t *attemptLRU) sweep(now time.Time, idle time.Duration) int {
	removed := 0
	for el := t.order.Back(); el != nil; {
		prev := el.Prev()
		e := el.Value.(*attemptEntry)
		unlocked := e.lockedUntil.IsZero() || !now.Before(e.lockedUntil)
		if unlocked && !e.lastFail.IsZero() && now.Sub(e.lastFail) > idle {
			t.order.Remove(el)
			delete(t.items, e.key)
			removed++
		}
		el = prev
	}
	return removed
}

// ============================================================================
// 限流器
// ============================================================================

type loginRateLimiter struct {
	mu     sync.Mutex
	policy LoginPolicy
	ips    *attemptLRU
	users  *attemptLRU
	now    func() time.Time
}

func newLoginRateLimiter(p LoginPolicy) *loginRateLimiter {
	return &loginRateLimiter{
		policy: p,
		ips:    newAttemptLRU(loginLimiterCap),
		users:  newAttemptLRU(loginLimiterCap),
		now:    time.Now,
	}
}

// setPolicy 替换策略（loadFromDB / -i 导入后调用）。
// 策略变更不清空既有记录：降阈值后旧记录会自然在下次失败时按新阈值判定。
func (l *loginRateLimiter) setPolicy(p LoginPolicy) {
	l.mu.Lock()
	l.policy = p
	l.mu.Unlock()
}

// policySnapshot 返回当前策略的副本。
func (l *loginRateLimiter) policySnapshot() LoginPolicy {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.policy
}

// ipLockRemaining 返回该 IP 还需等待多久解锁；0 表示未锁定。
func (l *loginRateLimiter) ipLockRemaining(ip string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.lockRemaining(l.ips, ip)
}

// userLockRemaining 返回该账号还需等待多久解锁；0 表示未锁定。
func (l *loginRateLimiter) userLockRemaining(username string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.lockRemaining(l.users, username)
}

// lockRemaining 内部实现：顺带把已到期的锁定清掉（懒清理）。
func (l *loginRateLimiter) lockRemaining(t *attemptLRU, key string) time.Duration {
	if key == "" {
		return 0
	}
	e := t.get(key)
	if e == nil || e.lockedUntil.IsZero() {
		return 0
	}
	now := l.now()
	if !now.Before(e.lockedUntil) {
		e.lockedUntil = time.Time{}
		return 0
	}
	return e.lockedUntil.Sub(now)
}

// recordFailure 记录一次登录失败，必要时双维度同时施加锁定。
// 调用方应已确认两个维度当前均未锁定（锁定期间不会走到密码校验）。
func (l *loginRateLimiter) recordFailure(ip, username string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	p := l.policy

	// per-IP：窗口内累积失败 → 达阈值锁定 → 翻倍等级 +1
	if p.MaxFailsIp > 0 && ip != "" {
		window := time.Duration(p.LockIpSec) * time.Second
		capWindow := time.Duration(p.LockCapSec) * time.Second
		e := l.ips.get(ip)
		if e == nil {
			e = &attemptEntry{key: ip}
			l.ips.put(e)
		} else if !e.lastFail.IsZero() {
			idle := now.Sub(e.lastFail)
			// 空闲超过封顶窗口 → 翻倍等级也归零，避免长期累积成永久重罚
			if idle > capWindow {
				e.lockLevel = 0
			}
			// 空闲超过基础窗口 → 失败计数归零（「连续」的定义）
			if idle > window {
				e.fails = 0
			}
		}
		if e.lockedUntil.IsZero() || !now.Before(e.lockedUntil) {
			e.fails++
			e.lastFail = now
			if e.fails >= p.MaxFailsIp {
				e.lockedUntil = now.Add(ipLockDuration(p, e.lockLevel))
				e.lockLevel++
				e.fails = 0
			}
		}
	}

	// per-账号：固定时长锁定，不翻倍
	if p.MaxFailsUser > 0 && username != "" {
		window := time.Duration(p.LockUserSec) * time.Second
		e := l.users.get(username)
		if e == nil {
			e = &attemptEntry{key: username}
			l.users.put(e)
		} else if !e.lastFail.IsZero() && now.Sub(e.lastFail) > window {
			e.fails = 0
		}
		if e.lockedUntil.IsZero() || !now.Before(e.lockedUntil) {
			e.fails++
			e.lastFail = now
			if e.fails >= p.MaxFailsUser {
				e.lockedUntil = now.Add(window)
				e.fails = 0
			}
		}
	}
}

// recordSuccess 登录成功后清空两个维度的记录（含 per-IP 翻倍等级）。
func (l *loginRateLimiter) recordSuccess(ip, username string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if ip != "" {
		l.ips.remove(ip)
	}
	if username != "" {
		l.users.remove(username)
	}
}

// gc 清理过期条目。由 runScheduler 的 10 分钟 ticker 驱动。
// 正确性由 lockRemaining / recordFailure 的懒清理保证，这里只负责抑制长期滞胀。
func (l *loginRateLimiter) gc() {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	p := l.policy
	// per-IP 表按封顶窗口清理：翻倍等级需要保留更久
	ipIdle := time.Duration(p.LockCapSec) * time.Second
	userIdle := time.Duration(p.LockUserSec) * time.Second
	removed := l.ips.sweep(now, ipIdle) + l.users.sweep(now, userIdle)
	if removed > 0 {
		log.Printf("[auth] login limiter swept %d expired entr%s", removed, plural(removed))
	}
}

// sizes 返回两个维度的条目数（测试与排障用）。
func (l *loginRateLimiter) sizes() (ips, users int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.ips.len(), l.users.len()
}

// ipLockDuration 计算第 level 级（0 起）的 per-IP 锁定时长：
// lockIpSec << level，封顶 lockCapSec。
func ipLockDuration(p LoginPolicy, level int) time.Duration {
	capDur := time.Duration(p.LockCapSec) * time.Second
	d := time.Duration(p.LockIpSec) * time.Second
	for i := 0; i < level && d < capDur; i++ {
		d *= 2
	}
	if d > capDur {
		d = capDur
	}
	return d
}

// plural 返回英文复数后缀（仅用于日志）。
func plural(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}

// loginLimiter 进程级限流器，策略由 loadFromDB 从 login_policy 表载入。
var loginLimiter = newLoginRateLimiter(defaultLoginPolicy())
