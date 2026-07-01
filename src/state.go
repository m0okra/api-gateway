package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"time"

	_ "modernc.org/sqlite"
)

// ============================================================================
// 可用性状态管理：从 SQLite 读取，dirty flag + 5min 检查保存
// 配置与运行时状态统一存储在 gateway.db，runtime 与持久化数据模型一致。
// ============================================================================

const dbSchema = `
CREATE TABLE IF NOT EXISTS aliases (
  name               TEXT PRIMARY KEY,
  real_token         TEXT NOT NULL,
  target_base        TEXT NOT NULL,
  avail_type         TEXT,
  avail_limit        INTEGER,
  avail_refresh_cron TEXT,
  avail_provider     TEXT,
  exhausted          INTEGER NOT NULL DEFAULT 0,
  count              INTEGER NOT NULL DEFAULT 0,
  balance            REAL    NOT NULL DEFAULT 0,
  recovery_cron      TEXT,
  recovery_at        DATETIME,
  last_recovery      DATETIME,
  last_checked       DATETIME
);
CREATE TABLE IF NOT EXISTS alias_tiers (
  alias_name   TEXT NOT NULL,
  name         TEXT NOT NULL,
  used_pct     REAL    NOT NULL DEFAULT 0,
  reset_in_sec INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY(alias_name, name),
  FOREIGN KEY(alias_name) REFERENCES aliases(name) ON DELETE CASCADE
);
CREATE TABLE IF NOT EXISTS alias_extra (
  alias_name TEXT NOT NULL,
  key        TEXT NOT NULL,
  value      TEXT NOT NULL,
  PRIMARY KEY(alias_name, key),
  FOREIGN KEY(alias_name) REFERENCES aliases(name) ON DELETE CASCADE
);
CREATE TABLE IF NOT EXISTS fake_tokens (
  fake_token TEXT NOT NULL,
  alias_name TEXT NOT NULL,
  priority   INTEGER NOT NULL,
  PRIMARY KEY(fake_token, alias_name),
  FOREIGN KEY(alias_name) REFERENCES aliases(name) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_fake_tokens_order ON fake_tokens(fake_token, priority);
`

// openDB 打开 SQLite 连接，设置 PRAGMA（FK/WAL/busy_timeout）并确保 schema 存在。
// 调用者负责在使用完毕后 Close 返回的连接（loadFromDB 将其赋给全局 db 由 main 关闭；
// importFromJSON 用局部连接在函数末尾关闭）。
func openDB(path string) (*sql.DB, error) {
	conn, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	// 启用 FK + WAL，提升并发与崩溃安全
	if _, err := conn.Exec(`PRAGMA foreign_keys=ON; PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000;`); err != nil {
		conn.Close()
		return nil, fmt.Errorf("set pragmas: %w", err)
	}
	if _, err := conn.Exec(dbSchema); err != nil {
		conn.Close()
		return nil, fmt.Errorf("ensure schema: %w", err)
	}
	return conn, nil
}

// loadFromDB 打开 SQLite 并将所有配置+状态读入内存运行时结构。
// 空库 → 空 map，网关正常启动（请求回 Invalid token，等用户编辑 DB 加配置）。
func loadFromDB() error {
	mu.Lock()
	defer mu.Unlock()

	conn, err := openDB(dbPath)
	if err != nil {
		return err
	}
	db = conn

	tokenMap = &TokenMapConfig{
		FakeTokens: make(map[string][]string),
		Aliases:    make(map[string]AliasConfig),
	}
	stateMap = make(map[string]*AvailabilityState)

	// 1. aliases 行 → 同时填充 tokenMap.Aliases（配置列）与 stateMap（状态列）
	aliasRows, err := conn.Query(`SELECT name, real_token, target_base,
		avail_type, avail_limit, avail_refresh_cron, avail_provider,
		exhausted, count, balance, recovery_cron, recovery_at, last_recovery, last_checked
		FROM aliases`)
	if err != nil {
		return fmt.Errorf("query aliases: %w", err)
	}
	defer aliasRows.Close()

	type aliasMeta struct {
		hasAvail bool
		avail    *AvailabilityConfig
	}
	metas := make(map[string]aliasMeta)

	for aliasRows.Next() {
		var (
			name, realToken, targetBase      string
			availType                        sql.NullString
			availLimit                       sql.NullInt64
			availRefreshCron, availProvider  sql.NullString
			exhausted                        int
			count                            int
			balance                          float64
			recoveryCron                     sql.NullString
			recoveryAt, lastRecovery, lastChecked sql.NullString
		)
		if err := aliasRows.Scan(&name, &realToken, &targetBase,
			&availType, &availLimit, &availRefreshCron, &availProvider,
			&exhausted, &count, &balance, &recoveryCron, &recoveryAt, &lastRecovery, &lastChecked); err != nil {
			return fmt.Errorf("scan alias: %w", err)
		}

		alias := AliasConfig{RealToken: realToken, TargetBase: targetBase, Extra: map[string]string{}}
		var avail *AvailabilityConfig
		if availType.Valid {
			avail = &AvailabilityConfig{Type: availType.String}
			if availType.String == availCount {
				avail.Limit = int(availLimit.Int64)
			}
			if availRefreshCron.Valid {
				avail.RefreshCron = availRefreshCron.String
			}
			if availProvider.Valid {
				avail.Provider = availProvider.String
			}
			alias.Availability = avail
		}
		tokenMap.Aliases[name] = alias
		metas[name] = aliasMeta{hasAvail: avail != nil, avail: avail}

		st := &AvailabilityState{
			Exhausted:    exhausted != 0,
			Count:        count,
			Balance:      balance,
			RecoveryCron: recoveryCron.String,
		}
		if recoveryAt.Valid {
			if t, err := time.Parse(time.RFC3339Nano, recoveryAt.String); err == nil {
				st.RecoveryAt = t
			}
		}
		if lastRecovery.Valid {
			if t, err := time.Parse(time.RFC3339Nano, lastRecovery.String); err == nil {
				st.LastRecovery = t
			}
		}
		if lastChecked.Valid {
			if t, err := time.Parse(time.RFC3339Nano, lastChecked.String); err == nil {
				st.LastChecked = t
			}
		}
		stateMap[name] = st
	}
	if err := aliasRows.Err(); err != nil {
		return fmt.Errorf("iterate aliases: %w", err)
	}

	// 2. alias_tiers → AvailabilityConfig.Tiers（配置）+ AvailabilityState.Tiers（运行时状态）
	tierRows, err := conn.Query(`SELECT alias_name, name, used_pct, reset_in_sec FROM alias_tiers`)
	if err != nil {
		return fmt.Errorf("query alias_tiers: %w", err)
	}
	defer tierRows.Close()
	stTiersByAlias := make(map[string][]TierState)
	for tierRows.Next() {
		var aliasName, tierName string
		var usedPct float64
		var resetInSec int
		if err := tierRows.Scan(&aliasName, &tierName, &usedPct, &resetInSec); err != nil {
			return fmt.Errorf("scan tier: %w", err)
		}
		if meta, ok := metas[aliasName]; ok && meta.avail != nil {
			meta.avail.Tiers = append(meta.avail.Tiers, TierConfig{Name: tierName})
			// 注意：metas 存的是指针副本，append 写到的是同一个 *AvailabilityConfig
		}
		stTiersByAlias[aliasName] = append(stTiersByAlias[aliasName], TierState{
			Name: tierName, UsedPct: usedPct, ResetInSec: resetInSec,
		})
	}
	if err := tierRows.Err(); err != nil {
		return fmt.Errorf("iterate tiers: %w", err)
	}
	for aliasName, tiers := range stTiersByAlias {
		if st, ok := stateMap[aliasName]; ok {
			st.Tiers = tiers
		}
	}

	// 3. alias_extra → AliasConfig.Extra
	extraRows, err := conn.Query(`SELECT alias_name, key, value FROM alias_extra`)
	if err != nil {
		return fmt.Errorf("query alias_extra: %w", err)
	}
	defer extraRows.Close()
	for extraRows.Next() {
		var aliasName, k, v string
		if err := extraRows.Scan(&aliasName, &k, &v); err != nil {
			return fmt.Errorf("scan extra: %w", err)
		}
		if alias, ok := tokenMap.Aliases[aliasName]; ok {
			alias.Extra[k] = v
			tokenMap.Aliases[aliasName] = alias // map 值类型，需写回
		}
	}
	if err := extraRows.Err(); err != nil {
		return fmt.Errorf("iterate extras: %w", err)
	}

	// 4. fake_tokens ORDER BY priority → tokenMap.FakeTokens 切片
	ftRows, err := conn.Query(`SELECT fake_token, alias_name, priority FROM fake_tokens ORDER BY fake_token, priority`)
	if err != nil {
		return fmt.Errorf("query fake_tokens: %w", err)
	}
	defer ftRows.Close()
	ftMap := make(map[string][]string)
	for ftRows.Next() {
		var fake, aliasName string
		var priority int
		if err := ftRows.Scan(&fake, &aliasName, &priority); err != nil {
			return fmt.Errorf("scan fake_token: %w", err)
		}
		ftMap[fake] = append(ftMap[fake], aliasName)
	}
	if err := ftRows.Err(); err != nil {
		return fmt.Errorf("iterate fake_tokens: %w", err)
	}
	for fake, queue := range ftMap {
		tokenMap.FakeTokens[fake] = queue
	}

	// 启动时一致性保护：清理队列中 Aliases 不存在的 alias
	cleanFakeTokenQueues()

	// 为 tokenMap 中存在但 stateMap 缺失的 alias 补默认 state（DB 为空后手工新增 alias 的情况）
	for name := range tokenMap.Aliases {
		if _, ok := stateMap[name]; !ok {
			stateMap[name] = initStateFor(name)
		}
	}
	// 移除 tokenMap 中已不存在的 alias 的残留 state（DB 手工删除 alias 但 state 还在的兜底）
	for name := range stateMap {
		if _, ok := tokenMap.Aliases[name]; !ok {
			delete(stateMap, name)
		}
	}
	stateDirty = false
	reconcileStateWithConfig()
	log.Printf("State loaded from DB (%d aliases)", len(stateMap))
	return nil
}

// cleanFakeTokenQueues 移除 FakeTokens 队列中 Aliases 缺失的 alias 名。
// 调用者需持有 mu 锁。
func cleanFakeTokenQueues() {
	for fake, queue := range tokenMap.FakeTokens {
		if len(queue) == 0 {
			continue
		}
		cleaned := make([]string, 0, len(queue))
		for _, a := range queue {
			if _, ok := tokenMap.Aliases[a]; ok {
				cleaned = append(cleaned, a)
			} else {
				log.Printf("[state] fakeToken=%s queue contains missing alias=%s -> removed from queue",
					maskFakeToken(fake), a)
			}
		}
		if len(cleaned) != len(queue) {
			tokenMap.FakeTokens[fake] = cleaned
		}
	}
}

func initStateFor(name string) *AvailabilityState {
	return &AvailabilityState{Exhausted: false, Count: 0}
}

// reconcileStateWithConfig 启动时确认：处理 availability type 变更与 count limit 下调。
// 调用者需持有 mu 锁。
//   - type 变更：清理无关字段（不影响功能，仅保持 state 干净）
//   - count limit 下调：若 count >= 新 limit，立即标记 exhaust（避免延后一个请求才生效）
func reconcileStateWithConfig() {
	for name, alias := range tokenMap.Aliases {
		st, ok := stateMap[name]
		if !ok || st == nil {
			continue
		}
		cfg := alias.Availability
		if cfg == nil {
			continue
		}
		// 1) type 变更：清理无关字段，并清理另一调度类型的残留依据
		switch cfg.Type {
		case availCount:
			st.Balance = 0
			st.Tiers = nil
			st.RecoveryAt = time.Time{} // count 不使用 RecoveryAt
			// count 型 RecoveryCron 始终对齐 RefreshCron
			if cfg.RefreshCron != "" {
				st.RecoveryCron = cfg.RefreshCron
			}
		case availBalance:
			st.Count = 0
			st.Tiers = nil
			st.RecoveryCron = "" // 非 count 不使用 RecoveryCron
		case availUsage:
			st.Count = 0
			st.Balance = 0
			st.RecoveryCron = ""
		case availFallback:
			st.Count = 0
			st.Balance = 0
			st.Tiers = nil
			st.RecoveryCron = ""
		}
		// 2) count limit 下调：count >= limit 立即标记 exhaust
if cfg.Type == availCount && cfg.Limit > 0 && st.Count >= cfg.Limit && !st.Exhausted {
		st.Exhausted = true
		markDirty()
		log.Printf("[state] alias=%s count=%d >= limit=%d -> exhaust at startup", name, st.Count, cfg.Limit)
		}
	}
}

// formatTime 将 time.Time 转为 sql.NullString；零值→NULL（避免 1970 误判）
func formatTime(t time.Time) sql.NullString {
	if t.IsZero() {
		return sql.NullString{}
	}
	return sql.NullString{String: t.UTC().Format(time.RFC3339Nano), Valid: true}
}

// saveState 将内存中所有 alias 状态全量写回 SQLite（单事务）。
// 配置字段不写回（配置由用户直接编辑 DB；运行时轮转顺序不持久化）。
//
// 并发优化：在写锁内构建 state 深拷贝快照后即释放锁，事务的 SQLite I/O 在锁外执行，
// 避免写库（可能数百 ms）阻塞所有请求。快照后记录 stateGen 代际计数，提交后再取锁：
// 若代际未变 → 无新写入，安全清除 stateDirty；若代际已增 → 快照后又有变更，保留 stateDirty
// 由下一个保存周期补写，保证不丢数据。
func saveState() error {
	// 1. 持锁快照
	type aliasSnap struct {
		name          string
		exhausted     bool
		count         int
		balance       float64
		recoveryCron  string
		recoveryAt    time.Time
		lastRecovery  time.Time
		lastChecked   time.Time
		tiers         []TierState
	}
	var snap []aliasSnap
	mu.Lock()
	for name, st := range stateMap {
		if st == nil {
			continue
		}
		s := aliasSnap{
			name:         name,
			exhausted:    st.Exhausted,
			count:        st.Count,
			balance:      st.Balance,
			recoveryCron: st.RecoveryCron,
			recoveryAt:   st.RecoveryAt,
			lastRecovery: st.LastRecovery,
			lastChecked:  st.LastChecked,
		}
		if len(st.Tiers) > 0 {
			s.tiers = make([]TierState, len(st.Tiers))
			copy(s.tiers, st.Tiers)
		}
		snap = append(snap, s)
	}
	snapGen := stateGen.Load()
	mu.Unlock()

	// 2. 锁外执行事务 I/O
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	aliasUpd, err := tx.Prepare(`UPDATE aliases SET
		exhausted=?, count=?, balance=?, recovery_cron=?, recovery_at=?, last_recovery=?, last_checked=?
		WHERE name=?`)
	if err != nil {
		return fmt.Errorf("prepare alias update: %w", err)
	}
	defer aliasUpd.Close()

	tierDel, err := tx.Prepare(`DELETE FROM alias_tiers WHERE alias_name=?`)
	if err != nil {
		return fmt.Errorf("prepare tier delete: %w", err)
	}
	defer tierDel.Close()

	tierIns, err := tx.Prepare(`INSERT INTO alias_tiers (alias_name, name, used_pct, reset_in_sec) VALUES (?,?,?,?)`)
	if err != nil {
		return fmt.Errorf("prepare tier insert: %w", err)
	}
	defer tierIns.Close()

	for _, s := range snap {
		exhausted := 0
		if s.exhausted {
			exhausted = 1
		}
		var recoveryCron sql.NullString
		if s.recoveryCron != "" {
			recoveryCron = sql.NullString{String: s.recoveryCron, Valid: true}
		}
		if _, err := aliasUpd.Exec(exhausted, s.count, s.balance, recoveryCron,
			formatTime(s.recoveryAt), formatTime(s.lastRecovery), formatTime(s.lastChecked),
			s.name); err != nil {
			return fmt.Errorf("update alias %s: %w", s.name, err)
		}
		// tiers 全量重写（usage 型；非 usage 型 tiers 为 nil → 仅删除）
		if _, err := tierDel.Exec(s.name); err != nil {
			return fmt.Errorf("delete tiers for %s: %w", s.name, err)
		}
		for _, ts := range s.tiers {
			if _, err := tierIns.Exec(s.name, ts.Name, ts.UsedPct, ts.ResetInSec); err != nil {
				return fmt.Errorf("insert tier %s.%s: %w", s.name, ts.Name, err)
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}

	// 3. 提交后再取锁：仅当代际未变才清 dirty
	mu.Lock()
	if stateGen.Load() == snapGen {
		stateDirty = false
	}
	mu.Unlock()
	return nil
}

// markDirty 在持有 mu 写锁时调用：置脏并自增代际计数。
// saveState 据此判断快照→提交期间是否产生新变更，避免误清 stateDirty 导致丢失写入。
func markDirty() {
	stateDirty = true
	stateGen.Add(1)
}

// ============================================================================
// 导入/导出（-e/-i）：单个 JSON 文件 <-> SQLite 全量同步
//   - exportToJSON: 复用 loadFromDB 载入内存 → marshal DBDump → 原子写
//   - importFromJSON: 读 JSON → 单事务 DELETE 4 表 + INSERT 全量覆盖
// ============================================================================

// exportToJSON 将 -db 指定的库全量导出为单个 JSON 文件。
// 复用 loadFromDB 载入内存（含 reconcile/cleanFakeTokenQueues 规范化），导出的是规范视图。
// 完成后关闭本次打开的 DB 连接（不保留全局 db，因为导出后进程即退出）。
func exportToJSON(outPath string) error {
	if err := loadFromDB(); err != nil {
		return err
	}
	// loadFromDB 把连接赋给了全局 db；导出场景下立即关闭，避免泄漏
	if db != nil {
		db.Close()
		db = nil
	}

	mu.Lock()
	dump := DBDump{TokenMap: tokenMap, State: stateMap}
	mu.Unlock()

	data, err := json.MarshalIndent(dump, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal dump: %w", err)
	}
	// 原子写：先写 tmp 再 rename，避免导出中途崩溃产生半截文件
	tmp := outPath + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return fmt.Errorf("write tmp: %w", err)
	}
	if err := os.Rename(tmp, outPath); err != nil {
		return fmt.Errorf("rename: %w", err)
	}
	log.Printf("Exported %d aliases / %d fakeTokens -> %s",
		len(dump.TokenMap.Aliases), len(dump.TokenMap.FakeTokens), outPath)
	return nil
}

// importFromJSON 将单个 JSON 文件全量导入 -db 指定的库。
// 单事务：DELETE 4 表（先子后父）→ INSERT aliases（配置+状态列）→ alias_tiers → alias_extra → fake_tokens。
// 任一步失败 → Rollback，DB 保持原状。完成后关闭连接（导入后进程即退出）。
func importFromJSON(inPath string) error {
	data, err := os.ReadFile(inPath)
	if err != nil {
		return fmt.Errorf("read json: %w", err)
	}
	var dump DBDump
	if err := json.Unmarshal(data, &dump); err != nil {
		return fmt.Errorf("parse json: %w", err)
	}
	// nil → 空 map，导入空库合法
	if dump.TokenMap == nil {
		dump.TokenMap = &TokenMapConfig{
			FakeTokens: map[string][]string{},
			Aliases:    map[string]AliasConfig{},
		}
	}
	if dump.TokenMap.FakeTokens == nil {
		dump.TokenMap.FakeTokens = map[string][]string{}
	}
	if dump.TokenMap.Aliases == nil {
		dump.TokenMap.Aliases = map[string]AliasConfig{}
	}
	if dump.State == nil {
		dump.State = map[string]*AvailabilityState{}
	}

	conn, err := openDB(dbPath)
	if err != nil {
		return err
	}
	defer conn.Close()

	tx, err := conn.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	// 1. 全量清空（先子表后父表，虽然开了 FK 但 DELETE 顺序仍稳妥）
	if _, err := tx.Exec(`DELETE FROM alias_tiers`); err != nil {
		return fmt.Errorf("clear alias_tiers: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM alias_extra`); err != nil {
		return fmt.Errorf("clear alias_extra: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM fake_tokens`); err != nil {
		return fmt.Errorf("clear fake_tokens: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM aliases`); err != nil {
		return fmt.Errorf("clear aliases: %w", err)
	}

	// 2. INSERT aliases（配置列 + 状态列全写）
	aliasStmt, err := tx.Prepare(`INSERT INTO aliases
		(name, real_token, target_base, avail_type, avail_limit, avail_refresh_cron, avail_provider,
		 exhausted, count, balance, recovery_cron, recovery_at, last_recovery, last_checked)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return fmt.Errorf("prepare alias insert: %w", err)
	}
	defer aliasStmt.Close()

	tierStmt, err := tx.Prepare(`INSERT INTO alias_tiers (alias_name, name, used_pct, reset_in_sec) VALUES (?,?,?,?)`)
	if err != nil {
		return fmt.Errorf("prepare tier insert: %w", err)
	}
	defer tierStmt.Close()

	extraStmt, err := tx.Prepare(`INSERT INTO alias_extra (alias_name, key, value) VALUES (?,?,?)`)
	if err != nil {
		return fmt.Errorf("prepare extra insert: %w", err)
	}
	defer extraStmt.Close()

	ftStmt, err := tx.Prepare(`INSERT OR IGNORE INTO fake_tokens (fake_token, alias_name, priority) VALUES (?,?,?)`)
	if err != nil {
		return fmt.Errorf("prepare fake_tokens insert: %w", err)
	}
	defer ftStmt.Close()

	aliasCount := 0
	for name, alias := range dump.TokenMap.Aliases {
		var availType sql.NullString
		var availLimit sql.NullInt64
		var availRefreshCron, availProvider sql.NullString
		if alias.Availability != nil {
			availType = sql.NullString{String: alias.Availability.Type, Valid: true}
			if alias.Availability.Type == availCount {
				availLimit = sql.NullInt64{Int64: int64(alias.Availability.Limit), Valid: true}
			}
			if alias.Availability.RefreshCron != "" {
				availRefreshCron = sql.NullString{String: alias.Availability.RefreshCron, Valid: true}
			}
			if alias.Availability.Provider != "" {
				availProvider = sql.NullString{String: alias.Availability.Provider, Valid: true}
			}
		}
		st, ok := dump.State[name]
		if !ok {
			st = &AvailabilityState{}
		}
		exhausted := 0
		if st.Exhausted {
			exhausted = 1
		}
		var recoveryCron sql.NullString
		if st.RecoveryCron != "" {
			recoveryCron = sql.NullString{String: st.RecoveryCron, Valid: true}
		}
		if _, err := aliasStmt.Exec(name, alias.RealToken, alias.TargetBase,
			availType, availLimit, availRefreshCron, availProvider,
			exhausted, st.Count, st.Balance, recoveryCron,
			formatTime(st.RecoveryAt), formatTime(st.LastRecovery), formatTime(st.LastChecked)); err != nil {
			return fmt.Errorf("insert alias %q: %w", name, err)
		}
		aliasCount++

		// 3. alias_tiers：配置 TierConfig + 对应 state.Tiers 运行时状态合并
		if alias.Availability != nil && len(alias.Availability.Tiers) > 0 {
			stByName := make(map[string]TierState, len(st.Tiers))
			for _, ts := range st.Tiers {
				stByName[ts.Name] = ts
			}
			for _, tc := range alias.Availability.Tiers {
				ts := stByName[tc.Name] // 缺失则零值
				if _, err := tierStmt.Exec(name, tc.Name, ts.UsedPct, ts.ResetInSec); err != nil {
					return fmt.Errorf("insert tier %s.%s: %w", name, tc.Name, err)
				}
			}
		}

		// 4. alias_extra
		for k, v := range alias.Extra {
			if _, err := extraStmt.Exec(name, k, v); err != nil {
				return fmt.Errorf("insert extra %s.%s: %w", name, k, err)
			}
		}
	}

	// 5. fake_tokens（priority = 切片下标；重复 alias 用 OR IGNORE 跳过，引用不存在的 alias 跳过避免 FK 违约）
	ftCount := 0
	dupSkipped := 0
	missingSkipped := 0
	for fake, queue := range dump.TokenMap.FakeTokens {
		seen := map[string]bool{}
		for i, aliasName := range queue {
			if _, ok := dump.TokenMap.Aliases[aliasName]; !ok {
				missingSkipped++
				log.Printf("[import] WARN fakeToken=%s queue references missing alias=%s -> skipped",
					maskFakeToken(fake), aliasName)
				continue
			}
			if seen[aliasName] {
				dupSkipped++
				log.Printf("[import] WARN fakeToken=%s has duplicate alias=%s in queue -> skipped (kept first)",
					maskFakeToken(fake), aliasName)
				continue
			}
			seen[aliasName] = true
			if _, err := ftStmt.Exec(fake, aliasName, i); err != nil {
				return fmt.Errorf("insert fake_token %s.%s: %w", fake, aliasName, err)
			}
			ftCount++
		}
	}

	// 6. 孤儿 state 警告（state 中有但 Aliases 中无）
	orphanWarned := 0
	for name := range dump.State {
		if _, ok := dump.TokenMap.Aliases[name]; !ok {
			log.Printf("[import] WARN state has orphan alias=%s (not in tokenMap.aliases) -> ignored", name)
			orphanWarned++
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	log.Printf("Imported %d aliases / %d fake_tokens -> %s (skipped %d dup, %d missing, %d orphan warned)",
		aliasCount, ftCount, dbPath, dupSkipped, missingSkipped, orphanWarned)
	return nil
}
