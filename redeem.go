// redeem.go — 邀请码兑换全链（wake → stateful-sharing → redeem → 取回自己的码）。
// 协议要点（逆向站点前端 + musepy/muse-protocol 对照，2026-11-15 实测验证）：
//   ① 所有请求必须带 Sec-Fetch-* 浏览器特征头，否则边缘层直接 403 Forbidden
//     （纯 Go 标准库即可，无需 TLS 指纹伪装——restart 已实测 201）。
//   ② 兑换前必须先 GET /?aymh_complete=1 从 HTML 提取 vmId，并 POST /api/hatch/vm/wake
//     （body: vm_id + retry_count:0 + connect_attempt_id(uuid) + wake_trigger:"mount"
//      + open_id(新 uuid) + away_ms:0）；不唤醒会让 redeem 把有效码误报成 invalid。
//   ③ redeem body: {"code":.., "supportsRedemptionStatus":true}
//      失败 reason: already_redeemed（本账号已兑过）/ used_up（码达 30 次上限）/ invalid。
//   ④ 取回自己的码：POST /api/hatch/invite（空 body）→ {code}。
package main

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// vmIDRe 兼容 `vmId":"uuid`、`vmId\":\"uuid`、`vmId:uuid` 等序列化形态。
var vmIDRe = regexp.MustCompile(`vmId[\\"'\s:]*([0-9a-fA-F-]{36})`)

// RedeemOutcome 兑换链执行结果。
type RedeemOutcome struct {
	VMID    string `json:"vm_id,omitempty"`          // 唤醒的 VM ID
	State   string `json:"state"`                     // redeemed/already_redeemed/used_up/invalid/auth_expired/http-<code>/error
	OwnCode string `json:"own_code,omitempty"`        // 兑换成功后取回的本账号邀请码
	Reason  string `json:"reason,omitempty"`          // 服务端原始 reason
	Detail  string `json:"detail"`                    // 中文说明
}

// RedeemInviteCode 对外入口。identity 为账号稳定标识（登录邮箱或 ID），
// 用于派生每账号独立的请求头指纹。代理优先级：账号代理 > 全局设置 > MUSE_PROXY > 直连。
func RedeemInviteCode(cookie, identity, code, accountProxy, globalProxy string) (*RedeemOutcome, error) {
	return redeemChainFrom(museBaseURL, cookie, code, identity, effectiveProxy(accountProxy, globalProxy), 30*time.Second)
}

// redeemChainFrom 可注入 base 地址（便于测试）。
func redeemChainFrom(base, cookie, code, identity, proxyURL string, timeout time.Duration) (*RedeemOutcome, error) {
	if strings.TrimSpace(cookie) == "" {
		return nil, fmt.Errorf("未设置登录 Cookie：请先在编辑账号中粘贴 muse.ai 的 Cookie（含 hatch_sess）")
	}
	code = strings.ToUpper(strings.TrimSpace(code))
	if code == "" {
		return nil, fmt.Errorf("邀请码不能为空")
	}
	client, err := newMuseClient(proxyURL, timeout)
	if err != nil {
		return nil, err
	}
	fp := fingerprintFor(identity)
	out := &RedeemOutcome{}

	// ---- 1. 唤醒前置：GET /?aymh_complete=1 提取 vmId（不跟随重定向，
	//      失效 Cookie 会被登录重定向循环卡死，直接给出真实原因） ----
	pageBody, pageStatus, perr := fetchVMPage(client, base+"/?aymh_complete=1", cookie, fp)
	if perr != nil {
		return nil, hintProxyOnNetErr(fmt.Errorf("请求 muse.ai 失败: %w", perr))
	}
	if pageStatus != http.StatusOK {
		out.State = "error"
		out.Detail = fmt.Sprintf("落地页返回 HTTP %d（未跟随重定向；登录态大概率已失效，请重新登录并更新 Cookie）", pageStatus)
		return out, nil
	}
	vmID := ""
	if m := vmIDRe.FindSubmatch(pageBody); m != nil {
		vmID = string(m[1])
	}
	if vmID == "" {
		out.State = "error"
		out.Detail = "落地页未解析到 vmId——Cookie 登录态大概率已失效，请重新登录并更新 Cookie"
		return out, nil
	}
	out.VMID = vmID

	// ---- 2. vm/wake（协议语义：尽力而为，不校验状态码） ----
	wakePayload, _ := json.Marshal(map[string]any{
		"vm_id":              vmID,
		"retry_count":        0,
		"connect_attempt_id": uuidv4(),
		"wake_trigger":       "mount",
		"open_id":            uuidv4(),
		"away_ms":            0,
	})
	_, _, _ = musePostJSON(client, base+"/api/hatch/vm/wake", cookie, string(wakePayload), fp)

	// ---- 3. stateful-sharing（空 body，尽力而为） ----
	_, _, _ = musePostJSON(client, base+"/api/hatch/stateful-sharing", cookie, "", fp)

	// ---- 4. redeem ----
	redeemPayload, _ := json.Marshal(map[string]any{"code": code, "supportsRedemptionStatus": true})
	rBody, rStatus, rerr := musePostJSON(client, base+"/api/hatch/invite-code/redeem", cookie, string(redeemPayload), fp)
	if rerr != nil {
		return nil, hintProxyOnNetErr(fmt.Errorf("请求 muse.ai 失败: %w", rerr))
	}
	var parsed struct {
		Success          bool   `json:"success"`
		Reason            string `json:"reason"`
		Detail            string `json:"detail"`
		RedemptionStatus  string `json:"redemptionStatus"`
		Type              string `json:"type"`
	}
	_ = json.Unmarshal(rBody, &parsed)
	out.Reason = parsed.Reason

	switch {
	case rStatus == http.StatusOK && (parsed.Success || parsed.RedemptionStatus == "redeemed" || strings.Contains(string(rBody), `"success":true`)):
		out.State = "redeemed"
		// ---- 5. 取回自己的码 ----
		oBody, oStatus, _ := musePostJSON(client, base+"/api/hatch/invite", cookie, "", fp)
		if oStatus == http.StatusOK {
			var d struct {
				Code string `json:"code"`
			}
			if json.Unmarshal(oBody, &d) == nil && d.Code != "" {
				out.OwnCode = d.Code
			}
		}
		out.Detail = "兑换成功"
		if out.OwnCode != "" {
			out.Detail += "，本账号自己的邀请码：" + out.OwnCode
		}
	case rStatus == http.StatusUnauthorized:
		out.State = "auth_expired"
		out.Detail = "登录态失效（Cookie 过期或无效），请重新登录并更新 Cookie"
	case rStatus == http.StatusForbidden && parsed.Type == "hatchAdmissionInvalidated":
		out.State = "region_limited"
		out.Detail = "账号准入被撤销（区域受限/等候名单），无法兑换"
	case parsed.Reason != "":
		out.State = parsed.Reason
		out.Detail = redeemReasonDetail(parsed.Reason, parsed.Detail)
	default:
		out.State = fmt.Sprintf("http-%d", rStatus)
		out.Detail = fmt.Sprintf("HTTP %d：%s", rStatus, bodySnippet(rBody))
	}
	return out, nil
}

// redeemReasonDetail 失败 reason → 中文说明。
func redeemReasonDetail(reason, detail string) string {
	switch reason {
	case "already_redeemed":
		return "本账号已经兑换过一个邀请码（每账号限一次）"
	case "used_up":
		return "该邀请码已达到使用上限（默认 30 次）"
	case "invalid":
		d := strings.TrimSpace(detail)
		if d != "" {
			return "邀请码不存在或无效：" + d
		}
		return "邀请码不存在或无效（已先执行 VM 唤醒，排除未唤醒误报）"
	default:
		return "兑换未成功：" + reason
	}
}

// ---------- 底层请求（浏览器特征头，实测必需） ----------

// museBrowserHeaders 设置浏览器特征头（默认指纹，兼容旧调用）。
func museBrowserHeaders(h http.Header, base string, isDocument bool) {
	museBrowserHeadersFP(h, base, isDocument, nil)
}

// museBrowserHeadersFP 按 fp 指纹设置 Sec-Fetch-* 等浏览器特征头（缺失会被边缘层 403）。
// 注意：不手动设置 Accept-Encoding——手动设置会让 Go 跳过透明 gzip 解压，
// 导致响应读出乱码（实测 confirm-otp/落地页均会压缩）。
func museBrowserHeadersFP(h http.Header, base string, isDocument bool, fp *BrowserFingerprint) {
	applyFingerprintHeaders(h, fp) // 每账号独立 UA/Accept-Language/sec-ch-ua
	if isDocument {
		h.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8,application/signed-exchange;v=b3;q=0.7")
		h.Set("Sec-Fetch-Dest", "document")
		h.Set("Sec-Fetch-Mode", "navigate")
		h.Set("Sec-Fetch-Site", "same-origin")
		h.Set("Referer", base+"/")
	} else {
		h.Set("Accept", "*/*")
		h.Set("Sec-Fetch-Dest", "empty")
		h.Set("Sec-Fetch-Mode", "cors")
		h.Set("Sec-Fetch-Site", "same-origin")
		h.Set("Origin", base)
		h.Set("Referer", base+"/")
	}
}

// museGetDoc GET 文档请求（HTML accept + navigate 特征）。
func museGetDoc(client *http.Client, target, cookie string, fp *BrowserFingerprint) ([]byte, int, error) {
	req, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		return nil, 0, err
	}
	base := "https://muse.ai"
	if u, perr := parseBase(target); perr == nil {
		base = u
	}
	museBrowserHeadersFP(req.Header, base, true, fp)
	req.Header.Set("Cookie", cookie)
	return museDo(client, req)
}

// fetchVMPage 取 vmId 落地页，不自动跟随重定向：失效 Cookie 会触发登录重定向循环，
// 302/303/307 即可判定登录态失效，避免卡在 10 跳后被 Go 报成裸 redirect 错误。
func fetchVMPage(client *http.Client, target, cookie string, fp *BrowserFingerprint) ([]byte, int, error) {
	req, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		return nil, 0, err
	}
	base := "https://muse.ai"
	if u, perr := parseBase(target); perr == nil {
		base = u
	}
	museBrowserHeadersFP(req.Header, base, true, fp)
	req.Header.Set("Cookie", cookie)
	// 快照客户端的重定向设置，请求完恢复（互不影响其他调用）
	oldCheckRedirect := client.CheckRedirect
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse // 不跟随，拿第一跳响应
	}
	defer func() { client.CheckRedirect = oldCheckRedirect }()
	body, status, err := museDo(client, req)
	if err != nil {
		return body, status, err
	}
	// 3xx 即登录重定向 → 归一化为 401 语义，上层按登录失效处理
	if status >= 300 && status < 400 {
		return body, http.StatusUnauthorized, nil
	}
	return body, status, nil
}

// musePostJSON POST 请求（JSON 形态；body 为空串时也保持该头集合）。
func musePostJSON(client *http.Client, target, cookie, body string, fp *BrowserFingerprint) ([]byte, int, error) {
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(http.MethodPost, target, rd)
	if err != nil {
		return nil, 0, err
	}
	base := "https://muse.ai"
	if u, perr := parseBase(target); perr == nil {
		base = u
	}
	museBrowserHeadersFP(req.Header, base, false, fp)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Cookie", cookie)
	return museDo(client, req)
}

// museDo 执行请求并读取响应体（≤4MB）。
func museDo(client *http.Client, req *http.Request) ([]byte, int, error) {
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return body, resp.StatusCode, err
	}
	return body, resp.StatusCode, nil
}

// parseBase 从 URL 提取 scheme://host。
func parseBase(target string) (string, error) {
	i := strings.Index(target, "://")
	if i < 0 {
		return "", fmt.Errorf("非绝对 URL: %s", target)
	}
	rest := target[i+3:]
	j := strings.IndexAny(rest, "/?#")
	if j < 0 {
		return target, nil
	}
	return target[:i+3] + rest[:j], nil
}

// uuidv4 生成标准 UUID v4。
func uuidv4() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
