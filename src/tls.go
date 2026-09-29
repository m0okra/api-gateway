package main

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"
)

// ============================================================================
// HTTPS 监听支持（仅标准库 crypto/tls + crypto/x509）
//
// 设计要点：
//  1. 证书由用户通过 -cert / -key 提供（PEM），本文件不生成、不申请证书。
//  2. 走 http.Server.ServeTLS 而不是自己包 tls.NewListener：只有 ServeTLS 会把
//     "h2" 写进 ALPN 并注册 HTTP/2 的 TLSNextProto，直接 Serve(tls.NewListener(...))
//     会让客户端停在 HTTP/1.1（除非手工维护 NextProtos）。
//  3. **每个监听端口一个独立的 *tls.Config 实例**：net/http 在 ServeTLS 初始化
//     期间会无锁 append 到 s.TLSConfig.NextProtos（见 h2_bundle.go 的
//     http2ConfigureServer），而它是在这次写入之后才 cloneTLSConfig，多端口并发
//     启动时共享同一实例构成数据竞争。证书对象（*tls.Certificate）本身只读，可共享。
// ============================================================================

// certReloader 持有证书/私钥文件路径与最近一次成功加载的证书。
// 每次 TLS 握手经 GetCertificate 回调按文件指纹（mtime+size）惰性重载，
// 使 certbot / acme.sh 等续期工具替换证书文件后**无需重启进程**即可生效。
type certReloader struct {
	certFile string
	keyFile  string

	mu  sync.Mutex
	fp  string           // 上次成功加载时的文件指纹
	cur *tls.Certificate // 最近一次成功加载的证书（重载失败时作为兜底沿用）
}

// newCertReloader 构造并**立即加载一次**证书，加载失败即返回错误，
// 与项目 fail-fast 风格一致（由调用方 log.Fatal 退出，避免半启动）。
func newCertReloader(certFile, keyFile string) (*certReloader, error) {
	r := &certReloader{certFile: certFile, keyFile: keyFile}
	if _, err := r.reload(); err != nil {
		return nil, err
	}
	return r, nil
}

// certFileFingerprint 计算证书与私钥文件的「内容版本」标识（路径 + mtime + size）。
// 续期工具普遍采用「写临时文件 + rename」替换证书，mtime 必然变化，
// 因此该指纹足以识别换证书事件，无需读取文件内容比对哈希。
func (r *certReloader) certFileFingerprint() (string, error) {
	var b strings.Builder
	for _, p := range []string{r.certFile, r.keyFile} {
		st, err := os.Stat(p)
		if err != nil {
			return "", fmt.Errorf("stat %s: %w", p, err)
		}
		fmt.Fprintf(&b, "%s|%d|%d;", p, st.ModTime().UnixNano(), st.Size())
	}
	return b.String(), nil
}

// reload 在文件指纹变化时重新加载证书；指纹未变则直接复用缓存。
// 返回的证书指针在重新加载后会被替换，调用方不可长期持有旧指针做写操作。
//
// 锁范围刻意收窄（与 saveState 的「锁内快照 → 锁外 I/O」同一思路）：该函数在**每次
// TLS 握手**都会经 GetCertificate 调用一次，而指纹计算含两次 os.Stat 系统调用、
// 加载含磁盘读 + 私钥解析。若全程持锁，高新建连接速率下所有握手会串行等待。
// 因此指纹计算与读盘均放在锁外，仅在比较/更新缓存时短暂持锁。
func (r *certReloader) reload() (*tls.Certificate, error) {
	fp, err := r.certFileFingerprint()
	if err != nil {
		return nil, err
	}

	r.mu.Lock()
	if r.cur != nil && fp == r.fp {
		cur := r.cur
		r.mu.Unlock()
		return cur, nil
	}
	r.mu.Unlock()

	// 指纹已变化（或首次加载，此时 r.cur==nil）：读盘解析。
	// 此处不加锁，故并发握手可能同时观察到变化并各自加载一次——结果等价（读的是
	// 同一份文件），仅多一次解析开销；即使后写入者的指纹略旧，下一次握手比对不上
	// 便会再重载一次，自愈且绝不会把错误证书当成生效版本。
	cert, err := tls.LoadX509KeyPair(r.certFile, r.keyFile)
	if err != nil {
		// 不动 r.cur：保留上一份成功加载的证书作为兜底。
		return nil, fmt.Errorf("load key pair (%s, %s): %w", r.certFile, r.keyFile, err)
	}

	r.mu.Lock()
	r.cur, r.fp = &cert, fp
	r.mu.Unlock()

	// 日志（含 x509 解析）放在锁外，避免续期瞬间阻塞并发握手。
	logCertInfo(r.certFile, &cert)
	return &cert, nil
}

// getCertificate 是 tls.Config.GetCertificate 回调：每次握手触发一次文件指纹检查。
// 重载失败（如续期工具写入的中间态、文件短暂缺失）**不致命**——沿用上一次成功的
// 证书继续服务并记日志，避免一次文件抖动导致 TLS 端口整体不可用；
// 只有从未成功加载过（启动阶段已拦截）才会把错误抛给客户端。
func (r *certReloader) getCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	cert, err := r.reload()
	if err == nil {
		return cert, nil
	}

	r.mu.Lock()
	stale := r.cur
	r.mu.Unlock()
	if stale != nil {
		log.Printf("[TLS] certificate reload failed, keep previous one: %v", err)
		return stale, nil
	}
	return nil, err
}

// logCertInfo 打印证书主体信息与剩余有效期，并对临期证书给出续期提醒。
// 解析失败只降级为跳过日志，不影响已成功加载的证书可用性。
func logCertInfo(path string, cert *tls.Certificate) {
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		log.Printf("[TLS] certificate loaded: %s", path)
		return
	}
	remaining := time.Until(leaf.NotAfter)
	log.Printf("[TLS] certificate loaded: file=%s subject=%q SANs=%v notAfter=%s (%.1f days left)",
		path, leaf.Subject.CommonName, leaf.DNSNames,
		leaf.NotAfter.Format(time.RFC3339), remaining.Hours()/24)
	if remaining <= 0 {
		log.Printf("[TLS] warning: certificate %s has EXPIRED at %s", path, leaf.NotAfter.Format(time.RFC3339))
	} else if remaining < 14*24*time.Hour {
		log.Printf("[TLS] warning: certificate %s expires in %.1f days, renew soon",
			path, remaining.Hours()/24)
	}
}

// newTLSConfig 为**单个**监听端口构造独立的 *tls.Config。
// 证书经 GetCertificate 回调动态提供（而非静态 Certificates 字段），
// 使文件替换能在不重启进程的前提下于下一次握手生效。
//
// 说明：Certificates 留空 + GetCertificate 非空时，crypto/tls 必然调用回调
// （getCertificate 中 len(c.Certificates)==0 即走该分支），因此无需再填静态证书。
func newTLSConfig(reloader *certReloader) *tls.Config {
	return &tls.Config{
		// 显式声明最低版本 TLS 1.2，拒绝 TLS 1.0/1.1（默认值本身已禁用，
		// 这里写死以防未来 Go 默认值变化，也让配置意图可读）。
		MinVersion: tls.VersionTLS12,
		// 显式预设 ALPN：ServeTLS 会自动补齐/校正这两项，但先写好可让
		// 该实例自解释，并避免依赖 ServeTLS 对 NextProtos 的隐式写入。
		NextProtos:     []string{"h2", "http/1.1"},
		GetCertificate: reloader.getCertificate,
	}
}
