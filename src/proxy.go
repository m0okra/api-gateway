package main

import (
	"fmt"
	"log"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// ============================================================================
// 代理支持（per-upstream 统一代理 + per-model 覆盖）
//
// 生效规则：
//   - UpstreamConfig.Proxy：该 upstream 所有出站请求（转发 + 可用性检查）统一走代理
//   - UpstreamConfig.ModelProxies：以 alias 替换后实际发送的模型名（sendModel）为 key，
//     命中则覆盖 Proxy；value 空串 = 该模型显式直连豁免；未命中回退 Proxy
//   - 均未配置 → 走现有 sharedTransport / proxyClient / streamClient，行为不变
//
// 实现：按代理 URL 懒加载缓存 *http.Transport（连接池复用，锁保护，同一 URL 只建一个），
// 从 transport 派生非流式/流式两个 *http.Client（超时语义与全局
// proxyClient/streamClient 一一对应），供转发路径与 provider 可用性检查路径共用。
// ============================================================================

var (
	proxyMu         sync.Mutex
	proxyTransports = map[string]*http.Transport{}                      // proxy URL -> Transport
	proxyClients    = map[string]struct{ plain, stream *http.Client }{} // proxy URL -> client 对
)

// resolveProxyURL 按生效规则解析本次请求应使用的代理 URL。
//   - sendModel 命中 modelProxies 且 value 非空 → 返回该 value（覆盖 Proxy）
//   - sendModel 命中 modelProxies 且 value 为空串 → 返回 ""（显式直连豁免）
//   - 未命中 modelProxies → 回退 upstream 级 Proxy
//
// 返回 (proxyURL, hitModelOverride)：hitModelOverride 表示本次结果是否由 modelProxies
// 决定（含直连豁免），仅用于日志区分。
func resolveProxyURL(cfg *UpstreamConfig, sendModel string) (proxyURL string, hitModelOverride bool) {
	if cfg == nil {
		return "", false
	}
	if cfg.ModelProxies != nil && sendModel != "" {
		if p, ok := cfg.ModelProxies[sendModel]; ok {
			return p, true // 空串 p = 显式直连豁免
		}
	}
	return cfg.Proxy, false
}

// newProxyTransport 按代理 URL 构造 Transport（连接池参数与 sharedTransport 一致）。
// 代理 URL 在 Validate() 阶段已校验合法，此处解析失败属编程错误，回退直连 transport 并告警。
func newProxyTransport(proxyURL string) *http.Transport {
	parsed, err := url.Parse(proxyURL)
	if err != nil {
		log.Printf("[PROXY] parse %q failed: %v -> fallback direct", proxyURL, err)
		return sharedTransport
	}
	return &http.Transport{
		Proxy:               http.ProxyURL(parsed),
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 20,
		IdleConnTimeout:     90 * time.Second,
	}
}

// getProxyTransport 返回指定代理 URL 的 Transport（懒加载缓存）。
// provider 可用性检查路径使用；proxyURL 为空时返回 sharedTransport（直连）。
func getProxyTransport(proxyURL string) *http.Transport {
	if proxyURL == "" {
		return sharedTransport
	}
	proxyMu.Lock()
	defer proxyMu.Unlock()
	if tr, ok := proxyTransports[proxyURL]; ok {
		return tr
	}
	tr := newProxyTransport(proxyURL)
	proxyTransports[proxyURL] = tr
	return tr
}

// getProxyClients 返回指定代理 URL 的非流式/流式 client 对（懒加载缓存）。
// 转发路径使用；内部与 getProxyTransport 共享同一 Transport 缓存。
func getProxyClients(proxyURL string) (plain, stream *http.Client) {
	proxyMu.Lock()
	defer proxyMu.Unlock()
	if pair, ok := proxyClients[proxyURL]; ok {
		return pair.plain, pair.stream
	}
	tr, ok := proxyTransports[proxyURL]
	if !ok {
		tr = newProxyTransport(proxyURL)
		proxyTransports[proxyURL] = tr
	}
	pair := struct{ plain, stream *http.Client }{
		plain:  &http.Client{Timeout: 120 * time.Second, Transport: tr}, // 非流式，与 proxyClient 一致
		stream: &http.Client{Timeout: 0, Transport: tr},                 // 流式无整体超时，与 streamClient 一致
	}
	proxyClients[proxyURL] = pair
	return pair.plain, pair.stream
}

// maskProxyURL 脱敏代理 URL 的 userinfo（日志用）：
// socks5://user:pass@127.0.0.1:1080 -> socks5://***@127.0.0.1:1080
// 不用 url.User 重建（会百分号转义特殊字符），手动拼接保持可读。
func maskProxyURL(proxyURL string) string {
	parsed, err := url.Parse(proxyURL)
	if err != nil || parsed.User == nil {
		return proxyURL
	}
	masked := parsed.Scheme + "://" + mask + "@" + parsed.Host
	if parsed.Path != "" {
		masked += parsed.Path
	}
	if parsed.RawQuery != "" {
		masked += "?" + parsed.RawQuery
	}
	return masked
}

// proxyLogDesc 返回用于日志的代理描述（脱敏）；空串返回 "direct"。
func proxyLogDesc(proxyURL string) string {
	if proxyURL == "" {
		return "direct"
	}
	return fmt.Sprintf("proxy %s", maskProxyURL(proxyURL))
}
