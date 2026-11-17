// login.go — 存量账号邮箱验证码登录链（服务端代请求，直接获取 Cookie）。
// 实测链路（2026-11-16，临时邮箱真实验证通过）：
//   ① POST /api/auth/native/restart {"resume":false}
//        → 201 {csrf_token}（Set-Cookie 种 hatch_native_auth_* Cookie 组）
//   ② POST /api/auth/native/send-otp {"contact_point":email}
//        → 201 {registration_state:"otp_sent", flow:"login", has_muse_account:bool}
//   ③ POST /api/auth/native/confirm-otp
//        {"otp_code","remember_device":true,"supports_muse_link_transparency":true}
//        → 200 {redirect_to, auth_meta_session:{frl_access_token, frl_account_id}}
//        特殊分支（2026-11-16 区域受限账号实测）：返回
//        {registration_state:"account_selection_required", accounts:[{account_id,...}]}
//        → ③b POST /api/auth/native/select-account {"account_id":...}（自动选号）
//           → 200 {auth_meta_session:{...}}（与 ③ 成功形态相同）
//   ④ POST /api/auth/save-account {} → 200 {saved:true}（Set-Cookie 下发 hatch_sess）
//   ⑤ GET /api/hatch/invite → 200 即登录态有效（验证用）。
// 关键点：所有请求必须带 Sec-Fetch-* 浏览器特征头 + X-Hatch-CSRF-Token（restart 响应值）；
// saved-accounts/select-account 是 Cookie 态多账号选择端点，与 ③b 的 native
// select-account 不同，误调会 csrfFailed。
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

const loginSessionTTL = 10 * time.Minute

// LoginResult 登录成功结果。
type LoginResult struct {
	Cookie       string `json:"cookie"`         // 完整 Cookie 串（name=value; ...）
	Email        string `json:"email"`
	AccountID    string `json:"account_id,omitempty"` // frl_account_id
	HatchSessLen int    `json:"hatch_sess_len"`
}

// nativeLoginConfirm confirm-otp / select-account 的共通响应形态。
type nativeLoginConfirm struct {
	AuthMetaSession struct {
		FrlAccessToken string `json:"frl_access_token"`
		FrlAccountID   string `json:"frl_account_id"`
	} `json:"auth_meta_session"`
	RedirectTo        string          `json:"redirect_to"`
	RegistrationState string          `json:"registration_state"`
	Accounts          []nativeAccount `json:"accounts"`
	CheckpointURL     string          `json:"checkpoint_url"` // 真人验证挑战页（非空即被人机验证拦截）
}

// nativeAccount account_selection_required 响应中的待选账号。
type nativeAccount struct {
	AccountID     string `json:"account_id"`
	AccountStatus string `json:"account_status"`
}

// pickAccount 自动选择规则：优先状态正常的账号（非 deactivated/scheduled_for_deletion），
// 否则取第一个 account_id 非空者。均不可用时返回空（调用方报错）。
func (c *nativeLoginConfirm) pickAccount() string {
	if len(c.Accounts) == 0 {
		return ""
	}
	for _, a := range c.Accounts {
		if a.AccountID == "" {
			continue
		}
		if a.AccountStatus != "deactivated" && a.AccountStatus != "scheduled_for_deletion" {
			return a.AccountID
		}
	}
	for _, a := range c.Accounts {
		if a.AccountID != "" {
			return a.AccountID
		}
	}
	return ""
}

// loginSession 一次登录流程的会话状态（内存，TTL 过期）。
// fp 为该次登录的请求头指纹：以登录邮箱为标识派生，登录成功后保存的
// Cookie 由同一指纹的后续请求使用，保持会话前后一致性。
type loginSession struct {
	mu       sync.Mutex
	base     string
	client   *http.Client
	jar      map[string]string
	email    string
	fp       *BrowserFingerprint
	csrf     string
	proxy    string
	created  time.Time
	lastSent time.Time
	done     bool
	result   *LoginResult
}

func (s *loginSession) header() string {
	parts := make([]string, 0, len(s.jar))
	for k, v := range s.jar {
		parts = append(parts, k+"="+v)
	}
	return strings.Join(parts, "; ")
}

// loginRequest 登录链请求（浏览器特征头 + CSRF + Cookie jar）。
func (s *loginSession) loginRequest(method, path, body string, withCSRF bool) (int, []byte, error) {
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, s.base+path, rd)
	if err != nil {
		return 0, nil, err
	}
	museBrowserHeadersFP(req.Header, museBaseURL, false, s.fp) // 每账号独立指纹
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if hdr := s.header(); hdr != "" {
		req.Header.Set("Cookie", hdr)
	}
	if withCSRF && s.csrf != "" {
		req.Header.Set("X-Hatch-CSRF-Token", s.csrf)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	for _, c := range resp.Cookies() {
		s.jar[c.Name] = c.Value
	}
	return resp.StatusCode, b, nil
}

// loginSessions 全局会话池（TTL 清理惰性执行）。
type loginSessions struct {
	mu       sync.Mutex
	base     string
	sessions map[string]*loginSession
}

func newLoginSessions() *loginSessions {
	return &loginSessions{base: museBaseURL, sessions: map[string]*loginSession{}}
}

// newLoginSessionsWithBase 可注入 base 地址（测试用）。
func newLoginSessionsWithBase(base string) *loginSessions {
	return &loginSessions{base: base, sessions: map[string]*loginSession{}}
}

// cleanup 清理过期会话（创建超 TTL 或完成超 5 分钟）。
func (p *loginSessions) cleanup() {
	now := time.Now()
	for id, s := range p.sessions {
		limit := s.created.Add(loginSessionTTL)
		if s.done {
			limit = s.lastSent.Add(5 * time.Minute)
		}
		if now.After(limit) {
			delete(p.sessions, id)
		}
	}
}

// LoginSendOTP 登录第一步：restart + send-otp。返回 (会话ID, has_muse_account, 错误)。
func (p *loginSessions) LoginSendOTP(email, proxyURL string) (string, *bool, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" || !strings.Contains(email, "@") {
		return "", nil, fmt.Errorf("请填写有效的邮箱地址")
	}
	proxyURL = effectiveProxy(proxyURL, "")
	client, err := newMuseClient(proxyURL, 30*time.Second)
	if err != nil {
		return "", nil, err
	}
	s := &loginSession{
		base:    p.base,
		client:  client,
		jar:     map[string]string{},
		email:   email,
		fp:      fingerprintFor(email), // 指纹随邮箱派生：同邮箱每次登录同指纹
		proxy:   proxyURL,
		created: time.Now(),
	}

	// ① restart
	st, body, rerr := s.loginRequest("POST", "/api/auth/native/restart", `{"resume":false}`, false)
	if rerr != nil {
		return "", nil, hintProxyOnNetErr(fmt.Errorf("请求 muse.ai 失败: %w", rerr))
	}
	if st != http.StatusCreated && st != http.StatusOK {
		return "", nil, fmt.Errorf("restart 返回 HTTP %d：%s", st, bodySnippet(body))
	}
	var rr struct {
		CSRFToken string `json:"csrf_token"`
	}
	if json.Unmarshal(body, &rr) != nil || rr.CSRFToken == "" {
		return "", nil, fmt.Errorf("restart 未返回 csrf_token（响应可能被拦截）: %s", bodySnippet(body))
	}
	s.csrf = rr.CSRFToken

	// ② send-otp
	otpPayload, _ := json.Marshal(map[string]any{"contact_point": email})
	st, body, rerr = s.loginRequest("POST", "/api/auth/native/send-otp", string(otpPayload), true)
	if rerr != nil {
		return "", nil, hintProxyOnNetErr(fmt.Errorf("请求 muse.ai 失败: %w", rerr))
	}
	if st != http.StatusOK && st != http.StatusCreated {
		var er struct {
			Type   string `json:"type"`
			Detail string `json:"detail"`
			Title  string `json:"title"`
		}
		msg := bodySnippet(body)
		if json.Unmarshal(body, &er) == nil && (er.Detail != "" || er.Title != "") {
			msg = er.Title + "：" + er.Detail
			if er.Type == "rateLimitExceeded" || strings.Contains(strings.ToLower(er.Type), "rate") {
				msg += "（发送过于频繁，请稍后再试）"
			}
		}
		return "", nil, fmt.Errorf("验证码发送失败（HTTP %d）：%s", st, msg)
	}
	var sr struct {
		HasMuseAccount bool `json:"has_muse_account"`
	}
	hasAccount := false
	if json.Unmarshal(body, &sr) == nil {
		hasAccount = sr.HasMuseAccount
	}
	s.lastSent = time.Now()

	id := uuidv4()
	p.mu.Lock()
	p.cleanup()
	p.sessions[id] = s
	p.mu.Unlock()
	return id, &hasAccount, nil
}

// LoginVerify 登录第二步：confirm-otp + save-account，返回完整 Cookie。
func (p *loginSessions) LoginVerify(sessionID, code string) (*LoginResult, error) {
	code = strings.TrimSpace(code)
	if len(code) != 6 {
		return nil, fmt.Errorf("请输入 6 位验证码")
	}
	p.mu.Lock()
	s := p.sessions[sessionID]
	p.mu.Unlock()
	if s == nil {
		return nil, fmt.Errorf("登录会话不存在或已过期，请重新发送验证码")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done && s.result != nil {
		return s.result, nil
	}

	// ③ confirm-otp（关键字段 supports_muse_link_transparency 必须为 true）
	payload, _ := json.Marshal(map[string]any{
		"otp_code":                        code,
		"remember_device":                 true,
		"supports_muse_link_transparency": true,
	})
	st, body, rerr := s.loginRequest("POST", "/api/auth/native/confirm-otp", string(payload), true)
	if rerr != nil {
		return nil, hintProxyOnNetErr(fmt.Errorf("请求 muse.ai 失败: %w", rerr))
	}
	if st != http.StatusOK {
		// 真人验证拦截（站点前端 iQ/iu 逻辑：错误响应体可携带 checkpoint_url，
		// 含非空值即需人机验证，重试无法绕过）
		var ec struct {
			CheckpointURL string `json:"checkpoint_url"`
		}
		_ = json.Unmarshal(body, &ec)
		if ec.CheckpointURL != "" {
			return nil, fmt.Errorf("登录被真人验证拦截（checkpoint，HTTP %d）：请在浏览器打开 muse.ai 完成人机验证后重试", st)
		}
		var er struct {
			Type   string `json:"type"`
			Detail string `json:"detail"`
			Title  string `json:"title"`
		}
		msg := bodySnippet(body)
		if json.Unmarshal(body, &er) == nil && (er.Detail != "" || er.Title != "") {
			msg = er.Title + "：" + er.Detail
			switch {
			case strings.Contains(strings.ToLower(er.Type), "expired"):
				msg = "验证码已过期，请重新发送"
			case strings.Contains(strings.ToLower(er.Type), "otp") || strings.Contains(strings.ToLower(er.Type), "code"):
				msg = "验证码错误，请核对后重试"
			}
		}
		return nil, fmt.Errorf("验证码校验失败（HTTP %d）：%s", st, msg)
	}
	var cr nativeLoginConfirm
	_ = json.Unmarshal(body, &cr)
	// 200 也可能携带 checkpoint_url（站点前端 iQ：完成态响应含 checkpoint_url
	// 时需先过人机验证才能落地会话，无 waterfallId 直接 blocked）
	if cr.CheckpointURL != "" {
		return nil, fmt.Errorf("登录被真人验证拦截（checkpoint）：请在浏览器打开 muse.ai 完成人机验证后重试")
	}

	// ③b 账号选择步（实测 2026-11-16 特殊状态账号）：confirm-otp 返回
	// registration_state=account_selection_required（邮箱下存在多个/特殊状态账号档案）时，
	// 需 POST /api/auth/native/select-account {"account_id":...}（带 CSRF）后才能拿到
	// 会话令牌。单账号自动选号；多账号取首个状态正常者。
	if cr.AuthMetaSession.FrlAccessToken == "" && cr.RegistrationState == "account_selection_required" {
		accID := cr.pickAccount()
		if accID == "" {
			return nil, fmt.Errorf("登录需选择账号但响应未包含可用账号（accounts=%d），请在浏览器中完成选择后更新 Cookie", len(cr.Accounts))
		}
		selPayload, _ := json.Marshal(map[string]any{"account_id": accID})
		st, body, rerr = s.loginRequest("POST", "/api/auth/native/select-account", string(selPayload), true)
		if rerr != nil {
			return nil, hintProxyOnNetErr(fmt.Errorf("请求 muse.ai 失败: %w", rerr))
		}
		if st != http.StatusOK {
			return nil, fmt.Errorf("select-account 返回 HTTP %d（account_id=%s）：%s", st, accID, bodySnippet(body))
		}
		_ = json.Unmarshal(body, &cr)
		if cr.CheckpointURL != "" {
			return nil, fmt.Errorf("选号后被真人验证拦截（checkpoint）：请在浏览器打开 muse.ai 完成人机验证后重试")
		}
	}
	if cr.AuthMetaSession.FrlAccessToken == "" {
		return nil, fmt.Errorf("confirm-otp 未返回会话令牌（响应: %s）", bodySnippet(body))
	}

	// ④ save-account（下发 hatch_sess）
	st, body, rerr = s.loginRequest("POST", "/api/auth/save-account", "{}", true)
	if rerr != nil {
		return nil, hintProxyOnNetErr(fmt.Errorf("请求 muse.ai 失败: %w", rerr))
	}
	if st != http.StatusOK {
		return nil, fmt.Errorf("save-account 返回 HTTP %d：%s", st, bodySnippet(body))
	}
	sess := s.jar["hatch_sess"]
	if sess == "" {
		return nil, fmt.Errorf("登录链完成但未捕获 hatch_sess Cookie，请重试")
	}
	res := &LoginResult{
		Cookie:       s.header(),
		Email:        s.email,
		AccountID:    cr.AuthMetaSession.FrlAccountID,
		HatchSessLen: len(sess),
	}
	s.done = true
	s.result = res
	return res, nil
}
