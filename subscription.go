// subscription.go — muse.ai 订阅/加购额度查询（Next.js Server Action）。
// 实测协议（2026-11-17 用户抓包 + 探针真实验证）：
//   POST /  (flight server action)
//   Content-Type: text/plain;charset=UTF-8
//   Accept: text/x-component
//   Next-Action: 40e0249eeae158685b6a8d0c3b203f53dd4f2c7d63   ← 订阅查询 action ID
//   Next-Router-State-Tree: (authenticated)/(shell) 页面树
//   Body: {"a":"$@1","q":"","i":false}                        ← 恰 27 字节，回显在响应首行
//   响应: Flight 流，每行 "N:JSON"；"1:" 行含 {success, subscription:{...}}。
// 关键用途：topupTotal > 0 即账号有加购/邀请奖励额度 —— 这是「已兑换他人邀请码」
// 的可靠判定信号（invite 接口的 has_redeemed_invite_code 字段实测恒为 false，不可信）。
// 注意：action ID 与部署绑定，next 部署后可能变动；失败时不阻断邀请主流程，
// 仅将订阅信息置空（前端显示「未同步」）。
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

// subActionID 订阅查询 Server Action ID（2026-11-17 抓包确认）。
const subActionID = "40e0249eeae158685b6a8d0c3b203f53dd4f2c7d63"

// subActionBody 与浏览器一致的 27 字节请求体（回显在响应 0: 行）。
const subActionBody = `{"a":"$@1","q":"","i":false}`

// subActionStateTree (authenticated)/(shell)/__PAGE__ 页面树（URL 编码形态）。
const subActionStateTree = `%5B%22%22%2C%7B%22children%22%3A%5B%22(authenticated)%22%2C%7B%22children%22%3A%5B%22(shell)%22%2C%7B%22children%22%3A%5B%5B%22path%22%2C%22%22%2C%22oc%22%2Cnull%5D%2C%7B%22children%22%3A%5B%22__PAGE__%22%2C%7B%7D%2Cnull%2Cnull%2C0%5D%7D%2Cnull%2Cnull%2C0%5D%7D%2Cnull%2Cnull%2C0%5D%7D%2Cnull%2Cnull%2C0%5D%7D%2Cnull%2Cnull%2C16%5D`

// MuseSubscription "1:" 行 subscription 对象（扁平化后关心的字段；真实响应 tier/usage 为嵌套）。
type MuseSubscription struct {
	Success          bool   `json:"success,omitempty"`
	SubscriptionType string `json:"subscriptionType,omitempty"` // FREE / PLUS...
	TierCode         string `json:"tierCode,omitempty"`         // HATCH_FREE（从 tier.tierCode 提升）
	TierName         string `json:"tierName,omitempty"`         // 从 tier.name 提升
	TierID           string `json:"tierId,omitempty"`
	IsPaid           bool   `json:"isPaid,omitempty"`
	State            string `json:"state,omitempty"`            // METERED（从 usage.state 提升）
	PercentUsed      int    `json:"percentUsed,omitempty"`      // 从 usage.percentUsed 提升
	QuotaStatus      string `json:"quotaStatus,omitempty"`      // SUFFICIENT
	ResetsAt         int64  `json:"resetsAt,omitempty"`         // unix 秒
	TopupBalance     int64  `json:"topupBalance,omitempty"`     // 加购余额（含邀请奖励）
	TopupTotal       int64  `json:"topupTotal"`                // 加购总额；>0 = 已有他人邀请奖励 → 已兑换（0 也序列化，前端需区分“未同步”与“无额度”，由对象存在性决定）
	TopupEligible    bool   `json:"topupEligible,omitempty"`
	HasRedeemed      bool   `json:"has_redeemed,omitempty"`    // 派生：TopupTotal > 0（序列化给前端）
}

// flightSubLine Flight 流中含 "subscription" 的数据行（前缀 "N:JSON"）。
var flightSubLine = regexp.MustCompile(`(?m)^\d+:(\{.*"subscription".*)$`)

// FetchSubscription 查询账号订阅与加购额度（服务端代请求）。
// identity 派生独立请求头指纹；代理优先级与其它出站链一致。
func FetchSubscription(cookie, identity, accountProxy, globalProxy string) (*MuseSubscription, error) {
	return fetchSubscriptionFrom(museBaseURL, cookie, identity, effectiveProxy(accountProxy, globalProxy), 20*time.Second)
}

// fetchSubscriptionFrom 可注入 base 地址（便于测试）。
func fetchSubscriptionFrom(base, cookie, identity, proxyURL string, timeout time.Duration) (*MuseSubscription, error) {
	if strings.TrimSpace(cookie) == "" {
		return nil, fmt.Errorf("未设置登录 Cookie")
	}
	client, err := newMuseClient(proxyURL, timeout)
	if err != nil {
		return nil, err
	}
	return fetchSubscriptionWithClient(client, base, cookie, identity)
}

// fetchSubscriptionBestEffort 邀请拉取链内尽力查询订阅（复用同一 client 避免重复建连）。
// 成功写入 snapshot["subscription"]；失败写入 snapshot["subscription_error"]；均不返回错误。
func fetchSubscriptionBestEffort(client *http.Client, base, cookie, identity string, snapshot map[string]any) *MuseSubscription {
	sub, err := fetchSubscriptionWithClient(client, base, cookie, identity)
	if err != nil {
		snapshot["subscription_error"] = err.Error()
		return nil
	}
	snapshot["subscription"] = sub
	return sub
}

// fetchSubscriptionWithClient 用已有 client 执行订阅查询并解析。
func fetchSubscriptionWithClient(client *http.Client, base, cookie, identity string) (*MuseSubscription, error) {
	fp := fingerprintFor(identity)

	req, err := http.NewRequest(http.MethodPost, base+"/", strings.NewReader(subActionBody))
	if err != nil {
		return nil, err
	}
	h := req.Header
	h.Set("Cookie", cookie)
	applyFingerprintHeaders(h, fp)
	h.Set("Accept", "text/x-component")
	h.Set("Content-Type", "text/plain;charset=UTF-8")
	h.Set("Next-Action", subActionID)
	h.Set("Next-Router-State-Tree", subActionStateTree)
	h.Set("Origin", base)
	h.Set("Referer", base+"/")
	h.Set("Sec-Fetch-Dest", "empty")
	h.Set("Sec-Fetch-Mode", "cors")
	h.Set("Sec-Fetch-Site", "same-origin")

	resp, err := client.Do(req)
	if err != nil {
		return nil, hintProxyOnNetErr(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusOK {
		var eb struct {
			CheckpointURL string `json:"checkpoint_url"`
		}
		_ = json.Unmarshal(body, &eb)
		if eb.CheckpointURL != "" {
			return nil, fmt.Errorf("订阅查询被真人验证拦截（checkpoint）：请先在浏览器完成人机验证")
		}
		return nil, fmt.Errorf("订阅查询返回 HTTP %d: %s", resp.StatusCode, bodySnippet(body))
	}

	// Flight 流按行解析：找含 subscription 的数据行（可能是 "1:{...}" 或 "0:...1:..." 相邻行）
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		// 剥掉 "N:" 前缀
		if i := strings.IndexByte(line, ':'); i > 0 && i < 4 {
			if isDigits(line[:i]) {
				line = line[i+1:]
			}
		}
		if !strings.Contains(line, `"subscription"`) {
			continue
		}
		j := strings.IndexByte(line, '{')
		if j < 0 {
			continue
		}
		// 真实响应为嵌套结构：tier{tierCode,name,...} / usage{state,percentUsed,...}，
		// 解析后提升为扁平字段（前端与快照直接消费）。
		var outer struct {
			Success bool `json:"success"`
			Subscription struct {
				SubscriptionType string `json:"subscriptionType"`
				Tier struct {
					TierID   string `json:"tierId"`
					Name     string `json:"name"`
					TierCode string `json:"tierCode"`
					IsPaid   bool   `json:"isPaid"`
				} `json:"tier"`
				Usage struct {
					State       string `json:"state"`
					PercentUsed int    `json:"percentUsed"`
					ResetsAt    int64  `json:"resetsAt"`
					QuotaStatus string `json:"quotaStatus"`
				} `json:"usage"`
				TopupBalance  int64 `json:"topupBalance"`
				TopupTotal    int64 `json:"topupTotal"`
				TopupEligible bool  `json:"topupEligible"`
			} `json:"subscription"`
		}
		if err := json.Unmarshal([]byte(line[j:]), &outer); err != nil || outer.Subscription.Tier.TierCode == "" && outer.Subscription.SubscriptionType == "" {
			continue
		}
		s := outer.Subscription
		sub := &MuseSubscription{
			Success:          outer.Success,
			SubscriptionType: s.SubscriptionType,
			TierID:           s.Tier.TierID,
			TierName:         s.Tier.Name,
			TierCode:         s.Tier.TierCode,
			IsPaid:           s.Tier.IsPaid,
			State:            s.Usage.State,
			PercentUsed:      s.Usage.PercentUsed,
			QuotaStatus:      s.Usage.QuotaStatus,
			ResetsAt:         s.Usage.ResetsAt,
			TopupBalance:     s.TopupBalance,
			TopupTotal:       s.TopupTotal,
			TopupEligible:    s.TopupEligible,
		}
		// 派生已兑换信号：有加购/邀请奖励额度即视为已兑换（发放来源只可能是邀请奖励/加购）
		sub.HasRedeemed = sub.TopupTotal > 0
		return sub, nil
	}
	// action ID 失效（部署更新）时 Next 通常返回非 JSON 或空流
	return nil, fmt.Errorf("订阅查询未返回数据（action ID 可能已随部署失效）")
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// formatTopup 供前端/日志展示加购额度（服务端单位）。
func (s *MuseSubscription) FormatTopup() string {
	if s == nil {
		return "未知"
	}
	return fmt.Sprintf("加购/奖励额度余额 %d / 总计 %d（服务端单位）", s.TopupBalance, s.TopupTotal)
}