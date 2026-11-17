// invite.go — 拉取 muse.ai 邀请信息（两点关键实测行为，2026-11-16/17）：
//  1. 邀请码首次需要「刮开」：GET /api/hatch/invite 只返回 invite_state（ELIGIBLE 且
//     code 为 null），必须再 POST /api/hatch/invite（空 body）才会生成并返回邀请码。
//     浏览器端「点一下刮开」对应的就是这个 POST。
//  2. GET 返回的 use_count 为陈旧缓存（实测 sedi 快照 2 次、POST 实时 15 次），
//     因此码/计数一律以 POST 结果为准（POST 响应含 code/invite_link/max_uses/
//     use_count/share_text）。
// also: has_redeemed_invite_code 字段实测不可靠（全部账号返回 false），仅作参考。
// 流程：GET publishable-key（失败不阻断）→ GET invite（展示信息）→ POST invite
// （刮码/刷新实时计数；GET 已有码时失败可静默降级）→ 合并快照。
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	museBaseURL   = "https://muse.ai"
	musePubKeyURL = museBaseURL + "/api/checkout/stripe/publishable-key"
	museInviteURL = museBaseURL + "/api/hatch/invite"
)

// defaultInviteUA 模拟浏览器 UA，避免被简单拦截。
const defaultInviteUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36"

// MuseInvite /api/hatch/invite（GET/POST 合并后）关心的字段。
type MuseInvite struct {
	InviteState           string `json:"invite_state"`
	Code                  string `json:"code"`
	InviteLink            string `json:"invite_link"`
	MaxUses               int    `json:"max_uses"`
	UseCount              int    `json:"use_count"`
	UsesRemaining         int    `json:"uses_remaining"`
	ShareText             string `json:"share_text"`
	HasRedeemedInviteCode bool   `json:"has_redeemed_invite_code"` // 注意：实测不可靠，仅参考
	Title                 string `json:"title"`
	Subtitle              string `json:"subtitle"`
	PublishableKey        string `json:"publishable_key,omitempty"`
	Subscription          *MuseSubscription `json:"subscription,omitempty"` // 订阅/加购额度（同链拉取，已兑换判定信号）
	Reward                struct {
		Title string `json:"title"`
		Body  string `json:"body"`
	} `json:"reward"`
}

// InviteSummary 供 CLI/日志使用的单行摘要。
func (inv *MuseInvite) Summary() string {
	return fmt.Sprintf("邀请码 %s | 剩余 %d/%d 次 | %s", inv.Code, inv.UsesRemaining, inv.MaxUses, inv.Reward.Title)
}

// FetchInvite 完整拉取流程（GET 展示信息 → POST 刮码/实时计数）。
// identity 为账号稳定标识（登录邮箱或 ID），用于派生每账号独立的请求头指纹。
// 代理优先级：accountProxy > globalProxy > MUSE_PROXY > 直连。
// 返回 (Stripe 公钥, 邀请解析结果, 合并后的快照 JSON, 错误)。
func FetchInvite(cookie, identity, accountProxy, globalProxy string) (string, *MuseInvite, json.RawMessage, error) {
	return fetchInviteFullFrom(museBaseURL, cookie, identity, effectiveProxy(accountProxy, globalProxy), 20*time.Second)
}

// fetchInviteFullFrom 可注入 base 地址（便于测试）。
// 返回语义：出错时仍尽量返回 inv 与快照（便于 UI 展示 invite_state 等状态），
// 调用方应优先检查 err。
func fetchInviteFullFrom(base, cookie, identity, proxyURL string, timeout time.Duration) (string, *MuseInvite, json.RawMessage, error) {
	if strings.TrimSpace(cookie) == "" {
		return "", nil, nil, fmt.Errorf("未设置登录 Cookie：请先在编辑账号中粘贴 muse.ai 的 Cookie（含 hatch_sess）")
	}
	client, err := newMuseClient(proxyURL, timeout)
	if err != nil {
		return "", nil, nil, err
	}
	fp := fingerprintFor(identity)

	// ---- 第 1 步：前置获取 Stripe publishable key（失败不阻断） ----
	pubKey := ""
	if pubBody, pubErr := doGet(client, base+"/api/checkout/stripe/publishable-key", cookie, fp); pubErr == nil {
		pubKey = extractPublishableKey(pubBody)
	}

	// ---- 第 2 步：GET 邀请信息（展示文案/状态/奖励等） ----
	getStatus, body, gerr := doGetStatus(client, base+"/api/hatch/invite", cookie, fp)
	if gerr != nil {
		return pubKey, nil, nil, hintProxyOnNetErr(gerr)
	}

	// 非 200：按状态码分类报错（401 登录态失效 / 403 真人验证或风控）
	if getStatus != http.StatusOK {
		return pubKey, nil, nil, classifyInviteHTTPError(base, getStatus, body)
	}

	var inv MuseInvite
	if err := json.Unmarshal(body, &inv); err != nil {
		return pubKey, nil, nil, fmt.Errorf("响应不是有效 JSON（可能被拦截页跳转）: %s", bodySnippet(body))
	}

	snapshot := map[string]any{}
	_ = json.Unmarshal(body, &snapshot)
	inv.Subscription = fetchSubscriptionBestEffort(client, base, cookie, identity, snapshot)

	// 明确不可邀请状态：直接给出分类错误（实测 EXHAUSTED 账号 POST 也返回 502）
	if inv.Code == "" && (inv.InviteState == "EXHAUSTED" || inv.InviteState == "NOT_ELIGIBLE" || inv.InviteState == "INELIGIBLE") {
		msg := fmt.Sprintf("该账号当前不可邀请（invite_state=%s）", inv.InviteState)
		if inv.InviteState == "EXHAUSTED" {
			msg = "邀请额度已用尽（invite_state=EXHAUSTED），无可刮开的邀请码"
		}
		if pubKey != "" {
			snapshot["publishable_key"] = pubKey
		}
		return pubKey, &inv, rawOrNil(snapshot), fmt.Errorf("%s", msg)
	}

	// ---- 第 3 步：POST 刮码 / 刷新实时计数（以 POST 结果为准） ----
	postStatus, postBody, perr := doPostJSON(client, base+"/api/hatch/invite", cookie, "", fp)
	switch {
	case perr != nil:
		// 网络错误：GET 已有码则保留展示数据；否则报错
		if inv.Code == "" {
			return pubKey, &inv, nil, hintProxyOnNetErr(fmt.Errorf("刮码请求失败: %w", perr))
		}
	case postStatus == http.StatusOK:
		var post struct {
			Code       string `json:"code"`
			InviteLink string `json:"invite_link"`
			MaxUses    int    `json:"max_uses"`
			UseCount   int    `json:"use_count"`
			ShareText  string `json:"share_text"`
		}
		if json.Unmarshal(postBody, &post) == nil {
			// 合并进快照（POST 为实时数据，覆盖 GET 的陈旧值）
			var pm map[string]any
			if json.Unmarshal(postBody, &pm) == nil {
				for k, v := range pm {
					snapshot[k] = v
				}
			}
			if post.Code != "" {
				inv.Code = post.Code
			}
			if post.InviteLink != "" {
				inv.InviteLink = post.InviteLink
			}
			if post.MaxUses > 0 {
				inv.MaxUses = post.MaxUses
			}
			// use_count 允许为 0（新码），POST 成功即视为可信实时值
			inv.UseCount = post.UseCount
			inv.UsesRemaining = post.MaxUses - post.UseCount
			if inv.UsesRemaining < 0 {
				inv.UsesRemaining = 0
			}
			if post.ShareText != "" {
				inv.ShareText = post.ShareText
			}
		}
	default:
		// POST 失败（如 502 Invite information is unavailable、403 checkpoint）
		if inv.Code == "" {
			// 无码且刮码失败 → 给出明确分类错误（不再是「登录态可能已失效」）
			return pubKey, &inv, rawOrNil(snapshot), classifyInviteHTTPError(base+"/api/hatch/invite", postStatus, postBody)
		}
		// 已有码（POST 仅用于刷新计数失败）→ 静默降级，保留 GET 数据
		snapshot["post_refresh_error"] = fmt.Sprintf("HTTP %d", postStatus)
	}

	// 兜底：码仍为空时给出与状态匹配的明确提示（不再误报登录态失效）
	if inv.Code == "" {
		switch inv.InviteState {
		case "EXHAUSTED":
			return pubKey, &inv, rawOrNil(snapshot), fmt.Errorf("邀请额度已用尽（invite_state=EXHAUSTED），无可刮开的邀请码")
		case "NOT_ELIGIBLE", "INELIGIBLE":
			return pubKey, &inv, rawOrNil(snapshot), fmt.Errorf("该账号当前不可邀请（invite_state=%s）", inv.InviteState)
		default:
			return pubKey, &inv, rawOrNil(snapshot), fmt.Errorf("刮码后仍未返回邀请码（invite_state=%s）：可能触发风控，请稍后重试", inv.InviteState)
		}
	}

	// ---- 快照合并：invite 原始 JSON + POST 实时字段 + publishable_key ----
	// 次数三件套统一写为计算后的值（GET 的 uses_remaining 可能为 null，
	// POST 只返回 use_count/max_uses；前端直接消费避免出现 null）。
	if inv.MaxUses > 0 || inv.UseCount > 0 {
		snapshot["max_uses"] = inv.MaxUses
		snapshot["use_count"] = inv.UseCount
		snapshot["uses_remaining"] = inv.UsesRemaining
	}
	if pubKey != "" {
		snapshot["publishable_key"] = pubKey
	}
	merged, merr := json.Marshal(snapshot)
	if merr != nil {
		merged = body
	}
	return pubKey, &inv, json.RawMessage(merged), nil
}

func rawOrNil(m map[string]any) json.RawMessage {
	if len(m) == 0 {
		return nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil
	}
	return json.RawMessage(b)
}

// classifyInviteHTTPError 把邀请接口的 HTTP 失败分类为可操作的中文错误。
func classifyInviteHTTPError(target string, status int, body []byte) error {
	var eb struct {
		Type          string `json:"type"`
		Detail        string `json:"detail"`
		Error         string `json:"error"`
		CheckpointURL string `json:"checkpoint_url"`
	}
	_ = json.Unmarshal(body, &eb)
	switch {
	case status == http.StatusUnauthorized || (status == http.StatusForbidden && eb.Type == "sessionExpired"):
		return fmt.Errorf("登录 Cookie 已失效（HTTP %d）：请在「编辑」中重新登录/更新 Cookie", status)
	case eb.CheckpointURL != "" || eb.Type == "checkpointRequired":
		return fmt.Errorf("触发真人验证（checkpoint）：请先在浏览器打开 muse.ai 完成人机验证后再试")
	case status == http.StatusBadGateway && strings.Contains(strings.ToLower(eb.Error), "invite information is unavailable"):
		return fmt.Errorf("邀请信息暂不可用（服务端 502）：该账号邀请额度可能已用尽，或触发风控，请稍后重试")
	case status == http.StatusForbidden && (eb.Type == "hatchAdmissionInvalidated" || strings.TrimSpace(string(body)) == `{"error":"Forbidden"}`):
		return fmt.Errorf("请求被拒绝（403 Forbidden）：该账号可能区域受限/被风控拦截，建议先执行一次「🩺 体检」确认账号状态")
	case status == http.StatusTooManyRequests:
		return fmt.Errorf("请求过于频繁（HTTP 429）：请稍后重试")
	}
	return fmt.Errorf("%s 返回 HTTP %d: %s", target, status, bodySnippet(body))
}

// doGet 执行带 Cookie 的 GET 请求并读取响应体（≤4MB），请求头按账号指纹独立。
// 非 200 视为错误（兼容旧调用方）。
func doGet(client *http.Client, target, cookie string, fp *BrowserFingerprint) ([]byte, error) {
	status, body, err := doGetStatus(client, target, cookie, fp)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("%s 返回 HTTP %d（Cookie 可能已过期）: %s", target, status, bodySnippet(body))
	}
	return body, nil
}

// doGetStatus 返回状态码与响应体（供调用方自定义错误分类）。
func doGetStatus(client *http.Client, target, cookie string, fp *BrowserFingerprint) (int, []byte, error) {
	req, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Cookie", cookie)
	applyFingerprintHeaders(req.Header, fp) // 每账号独立 UA/Accept-Language/sec-ch-ua
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Referer", "https://muse.ai/")

	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("请求 %s 失败: %w", target, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return 0, nil, fmt.Errorf("读取响应失败: %w", err)
	}
	return resp.StatusCode, body, nil
}

// doPostJSON 执行带 Cookie 的 POST（body 可为空），返回状态码与响应体。
func doPostJSON(client *http.Client, target, cookie, body string, fp *BrowserFingerprint) (int, []byte, error) {
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(http.MethodPost, target, rd)
	if err != nil {
		return 0, nil, err
	}
	h := req.Header
	h.Set("Cookie", cookie)
	applyFingerprintHeaders(h, fp)
	h.Set("Accept", "*/*")
	h.Set("Origin", "https://muse.ai")
	h.Set("Referer", "https://muse.ai/")
	h.Set("Sec-Fetch-Dest", "empty")
	h.Set("Sec-Fetch-Mode", "cors")
	h.Set("Sec-Fetch-Site", "same-origin")
	if body != "" {
		h.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("请求 %s 失败: %w", target, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return 0, nil, fmt.Errorf("读取响应失败: %w", err)
	}
	return resp.StatusCode, b, nil
}

// extractPublishableKey 宽松提取 pk_ 开头的 Stripe 可发布密钥。
// 兼容纯文本与多种 JSON 字段名；找不到返回空串。
func extractPublishableKey(body []byte) string {
	s := strings.TrimSpace(string(body))
	if !strings.HasPrefix(s, "{") && strings.HasPrefix(s, "pk_") {
		return s
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return ""
	}
	for _, k := range []string{"publishable_key", "publishableKey", "public_key", "publicKey", "key", "stripe_key"} {
		if v, ok := m[k].(string); ok && strings.HasPrefix(v, "pk_") {
			return v
		}
	}
	for _, v := range m {
		if sv, ok := v.(string); ok && strings.HasPrefix(sv, "pk_") {
			return sv
		}
	}
	return ""
}

func bodySnippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 120 {
		s = s[:120] + "…"
	}
	return s
}