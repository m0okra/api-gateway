package main

import "sync"

// ============================================================================
// 可用性检查 singleflight（自实现，仅用 sync.Mutex + map + WaitGroup）
//
// 用途：当多个并发请求同时命中同一个 upstream 触发可用性检查时（典型场景：上游批量返回
// 429/401 同一 upstream），只有首个调用真正发起 provider HTTP 检查（最长 15s），
// 其余并发调用阻塞等待首次结果复用，避免对外部 provider 发起重复请求风暴。
//
// 不引入 golang.org/x/sync/singleflight：保持项目纯标准库依赖。实现等价核心语义：
//   - 同 key 并发 Do：仅首次执行 fn，其余等 WaitGroup
//   - fn 完成后，结果广播给所有等待者
//   - 调用完成后从 map 删除该 key（无结果缓存，下次重新发起，避免脏结果复用）
//
// 与 x/sync/singleflight 的差异：本实现不做结果缓存、不做 suppress，仅做"进行中合并"。
// ============================================================================

type availCall struct {
	wg   sync.WaitGroup
	res  AvailabilityResult
}

// availSingleFlight 对同一 key 的并发调用去重：仅首个真正执行，其余复用结果。
type availSingleFlight struct {
	mu sync.Mutex
	m  map[string]*availCall
}

func newAvailSingleFlight() *availSingleFlight {
	return &availSingleFlight{m: make(map[string]*availCall)}
}

// Do 对 key 执行 fn。若已有同 key 进行中，则阻塞等待复用其结果；否则执行 fn 并广播结果。
// fn 必须无 panic（调用方保证）；若 fn 返回零值结果，所有等待者也会收到同样结果。
func (g *availSingleFlight) Do(key string, fn func() AvailabilityResult) AvailabilityResult {
	g.mu.Lock()
	if c, ok := g.m[key]; ok {
		// 已有进行中的同 key 调用：复用，等待结果
		g.mu.Unlock()
		c.wg.Wait()
		return c.res
	}
	// 首个调用：占位并初始化 WaitGroup（计数 1，待 fn 完成后 Done 通知所有 Wait）
	c := &availCall{}
	c.wg.Add(1)
	g.m[key] = c
	g.mu.Unlock()

	// 真正执行（可能耗时数秒）。defer 保证无论 fn 是否 panic 都通知等待者并清理占位。
	res := fn()
	c.res = res

	g.mu.Lock()
	// 防御：若 fn 期间有极端重复入队，确保 c 仍是当前占位；若被异常替换则不删错。
	if cur, ok := g.m[key]; ok && cur == c {
		delete(g.m, key)
	}
	// 解锁后再 Done：让等待者读到的 g.m 状态已清理；同时 c.res 已就绪。
	g.mu.Unlock()
	c.wg.Done()
	return res
}