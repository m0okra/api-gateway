package main

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ============================================================================
// 轻量Cron：6字段（秒 分 时 日 月 周），支持 * / N / a-b / a,b,c / */S / a-b/S
// ============================================================================

type CronSchedule struct {
	fields [6]map[int]bool
}

var cronFieldRanges = [6][2]int{
	{0, 59}, // 秒
	{0, 59}, // 分
	{0, 23}, // 时
	{1, 31}, // 日
	{1, 12}, // 月
	{0, 6},  // 周（0=周日）
}

func parseCron(expr string) (*CronSchedule, error) {
	parts := strings.Fields(strings.TrimSpace(expr))
	if len(parts) != 6 {
		return nil, fmt.Errorf("cron expression must have 6 fields, got %d: %q", len(parts), expr)
	}
	s := &CronSchedule{}
	for i, p := range parts {
		set, err := parseCronField(p, cronFieldRanges[i][0], cronFieldRanges[i][1])
		if err != nil {
			return nil, fmt.Errorf("cron field %d (%q): %w", i, p, err)
		}
		s.fields[i] = set
	}
	return s, nil
}

// parseCronCached 按 cron 表达式字符串缓存解析结果，避免每秒重解析。
// 仅缓存成功结果；解析失败不缓存，invalid cron 修复后立即生效。
// 底层使用LRU避免长生命周期下因cron表达式（尤其provider按resetInSec动态生成）无限增长。
func parseCronCached(expr string) (*CronSchedule, error) {
	if v, ok := cronCache.Load(expr); ok {
		return v, nil
	}
	s, err := parseCron(expr)
	if err != nil {
		return nil, err
	}
	cronCache.Store(expr, s)
	return s, nil
}

func parseCronField(s string, min, max int) (map[int]bool, error) {
	set := make(map[int]bool)
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "*" {
			for v := min; v <= max; v++ {
				set[v] = true
			}
			continue
		}
		// */N
		if strings.HasPrefix(part, "*/") {
			step, err := strconv.Atoi(part[2:])
			if err != nil || step <= 0 {
				return nil, fmt.Errorf("invalid step %q", part[2:])
			}
			for v := min; v <= max; v += step {
				set[v] = true
			}
			continue
		}
		// a-b 或 a-b/S
		if strings.Contains(part, "-") {
			var lo, hi, step int = 0, 0, 1
			rangePart := part
			if idx := strings.Index(part, "/"); idx >= 0 {
				rangePart = part[:idx]
				st, err := strconv.Atoi(part[idx+1:])
				if err != nil || st <= 0 {
					return nil, fmt.Errorf("invalid step %q", part[idx+1:])
				}
				step = st
			}
			bounds := strings.SplitN(rangePart, "-", 2)
			if len(bounds) != 2 {
				return nil, fmt.Errorf("invalid range %q", part)
			}
			var err error
			lo, err = strconv.Atoi(bounds[0])
			if err != nil {
				return nil, fmt.Errorf("invalid range lo %q", bounds[0])
			}
			hi, err = strconv.Atoi(bounds[1])
			if err != nil {
				return nil, fmt.Errorf("invalid range hi %q", bounds[1])
			}
			if lo < min || hi > max || lo > hi {
				return nil, fmt.Errorf("range %d-%d out of [%d,%d]", lo, hi, min, max)
			}
			for v := lo; v <= hi; v += step {
				set[v] = true
			}
			continue
		}
		// 单值
		v, err := strconv.Atoi(part)
		if err != nil || v < min || v > max {
			return nil, fmt.Errorf("invalid value %q (range [%d,%d])", part, min, max)
		}
		set[v] = true
	}
	return set, nil
}

func (s *CronSchedule) Match(t time.Time) bool {
	return s.fields[0][t.Second()] &&
		s.fields[1][t.Minute()] &&
		s.fields[2][t.Hour()] &&
		s.fields[3][t.Day()] &&
		s.fields[4][int(t.Month())] &&
		s.fields[5][int(t.Weekday())]
}

// ============================================================================
// cronLRU：带容量上限的最近最少使用缓存
// 双向链表 + map；最近访问移到头部，超容淘汰尾部。
// ============================================================================

type cronEntry struct {
	key        string
	val        *CronSchedule
	prev, next *cronEntry
}

type cronLRU struct {
	mu         sync.Mutex
	items      map[string]*cronEntry
	head, tail *cronEntry
	cap        int
}

func newCronLRU(cap int) *cronLRU {
	if cap <= 0 {
		cap = 64
	}
	c := &cronLRU{
		items: make(map[string]*cronEntry, cap),
		cap:   cap,
	}
	// 哨兵节点，简化边界处理
	c.head = &cronEntry{}
	c.tail = &cronEntry{}
	c.head.next = c.tail
	c.tail.prev = c.head
	return c
}

// Load 取值；命中时将其移到头部。
func (c *cronLRU) Load(key string) (*CronSchedule, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.items[key]; ok {
		c.moveToFront(e)
		return e.val, true
	}
	return nil, false
}

// Store 写入；若为新条目且超出容量则淘汰尾部。
func (c *cronLRU) Store(key string, val *CronSchedule) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.items[key]; ok {
		e.val = val
		c.moveToFront(e)
		return
	}
	e := &cronEntry{key: key, val: val}
	c.items[key] = e
	c.pushFront(e)
	if len(c.items) > c.cap {
		victim := c.tail.prev
		if victim != c.head {
			c.remove(victim)
			delete(c.items, victim.key)
		}
	}
}

func (c *cronLRU) pushFront(e *cronEntry) {
	e.prev = c.head
	e.next = c.head.next
	c.head.next.prev = e
	c.head.next = e
}

func (c *cronLRU) remove(e *cronEntry) {
	e.prev.next = e.next
	e.next.prev = e.prev
	e.prev, e.next = nil, nil
}

func (c *cronLRU) moveToFront(e *cronEntry) {
	if e.prev == c.head {
		return
	}
	c.remove(e)
	c.pushFront(e)
}
