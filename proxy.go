// proxy.go — 代理配置与连通性检测。
// 代理优先级：账号代理 > 全局设置（data/settings.json）> MUSE_PROXY 环境变量 > 直连。
package main

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// validateProxyURL 校验代理地址格式（支持 http/https/socks5）。
func validateProxyURL(p string) error {
	u, err := url.Parse(p)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "socks5") {
		return fmt.Errorf("代理地址无效（支持 http:// 或 socks5://）: %s", p)
	}
	return nil
}

// effectiveProxy 解析最终生效代理：账号代理 > 全局设置 > MUSE_PROXY > 直连。
func effectiveProxy(accountProxy, globalProxy string) string {
	if p := strings.TrimSpace(accountProxy); p != "" {
		return p
	}
	if p := strings.TrimSpace(globalProxy); p != "" {
		return p
	}
	return strings.TrimSpace(os.Getenv("MUSE_PROXY"))
}

// newMuseClient 构造访问 muse.ai 的 HTTP 客户端（可选代理）。
func newMuseClient(proxyURL string, timeout time.Duration) (*http.Client, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if p := strings.TrimSpace(proxyURL); p != "" {
		if err := validateProxyURL(p); err != nil {
			return nil, err
		}
		u, _ := url.Parse(p)
		transport.Proxy = http.ProxyURL(u)
	}
	return &http.Client{Timeout: timeout, Transport: transport}, nil
}

// hintProxyOnNetErr 网络层失败时补充代理配置提示（muse.ai 国内直连不可达）。
func hintProxyOnNetErr(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	markers := []string{
		"TLS handshake timeout", "connection refused", "context deadline exceeded",
		"Client.Timeout", "no such host", "i/o timeout", "proxyconnect", "EOF",
		"network is unreachable", "connection reset",
	}
	for _, m := range markers {
		if strings.Contains(msg, m) {
			return fmt.Errorf("%w\n提示：muse.ai 需代理才能访问。请点右上角「⚙ 代理」设置全局代理（如 socks5://127.0.0.1:10808），或在账号编辑的「请求代理」里填写，或用 启动账号管理.bat 启动", err)
		}
	}
	return err
}

// ProxyTestResult 代理连通性检测结果。
type ProxyTestResult struct {
	Proxy     string `json:"proxy"`
	LatencyMs int64  `json:"latency_ms"`
	Status    int    `json:"status"`
	OK        bool   `json:"ok"`
	Error     string `json:"error,omitempty"`
}

// TestProxy 用给定代理 GET muse.ai 首页，验证连通性与延迟。
func TestProxy(proxyURL string) *ProxyTestResult {
	return testProxyFrom("https://muse.ai/", proxyURL, 20*time.Second)
}

// testProxyFrom 可注入目标地址（便于测试）。收到任何 HTTP 响应即视为连通。
func testProxyFrom(target, proxyURL string, timeout time.Duration) *ProxyTestResult {
	res := &ProxyTestResult{Proxy: strings.TrimSpace(proxyURL)}
	client, err := newMuseClient(proxyURL, timeout)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	req, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	req.Header.Set("User-Agent", defaultInviteUA)
	req.Header.Set("Accept", "*/*")
	start := time.Now()
	resp, err := client.Do(req)
	res.LatencyMs = time.Since(start).Milliseconds()
	if err != nil {
		res.Error = hintProxyOnNetErr(fmt.Errorf("连接失败: %w", err)).Error()
		return res
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	res.Status = resp.StatusCode
	res.OK = true // 收到响应 = 代理链路通（状态码仅作参考）
	return res
}
