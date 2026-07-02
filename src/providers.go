package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"time"
)

// ============================================================================
// 可用性Provider：参考cclimits.py，使用alias的token；额外内容从Extra读
//   - usage/balance型：调用对应provider查询
//   - 出问题走兜底逻辑
// ============================================================================

// checkAvailability 根据 alias 配置与当前 state 执行可用性检查，返回是否exhaust
// 若检查过程出错，走兜底逻辑（返回exhausted=true）
func checkAvailability(aliasName string, cfg *AvailabilityConfig, st *AvailabilityState) AvailabilityResult {
	if cfg == nil {
		// 无配置 → 兜底
		return fallbackResult(st)
	}

	switch cfg.Type {
	case availCount:
		// count型：网关自行统计，检查时即判断 count>=limit
		exhausted := st.Count >= cfg.Limit
		return AvailabilityResult{Exhausted: exhausted, Count: st.Count, RecoveryCron: cfg.RefreshCron}

	case availBalance:
		return checkBalanceProvider(cfg.Provider, aliasName, st)

	case availUsage:
		return checkUsageProvider(cfg.Provider, aliasName, cfg, st)

	case availFallback:
		return fallbackResult(st)

	default:
		// 未知类型走兜底
		return fallbackResult(st)
	}
}

func fallbackResult(st *AvailabilityState) AvailabilityResult {
	res := AvailabilityResult{Exhausted: true}
	// 兜底：30min后复查。优先沿用已有的 RecoveryAt，避免重复触发时不断后延
	if st != nil && !st.RecoveryAt.IsZero() {
		res.RecoveryAt = st.RecoveryAt
	} else {
		res.RecoveryAt = time.Now().Add(defaultFallbackRecoverGap)
	}
	return res
}

// ---- balance型 provider ----

func checkBalanceProvider(provider, aliasName string, st *AvailabilityState) AvailabilityResult {
	switch provider {
	case "deepseek":
		return checkDeepSeekBalance(aliasName, st)
	case "kimi":
		return checkKimiBalance(aliasName, st)
	case "openrouter":
		return checkOpenRouterBalance(aliasName, st)
	default:
		// 未实现的provider走兜底
		return fallbackResult(st)
	}
}

// ---- usage型 provider ----

func checkUsageProvider(provider, aliasName string, cfg *AvailabilityConfig, st *AvailabilityState) AvailabilityResult {
	switch provider {
	case "opencode-go":
		return checkOpenCodeGoUsage(aliasName, cfg, st)
	case "claude":
		return checkClaudeUsage(aliasName, st)
	case "codex":
		return checkCodexUsage(aliasName, st)
	case "gemini":
		return checkGeminiUsage(aliasName, st)
	case "zai":
		return checkZAIUsage(aliasName, st)
	case "minimax":
		return checkMiniMaxUsage(aliasName, st)
	default:
		return fallbackResult(st)
	}
}

// ---- DeepSeek（余额型，具体实现）----
// GET https://api.deepseek.com/user/balance
// 余额为0则exhaust
func checkDeepSeekBalance(aliasName string, st *AvailabilityState) AvailabilityResult {
	alias := getAliasConfig(aliasName)
	if alias == nil {
		return fallbackResult(st)
	}
	token := alias.RealToken

	body, status, err := httpGetJSON("https://api.deepseek.com/user/balance",
		map[string]string{"Authorization": "Bearer " + token, "Accept": "application/json"})
	if err != nil || status != 200 {
		log.Printf("[AVAIL] deepseek check failed alias=%s status=%d err=%v -> fallback", aliasName, status, err)
		return fallbackResult(st)
	}

	// {"is_available":true,"balance_infos":[{"total_balance":"1.23","currency":"CNY",...}]}
	avail, _ := body["is_available"].(bool)
	infos, _ := body["balance_infos"].([]interface{})
	if !avail || len(infos) == 0 {
		log.Printf("[AVAIL] deepseek alias=%s not available -> fallback", aliasName)
		return fallbackResult(st)
	}
	info, _ := infos[0].(map[string]interface{})
	balStr, _ := info["total_balance"].(string)
	bal, _ := strconv.ParseFloat(balStr, 64)

	res := AvailabilityResult{Balance: bal, Exhausted: bal <= 0}
	if bal <= 0 {
		// 余额耗尽，30min后复查（DeepSeek 未返回重置时间，使用默认间隔）
		// 仅当上次设的复查时间点尚未到期时沿用，避免沿用已过期旧值导致每 60s 死循环重试
		if st != nil && !st.RecoveryAt.IsZero() && time.Now().Before(st.RecoveryAt) {
			res.RecoveryAt = st.RecoveryAt
		} else {
			res.RecoveryAt = time.Now().Add(defaultBalanceRecoverGap)
		}
	}
	log.Printf("[AVAIL] deepseek alias=%s balance=%.4f exhausted=%v", aliasName, bal, res.Exhausted)
	return res
}

// ---- OpenCode-Go（用量型，具体实现）----
// 需要 Extra["cookie"] 与 Extra["workspaceId"]
// GET https://opencode.ai/_server?id=...&args=...
// 解析 rollingUsage:$R[1]={status,resetInSec,usagePercent}
// 任何配额层级剩余用量为0则exhaust
func checkOpenCodeGoUsage(aliasName string, cfg *AvailabilityConfig, st *AvailabilityState) AvailabilityResult {
	alias := getAliasConfig(aliasName)
	if alias == nil {
		return fallbackResult(st)
	}
	cookie := alias.Extra["cookie"]
	workspaceID := alias.Extra["workspaceId"]
	if cookie == "" || workspaceID == "" {
		log.Printf("[AVAIL] opencode-go alias=%s missing cookie/workspaceId -> fallback", aliasName)
		return fallbackResult(st)
	}

	args := fmt.Sprintf(`{"t":{"t":9,"i":0,"l":1,"a":[{"t":1,"s":%q}],"o":0},"f":31,"m":[]}`, workspaceID)
	reqURL := fmt.Sprintf("https://opencode.ai/_server?id=%s&args=%s", opencodeGoServiceID, url.QueryEscape(args))

	text, status, err := httpGetText(reqURL, map[string]string{
		"accept":            "*/*",
		"cookie":            cookie,
		"x-server-id":       opencodeGoServiceID,
		"x-server-instance": "server-fn:3",
	})
	if err != nil || status != 200 {
		log.Printf("[AVAIL] opencode-go check failed alias=%s status=%d err=%v -> fallback", aliasName, status, err)
		return fallbackResult(st)
	}

	rolling := opencodeUsageRegex.FindStringSubmatch(text)
	if rolling == nil {
		log.Printf("[AVAIL] opencode-go alias=%s parse rollingUsage failed -> fallback", aliasName)
		return fallbackResult(st)
	}
	rollingPct, _ := strconv.ParseFloat(rolling[3], 64)

	weekly := opencodeUsageRegex2.FindStringSubmatch(text)
	weeklyPct := 0.0
	if weekly != nil {
		weeklyPct, _ = strconv.ParseFloat(weekly[3], 64)
	}

	monthly := opencodeUsageRegex3.FindStringSubmatch(text)
	monthlyPct := 0.0
	if monthly != nil {
		monthlyPct, _ = strconv.ParseFloat(monthly[3], 64)
	}

	// 任何层级用量>=100则exhaust（rolling/weekly/monthly 缺一不可）
	exhausted := rollingPct >= 100 || weeklyPct >= 100 || monthlyPct >= 100

	var tiers []TierState
	tiers = append(tiers, TierState{Name: "rolling", UsedPct: rollingPct})
	if weekly != nil {
		tiers = append(tiers, TierState{Name: "weekly", UsedPct: weeklyPct})
	}
	if monthly != nil {
		tiers = append(tiers, TierState{Name: "monthly", UsedPct: monthlyPct})
	}

	res := AvailabilityResult{Exhausted: exhausted, Tiers: tiers}
	if exhausted {
		if st != nil && !st.RecoveryAt.IsZero() && time.Now().Before(st.RecoveryAt) {
			// 已有下次复查时间点且尚未到期，沿用避免每次检查都后延
			res.RecoveryAt = st.RecoveryAt
		} else {
			// 在所有已耗尽的层级中选取最长的 resetInSec 作为下次检查间隔。
			// 因为只要任一层级仍耗尽就整体不可用：rolling 即使最先恢复，
			// 但 weekly/monthly 还没到 reset 时间则 alias 依然不能转发。
			// 故必须按最长重置周期的耗尽层级来设定精确复查时间点，
			// 一次性定时而非周期 cron（cron 表达式对超大秒数会语义退化）。
			var maxReset int
			addReset := func(match []string, pct float64) {
				if match == nil || pct < 100 {
					return
				}
				rs, _ := strconv.Atoi(match[2])
				if rs > maxReset {
					maxReset = rs
				}
			}
			addReset(rolling, rollingPct)
			addReset(weekly, weeklyPct)
			addReset(monthly, monthlyPct)
			if time.Duration(maxReset)*time.Second < minRecoverGap {
				maxReset = int(minRecoverGap.Seconds()) // 地板保护
			}
			res.RecoveryAt = time.Now().Add(time.Duration(maxReset) * time.Second)
		}
	}
	log.Printf("[AVAIL] opencode-go alias=%s rolling=%.0f%% weekly=%.0f%% monthly=%.0f%% exhausted=%v",
		aliasName, rollingPct, weeklyPct, monthlyPct, res.Exhausted)
	return res
}

var (
	opencodeUsageRegex  = regexp.MustCompile(`rollingUsage:\$R\[1\]=\{status:"([^"]+)",resetInSec:(\d+),usagePercent:(\d+)\}`)
	opencodeUsageRegex2 = regexp.MustCompile(`weeklyUsage:\$R\[2\]=\{status:"([^"]+)",resetInSec:(\d+),usagePercent:(\d+)\}`)
	opencodeUsageRegex3 = regexp.MustCompile(`monthlyUsage:\$R\[3\]=\{status:"([^"]+)",resetInSec:(\d+),usagePercent:(\d+)\}`)
)

// ---- 以下provider仅框架，返回兜底 ----

func checkKimiBalance(aliasName string, st *AvailabilityState) AvailabilityResult {
	// TODO: GET https://api.moonshot.ai/v1/users/me/balance
	return fallbackResult(st)
}

func checkOpenRouterBalance(aliasName string, st *AvailabilityState) AvailabilityResult {
	// TODO: GET https://openrouter.ai/api/v1/credits
	return fallbackResult(st)
}

func checkClaudeUsage(aliasName string, st *AvailabilityState) AvailabilityResult {
	// TODO: GET https://api.anthropic.com/api/oauth/usage
	return fallbackResult(st)
}

func checkCodexUsage(aliasName string, st *AvailabilityState) AvailabilityResult {
	// TODO: GET https://chatgpt.com/backend-api/wham/usage
	return fallbackResult(st)
}

func checkGeminiUsage(aliasName string, st *AvailabilityState) AvailabilityResult {
	// TODO: POST https://cloudcode-pa.googleapis.com/v1internal:retrieveUserQuota
	// gemini的oauth从文件读，不储存于Extra
	return fallbackResult(st)
}

func checkZAIUsage(aliasName string, st *AvailabilityState) AvailabilityResult {
	// TODO: GET https://api.z.ai/api/monitor/usage/quota/limit
	return fallbackResult(st)
}

func checkMiniMaxUsage(aliasName string, st *AvailabilityState) AvailabilityResult {
	// TODO: GET {base}/v1/api/openplatform/coding_plan/remains
	return fallbackResult(st)
}

// ---- HTTP 工具（三阶段重试：2s + 4s + 8s，仅网络错误/5xx/429 触发重试）----

// providerRetryTimeouts 三阶段重试的每阶段超时。
var providerRetryTimeouts = []time.Duration{providerRetryStage1, providerRetryStage2, providerRetryStage3}

// httpGetRaw 带三阶段重试的 HTTP GET 底层实现。
// 仅对网络错误（含超时）/ 5xx 服务器错误 / 429 限流重试；
// 2xx 成功立即返回；其他 4xx（如 401/403）视为确定失败不重试。
// 每阶段使用独立超时创建 client，共享 Transport 保持连接池复用。
func httpGetRaw(reqURL string, headers map[string]string) (statusCode int, body []byte, err error) {
	for stage, t := range providerRetryTimeouts {
		var code int
		var data []byte
		code, data, err = httpGetOnce(reqURL, headers, t)
		if err != nil {
			// 网络错误（含超时）：非最后一阶段则重试
			if stage < len(providerRetryTimeouts)-1 {
				log.Printf("[AVAIL] provider request stage %d/%d timeout=%s failed: %v -> retrying",
					stage+1, len(providerRetryTimeouts), t, err)
				continue
			}
			return code, nil, err
		}
		// 2xx：确定成功
		if code >= 200 && code < 300 {
			return code, data, nil
		}
		// 429 / 5xx：服务端临时错误，非最后一阶段则重试
		if code == 429 || (code >= 500 && code < 600) {
			if stage < len(providerRetryTimeouts)-1 {
				log.Printf("[AVAIL] provider request stage %d/%d HTTP %d -> retrying",
					stage+1, len(providerRetryTimeouts), code)
				continue
			}
			return code, nil, fmt.Errorf("HTTP %d", code)
		}
		// 其他 4xx：确定失败，不重试
		return code, data, nil
	}
	return 0, nil, err // unreachable
}

// httpGetOnce 单次 HTTP GET，使用指定超时。
func httpGetOnce(reqURL string, headers map[string]string, timeout time.Duration) (int, []byte, error) {
	client := &http.Client{Timeout: timeout, Transport: sharedTransport}
	req, err := http.NewRequest("GET", reqURL, nil)
	if err != nil {
		return 0, nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, data, nil
}

func httpGetJSON(reqURL string, headers map[string]string) (map[string]interface{}, int, error) {
	code, data, err := httpGetRaw(reqURL, headers)
	if err != nil {
		return nil, code, err
	}
	var m map[string]interface{}
	if jerr := json.Unmarshal(data, &m); jerr != nil {
		return nil, code, fmt.Errorf("json decode failed: %w", jerr)
	}
	return m, code, nil
}

func httpGetText(reqURL string, headers map[string]string) (string, int, error) {
	code, data, err := httpGetRaw(reqURL, headers)
	if err != nil {
		return "", code, err
	}
	return string(data), code, nil
}

func getAliasConfig(name string) *AliasConfig {
	mu.RLock()
	defer mu.RUnlock()
	if a, ok := tokenMap.Aliases[name]; ok {
		return &a
	}
	return nil
}
