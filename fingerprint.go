// fingerprint.go — 每账号独立浏览器请求头指纹。
// 目的（2026-11-16 用户要求）：多账号共用同一 UA / Accept-Language / sec-ch-ua
// 会被服务端通过请求头把账号关联起来（独立、不要混淆）。本文件从账号的稳定
// 标识（登录邮箱，缺省用账号 ID）做 SHA-256 哈希，确定性地选出一套浏览器指纹：
//   - 同一账号每次请求指纹完全一致（重启、重编译、换机器都不变）；
//   - 不同账号指纹高概率互不相同（300 种组合：10 版本 × 3 平台 × 10 语言）；
//   - 无需任何存储改动（哈希即存储），老数据零迁移。
package main

import (
	"crypto/sha256"
	"net/http"
	"strings"
)

// BrowserFingerprint 单账号浏览器指纹核心 4 项。
// Sec-Fetch-*、Origin/Referer 等与"哪台浏览器"无关的头由各请求函数按场景设置。
type BrowserFingerprint struct {
	UserAgent       string
	SecChUA         string
	SecChUAPlatform string // 含引号的取值形态，如 `"Windows"`
	AcceptLanguage  string
	ChromeMajor     string // 主版本号，测试一致性用
}

// fpPlatform 平台池：UA 平台片段 ↔ sec-ch-ua-platform 取值必须严格对应。
type fpPlatform struct {
	uaFragment    string
	secChPlatform string
}

// 指纹池（2026-11 处于活跃支持区间的 Chrome 主版本 + 常见桌面平台 + 常见语言）。
var fpChromeVersions = []string{"133", "134", "135", "136", "137", "138", "139", "140", "141", "142"}

var fpPlatforms = []fpPlatform{
	{"Windows NT 10.0; Win64; x64", "Windows"},
	{"Macintosh; Intel Mac OS X 10_15_7", "macOS"},
	{"X11; Linux x86_64", "Linux"},
}

var fpLanguages = []string{
	"zh-CN,zh;q=0.9",
	"en-US,en;q=0.9",
	"ja-JP,ja;q=0.9",
	"ko-KR,ko;q=0.9",
	"en-GB,en;q=0.9",
	"de-DE,de;q=0.9",
	"fr-FR,fr;q=0.9",
	"zh-TW,zh;q=0.9",
	"es-ES,es;q=0.9",
	"pt-BR,pt;q=0.9",
}

// defaultFingerprint 默认指纹（与旧版固定头完全一致：Chrome 140 / Windows / zh-CN）。
// 用于无账号身份的场景（代理连通性测试）以及 identity 为空的兼容调用。
func defaultFingerprint() *BrowserFingerprint {
	return &BrowserFingerprint{
		UserAgent:       defaultInviteUA,
		SecChUA:         `"Chromium";v="140", "Google Chrome";v="140", "Not_A Brand";v="99"`,
		SecChUAPlatform: `"Windows"`,
		AcceptLanguage:  "zh-CN,zh;q=0.9",
		ChromeMajor:     "140",
	}
}

// fingerprintFor 从稳定标识派生确定性指纹。
// 标识规范化为小写再哈希，避免大小写差异产生两套指纹。
func fingerprintFor(identity string) *BrowserFingerprint {
	identity = strings.ToLower(strings.TrimSpace(identity))
	if identity == "" {
		return defaultFingerprint()
	}
	sum := sha256.Sum256([]byte(identity))
	ver := fpChromeVersions[int(sum[0])%len(fpChromeVersions)]
	plat := fpPlatforms[int(sum[1])%len(fpPlatforms)]
	lang := fpLanguages[int(sum[2])%len(fpLanguages)]
	return &BrowserFingerprint{
		UserAgent: "Mozilla/5.0 (" + plat.uaFragment + ") AppleWebKit/537.36 (KHTML, like Gecko) Chrome/" +
			ver + ".0.0.0 Safari/537.36",
		SecChUA:         `"Chromium";v="` + ver + `", "Google Chrome";v="` + ver + `", "Not_A Brand";v="99"`,
		SecChUAPlatform: `"` + plat.secChPlatform + `"`,
		AcceptLanguage:  lang,
		ChromeMajor:     ver,
	}
}

// applyFingerprintHeaders 把指纹核心项写入请求头（fp 为 nil 时用默认指纹）。
func applyFingerprintHeaders(h http.Header, fp *BrowserFingerprint) {
	if fp == nil {
		fp = defaultFingerprint()
	}
	h.Set("User-Agent", fp.UserAgent)
	h.Set("Accept-Language", fp.AcceptLanguage)
	h.Set("sec-ch-ua", fp.SecChUA)
	h.Set("sec-ch-ua-mobile", "?0")
	h.Set("sec-ch-ua-platform", fp.SecChUAPlatform)
}

// accountIdentity 账号指纹标识：优先登录邮箱；纯 Cookie 手工账号无邮箱时退回账号 ID。
func accountIdentity(a *Account) string {
	if a == nil {
		return ""
	}
	if s := strings.TrimSpace(a.Email); s != "" {
		return s
	}
	return a.ID
}
