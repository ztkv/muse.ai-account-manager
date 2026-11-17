// status.go — 账号准入状态检测（POST https://muse.ai/api/auth/check + 落地页 vmState）。
// 实测结论（2026-11-16 双账号对照）：
//   auth/check 200                → 准入正常（admitted）
//   auth/check 403 hatchAdmission → 区域受限/等候名单（reason=activation）
//   auth/check 401 reason=session → 登录态失效
//   auth/check 403 + checkpoint_url → 真人验证拦截（challenge）
//   auth/check 403 Forbidden      → CSRF/权限不足，无法判定
//   落地页 vmState=DISABLED        → 账号已被禁用/解除关联（auth/check 不暴露此状态！）
//   落地页 vmState=RUNNING        → VM 正常运行
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

const museAuthCheckURL = "https://muse.ai/api/auth/check"

// vmStateRe 提取落地页 Flight 数据中的 vmState 字段。
// Flight 序列化为 push 参数 JSON 字符串，内层引号被转义，两种形态都兼容：
//   "vmState":"DISABLED"   （解码后）   \"vmState\":\"DISABLED\"  （HTML 源码形态）
var vmStateRe = regexp.MustCompile(`\\?"vmState\\?"\s*:\s*\\?"([A-Z_]+)\\?"`)

// AdmissionResult 单次准入检测结果。
type AdmissionResult struct {
	StatusCode int    `json:"status_code"`
	State      string `json:"state"` // admitted / disabled / region_limited / auth_expired / challenge / forbidden / unknown
	Type       string `json:"type,omitempty"`
	Reason     string `json:"reason,omitempty"`
	VMState    string `json:"vm_state,omitempty"` // 落地页提取的 VM 状态（RUNNING/DISABLED/…）
	Detail     string `json:"detail"`
}

// CheckAdmission 检测账号准入状态（服务端代请求）。
// identity 为账号稳定标识（登录邮箱或 ID），用于派生每账号独立的请求头指纹。
// 代理优先级：accountProxy > globalProxy > MUSE_PROXY > 直连。
// 403 无业务类型时两级重试（实测 2026-11-16：边缘风控动态拦抁无 CSRF 的 POST，
// 换新鲜 token 后稳定通过）：
//   ① 带 Cookie 内 hatch_native_auth_csrf 重试；
//   ② 仍 403 → POST /api/auth/native/restart 换新鲜 csrf_token 带头重试。
func CheckAdmission(cookie, identity, accountProxy, globalProxy string) (*AdmissionResult, error) {
	return checkAdmissionFrom(museAuthCheckURL, cookie, identity, effectiveProxy(accountProxy, globalProxy), 30*time.Second)
}

func checkAdmissionFrom(target, cookie, identity, proxyURL string, timeout time.Duration) (*AdmissionResult, error) {
	if strings.TrimSpace(cookie) == "" {
		return nil, fmt.Errorf("未设置登录 Cookie：请先在编辑账号中粘贴 muse.ai 的 Cookie（含 hatch_sess）")
	}
	client, err := newMuseClient(proxyURL, timeout)
	if err != nil {
		return nil, err
	}
	fp := fingerprintFor(identity)

	do := func(csrfValue string) (int, []byte, error) {
		// 复刻浏览器 prober 形态：裸 POST（无 body、无 Content-Type）+
		// 完整浏览器特征头。实测（2026-11-15）：缺 Sec-Fetch-* 会被 CSRF 防护
		// 拒绝（403 {"error":"Forbidden"}），带上后可拿到真实业务响应。
		req, err := http.NewRequest(http.MethodPost, target, nil)
		if err != nil {
			return 0, nil, err
		}
		h := req.Header
		h.Set("Cookie", cookie)
		applyFingerprintHeaders(h, fp) // 每账号独立 UA/Accept-Language/sec-ch-ua
		h.Set("Accept", "*/*")
		// 不手动设置 Accept-Encoding：手动设置会让 Go 跳过透明 gzip 解压（响应乱码）
		h.Set("Origin", "https://muse.ai")
		h.Set("Referer", "https://muse.ai/")
		h.Set("Sec-Fetch-Dest", "empty")
		h.Set("Sec-Fetch-Mode", "cors")
		h.Set("Sec-Fetch-Site", "same-origin")
		if csrfValue != "" {
			h.Set("X-Hatch-CSRF-Token", csrfValue)
		}
		resp, err := client.Do(req)
		if err != nil {
			return 0, nil, err
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return resp.StatusCode, body, err
	}

	status, body, err := do("")
	if err != nil {
		return nil, hintProxyOnNetErr(fmt.Errorf("请求 muse.ai 失败: %w", err))
	}
	// 纯 403 Forbidden（无业务类型）→ 两级重试：
	// ① Cookie 内 csrf 值；② restart 换新鲜 csrf_token
	if status == http.StatusForbidden && !strings.Contains(string(body), "hatchAdmission") {
		if s2, b2, e2 := do(extractCookieValue(cookie, "hatch_native_auth_csrf")); e2 == nil {
			status, body = s2, b2
		}
		if status == http.StatusForbidden && !strings.Contains(string(body), "hatchAdmission") {
			if base, perr := parseBase(target); perr == nil {
				if fresh := fetchFreshCSRFToken(client, base, cookie, fp); fresh != "" {
					if s3, b3, e3 := do(fresh); e3 == nil {
						status, body = s3, b3
					}
				}
			}
		}
	}

	res := &AdmissionResult{StatusCode: status}
	var parsed map[string]any
	_ = json.Unmarshal(body, &parsed)
	str := func(k string) string { v, _ := parsed[k].(string); return v }

	switch {
	case status == http.StatusOK:
		res.State = "admitted"
		res.Detail = "账号准入正常（admitted）"
		// 追加 VM 状态检查：解除关联/禁用的账号 auth/check 仍返回 200，
		// 但落地页 Flight 数据中 vmState=DISABLED（实测双账号对照验证）。
		if base, perr := parseBase(target); perr == nil {
			if vmState := fetchVMStateFrom(client, base, cookie, fp); vmState != "" {
				res.VMState = vmState
				switch vmState {
				case "DISABLED":
					res.State = "disabled"
					res.Detail = "账号已被禁用/解除关联（vmState=DISABLED）——登录态仍在，但 VM 已停用，无法正常使用"
				case "RUNNING":
					res.Detail = "账号准入正常（admitted · VM RUNNING）"
				default:
					res.Detail = fmt.Sprintf("账号准入正常（admitted · vmState=%s）", vmState)
				}
			}
		}
	case str("type") == "hatchAdmissionInvalidated" && status == http.StatusUnauthorized:
		// 实测：401 + reason=session → 登录会话已失效（服务端会同时清除 hatch_sess）
		res.State = "auth_expired"
		res.Type = "hatchAdmissionInvalidated"
		res.Reason = str("reason")
		res.Detail = "登录态已失效（服务端判定会话无效" + reasonSuffix(res.Reason) + "），请重新登录 muse.ai 并更新 Cookie"
	case status == http.StatusForbidden && str("type") == "hatchAdmissionInvalidated":
		res.State = "region_limited"
		res.Type = "hatchAdmissionInvalidated"
		res.Reason = str("reason")
		res.Detail = "账号准入被撤销（admission invalidated，reason=" + res.Reason + "）——区域受限/等候名单"
	case status == http.StatusUnauthorized:
		res.State = "auth_expired"
		res.Detail = "登录态失效（Cookie 过期或无效），请重新登录并更新 Cookie"
	case status == http.StatusForbidden && str("checkpoint_url") != "":
		// 真人验证拦截（站点前端 iu/iQ 逻辑：错误体携带非空 checkpoint_url
		// 即人机验证挑战，CSRF 重试无法绕过）。状态不映射，避免误改账号状态。
		res.State = "challenge"
		res.Type = "checkpoint"
		res.Detail = "触发真人验证/人机检查（checkpoint_url）：请稍后重试，或在浏览器登录 muse.ai 完成验证后再体检"
	case status == http.StatusForbidden:
		res.State = "forbidden"
		res.Detail = forbiddenDetail(cookie)
	default:
		res.State = "unknown"
		res.Detail = fmt.Sprintf("HTTP %d：%s", status, bodySnippet(body))
	}
	return res, nil
}

// fetchVMStateFrom 请求落地页（GET base/）提取 Flight 数据中的 vmState。
// 实测（2026-11-16）：正常账号 RUNNING；解除关联/禁用账号 DISABLED。
// 失效 Cookie 会被 302 到登录页——不跟随重定向，3xx/非 200 返回空（不覆盖 auth/check 的判定）。
func fetchVMStateFrom(client *http.Client, base, cookie string, fp *BrowserFingerprint) string {
	req, err := http.NewRequest(http.MethodGet, base+"/", nil)
	if err != nil {
		return ""
	}
	h := req.Header
	applyFingerprintHeaders(h, fp) // 每账号独立 UA/Accept-Language/sec-ch-ua
	h.Set("Cookie", cookie)
	h.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	h.Set("Sec-Fetch-Dest", "document")
	h.Set("Sec-Fetch-Mode", "navigate")
	h.Set("Sec-Fetch-Site", "none")
	h.Set("Upgrade-Insecure-Requests", "1")
	oldCR := client.CheckRedirect
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse }
	defer func() { client.CheckRedirect = oldCR }()
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if m := vmStateRe.FindSubmatch(body); m != nil {
		return string(m[1])
	}
	return ""
}

// fetchFreshCSRFToken POST /api/auth/native/restart 换新鲜 csrf_token。
// 实测（2026-11-16）：带登录 Cookie 调 restart 安全（不清除 hatch_sess），
// 201 响应体含 csrf_token；用它带 X-Hatch-CSRF-Token 头重试 auth/check 可穿透
// 边缘风控窗口。失败（网络/非 201/无 token）返回空串，调用方保持原判定。
func fetchFreshCSRFToken(client *http.Client, base, cookie string, fp *BrowserFingerprint) string {
	req, err := http.NewRequest(http.MethodPost, base+"/api/auth/native/restart", strings.NewReader(`{"resume":false}`))
	if err != nil {
		return ""
	}
	h := req.Header
	applyFingerprintHeaders(h, fp) // 每账号独立 UA/Accept-Language/sec-ch-ua
	h.Set("Cookie", cookie)
	h.Set("Accept", "*/*")
	h.Set("Content-Type", "application/json")
	h.Set("Origin", base)
	h.Set("Referer", base+"/")
	h.Set("Sec-Fetch-Dest", "empty")
	h.Set("Sec-Fetch-Mode", "cors")
	h.Set("Sec-Fetch-Site", "same-origin")
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return ""
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var rr struct {
		CSRFToken string `json:"csrf_token"`
	}
	if json.Unmarshal(body, &rr) != nil {
		return ""
	}
	return rr.CSRFToken
}

// forbiddenDetail 403 无法判定时的诊断：检查 Cookie 完整性，给出针对性提示。
func forbiddenDetail(cookie string) string {
	var missing []string
	if extractCookieValue(cookie, "hatch_sess") == "" {
		missing = append(missing, "hatch_sess")
	}
	const howTo = "获取方法：浏览器登录 muse.ai → F12 → 网络 → 任一 muse.ai 请求 → 请求标头 → 复制整行 cookie 值"
	if len(missing) > 0 {
		return "403 Forbidden：Cookie 缺少 " + strings.Join(missing, "、") +
			"。请求被 CSRF/风控拦截。" + howTo
	}
	return "403 Forbidden：请求被 CSRF/风控拦截（已尝试自动刷新 CSRF 重试仍未通过；Cookie 可能已失效或触发风控）。请稍后重试，或重新登录 muse.ai 并更新 Cookie。" + howTo
}

// reasonSuffix 给出可读的失效原因后缀。
func reasonSuffix(reason string) string {
	switch reason {
	case "session":
		return "，reason=session"
	case "":
		return ""
	default:
		return "，reason=" + reason
	}
}

// extractCookieValue 从 Cookie 字符串中取指定名称的值（大小写不敏感）。
func extractCookieValue(cookieStr, name string) string {
	for _, part := range strings.Split(cookieStr, ";") {
		kv := strings.TrimSpace(part)
		if i := strings.Index(kv, "="); i > 0 && strings.EqualFold(kv[:i], name) {
			return kv[i+1:]
		}
	}
	return ""
}
