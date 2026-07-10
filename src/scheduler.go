package main

import (
	"context"
	"log"
	"time"
)

// ============================================================================
// 调度goroutine：exhaust恢复 + 状态保存检查
//   - 每1s：检查每个upstream的恢复调度依据
//       count型 → RecoveryCron 周期匹配
//       usage/balance/exhaust型 → now >= RecoveryAt 时间点触发
//     且距上次实际触发 > recoveryMinGap，触发恢复
//   - 每5min：检查 dirty flag，为true则保存
//
// shutdownCtx 来自 main 的优雅停机 context，final save 用它以支持超时取消；
// 周期 save 用独立的 saveStateTimeout context，不受停机信号影响。
// ============================================================================

func runScheduler(shutdownCtx context.Context, stopCh <-chan struct{}, done chan<- struct{}) {
	ticker := time.NewTicker(schedulerTickInterval)
	defer ticker.Stop()
	saveTicker := time.NewTicker(stateSaveInterval)
	defer saveTicker.Stop()
	shadowCleanupTicker := time.NewTicker(10 * time.Minute)
	defer shadowCleanupTicker.Stop()
	defer close(done)

	for {
		select {
		case <-stopCh:
			// 退出前若有未保存状态则保存
			if dirty := func() bool {
				mu.RLock()
				defer mu.RUnlock()
				return stateDirty
			}(); dirty {
				if err := saveState(shutdownCtx); err != nil {
					log.Printf("[scheduler] final save failed: %v", err)
				} else {
					log.Printf("[scheduler] final state saved")
				}
			}
			return
		case now := <-ticker.C:
			checkRecovery(now)
		case <-saveTicker.C:
			if dirty := func() bool {
				mu.RLock()
				defer mu.RUnlock()
				return stateDirty
			}(); dirty {
				ctx, cancel := context.WithTimeout(context.Background(), saveStateTimeout)
				if err := saveState(ctx); err != nil {
					log.Printf("[scheduler] save state failed: %v", err)
				} else {
					log.Printf("[scheduler] state saved")
				}
				cancel()
			}
		case <-shadowCleanupTicker.C:
			cleanupExpiredGeminiShadows()
		}
	}
}

// checkRecovery 遍历所有upstream，判断是否触发恢复：
//   - count型：RecoveryCron 周期匹配命中即重置计数（不论是否已耗尽，
//     周期刷新语义：每到一个 cron 周期就把 count 清零，而非仅用于从 exhaust 恢复）
//   - usage/balance/exhaust型：now >= RecoveryAt 时间点触发
//     （RecoveryAt 为零值视为旧文件迁移，立即触发一次 provider 检查）
//     统一受 recoveryMinGap 约束，防止短时间内重复触发
func checkRecovery(now time.Time) {
	mu.RLock()
	// 复制一份需要触发的upstream名，避免长时间持锁且不持有 state 指针
	var toRecover []string
	for name, st := range stateMap {
		if st == nil {
			continue
		}
		cfg := tokenMap.Upstreams[name].Availability
		isCount := cfg != nil && cfg.Type == availCount
		// count 型按 cron 周期刷新计数，不要求已耗尽；
		// 其余类型仅在 exhausted 时才参与恢复调度。
		if !isCount && !st.Exhausted {
			continue
		}

		if isCount {
			// count型：按 cron 周期匹配
			if st.RecoveryCron == "" {
				continue
			}
			sched, err := parseCronCached(st.RecoveryCron)
			if err != nil {
				continue
			}
			if !sched.Match(now) {
				continue
			}
		} else {
			// usage/balance/exhaust型：按精确时间点触发
			if !st.RecoveryAt.IsZero() && now.Before(st.RecoveryAt) {
				continue
			}
			// RecoveryAt 为零值 → 旧文件迁移，立即触发一次 provider check
		}
		if !st.LastRecovery.IsZero() && now.Sub(st.LastRecovery) < recoveryMinGap {
			continue
		}
		toRecover = append(toRecover, name)
	}
	mu.RUnlock()

	for _, name := range toRecover {
		recoverUpstream(name, now)
	}
}

func recoverUpstream(name string, now time.Time) {
	mu.Lock()
	cur := stateMap[name]
	if cur == nil {
		mu.Unlock()
		return
	}
	cfg := tokenMap.Upstreams[name].Availability
	isCount := cfg != nil && cfg.Type == availCount
	// count 型按 cron 周期刷新计数（不论是否已耗尽）；
	// 其余类型仅在 exhausted 时才恢复，避免对健康 upstream 误触 provider 复查。
	if !isCount && !cur.Exhausted {
		mu.Unlock()
		return
	}

	// count型：直接重置count并恢复。
	// 注意：count 型语义由网关自行计数 + cron 周期重置驱动，不依赖 provider 检查，
	// 所以这里不调用 applyAvailabilityResult，LastChecked 也不会更新（保持零值）。
	// 若未来引入依赖 LastChecked 的逻辑需在此显式更新。
	if isCount {
		wasExhausted := cur.Exhausted
		// count 已是 0 且未耗尽时为 no-op，避免每个 cron 周期都触发无意义的 DB 写。
		// LastRecovery 始终更新以维持 recoveryMinGap 防抖，但不 markDirty（不持久化）。
		if cur.Count != 0 || wasExhausted {
			cur.Count = 0
			cur.Exhausted = false
			markDirty()
		}
		cur.LastRecovery = now
		mu.Unlock()
		log.Printf("[refresh] upstream=%s count reset by cron (exhausted=%v->false)", name, wasExhausted)
		return
	}

	// 无 availability 配置（旧版隐式 exhaust 迁移场景）：到达 RecoveryAt 即直接自动恢复，
	// 不调用 checkAvailability（否则沿用已过期的 RecoveryAt 形成 60s 死循环）。
	// 清零 RecoveryAt 防止下次 exhaust 时沿用过期旧值导致振荡。
	if cfg == nil {
		cur.Exhausted = false
		cur.RecoveryAt = time.Time{}
		cur.RecoveryCron = ""
		cur.LastRecovery = now
		markDirty()
		mu.Unlock()
		log.Printf("[recover] upstream=%s no-config auto-recovered (exhausted=false)", name)
		return
	}

	// exhaust型：到达 RecoveryAt 直接恢复，不调用 provider。
	// 因为 fallbackResult 永远返回 Exhausted=true，若走下方 provider 路径会死循环。
	if cfg.Type == availExhaust {
		cur.Exhausted = false
		cur.RecoveryAt = time.Time{}
		cur.RecoveryCron = ""
		cur.LastRecovery = now
		markDirty()
		mu.Unlock()
		log.Printf("[recover] upstream=%s exhaust auto-recovered (exhausted=false)", name)
		return
	}

	// usage/balance型：先做值拷贝快照，释放锁后调用provider校验。
	// 用 availSF singleflight 合并：若 handler 路径正在对同一 upstream 做可用性检查，
	// 这里复用其结果，避免并发对外部 provider 发起重复请求。
	stCopy := *cur
	mu.Unlock()

	res := availSF.Do(name, func() AvailabilityResult {
		return checkAvailability(name, cfg, &stCopy)
	})
	applyAvailabilityResult(name, res) // 内部自带锁，会更新 Exhausted 等字段

	// 统一写入 LastRecovery 与 dirty
	mu.Lock()
	if cur = stateMap[name]; cur != nil {
		cur.LastRecovery = now
		markDirty()
	}
	mu.Unlock()
	log.Printf("[recover] upstream=%s rechecked by provider (exhausted=%v)", name, res.Exhausted)
}
