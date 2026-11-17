// handlers.go — REST API 与内嵌 Web 页面路由。
package main

import (
	"crypto/subtle"
	"embed"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

//go:embed static
var staticFS embed.FS

// Server HTTP 服务。
type Server struct {
	store    *Store
	crypto   *Crypto
	token    string // 访问令牌；为空表示免认证（默认仅绑定 127.0.0.1）
	logins   *loginSessions // 验证码登录会话池（内存）
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeOK(w http.ResponseWriter, data any) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "data": data})
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"ok": false, "error": msg})
}

// AccountView 对外输出的账号视图：敏感字段仅返回掩码。
type AccountView struct {
	ID          string   `json:"id"`
	Email       string   `json:"email"`
	LoginType   string   `json:"login_type"`
	CodeEmail   string   `json:"code_email"`
	Proxy       string   `json:"proxy"`
	InviteCode  string   `json:"invite_code"`
	InviteInfo  string   `json:"invite_info"`
	InviteSyncedAt string `json:"invite_synced_at"`
	CheckInfo   string   `json:"check_info"`
	CheckedAt   string   `json:"checked_at"`
	RedeemInfo  string   `json:"redeem_info"`
	RedeemedAt  string   `json:"redeemed_at"`
	Plan        string   `json:"plan"`
	Status      string   `json:"status"`
	Balance     string   `json:"balance"`
	ExpiresAt   string   `json:"expires_at"`
	Tags        []string `json:"tags"`
	Notes       string   `json:"notes"`
	HasPassword bool     `json:"has_password"`
	HasAPIKey   bool     `json:"has_api_key"`
	HasCookie   bool     `json:"has_cookie"`
	LastUsedAt  string   `json:"last_used_at"`
	CreatedAt   string   `json:"created_at"`
	UpdatedAt   string   `json:"updated_at"`
}

func maskOf(v string) bool { return v != "" }

func toView(a *Account) AccountView {
	return AccountView{
		ID:          a.ID,
		Email:       a.Email,
		LoginType:   a.LoginType,
		CodeEmail:   a.CodeEmail,
		Proxy:       a.Proxy,
		InviteCode:  a.InviteCode,
		InviteInfo:  a.InviteInfo,
		InviteSyncedAt: a.InviteSyncedAt,
		CheckInfo:   a.CheckInfo,
		CheckedAt:   a.CheckedAt,
		RedeemInfo:  a.RedeemInfo,
		RedeemedAt:  a.RedeemedAt,
		Plan:        a.Plan,
		Status:      a.Status,
		Balance:     a.Balance,
		ExpiresAt:   a.ExpiresAt,
		Tags:        a.Tags,
		Notes:       a.Notes,
		HasPassword: maskOf(a.Password),
		HasAPIKey:   maskOf(a.APIKey),
		HasCookie:   maskOf(a.Cookie),
		LastUsedAt:  a.LastUsedAt,
		CreatedAt:   a.CreatedAt,
		UpdatedAt:   a.UpdatedAt,
	}
}

func toViews(list []*Account) []AccountView {
	out := make([]AccountView, 0, len(list))
	for _, a := range list {
		out = append(out, toView(a))
	}
	return out
}

// Routes 构建全部路由。
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("GET /api/health", s.handleHealth)
	mux.HandleFunc("GET /api/config", s.handleConfig)
	mux.HandleFunc("POST /api/login", s.handleLogin)
	mux.HandleFunc("GET /api/accounts", s.auth(s.handleList))
	mux.HandleFunc("POST /api/accounts", s.auth(s.handleCreate))
	mux.HandleFunc("GET /api/accounts/{id}", s.auth(s.handleGet))
	mux.HandleFunc("PUT /api/accounts/{id}", s.auth(s.handleUpdate))
	mux.HandleFunc("DELETE /api/accounts/{id}", s.auth(s.handleDelete))
	mux.HandleFunc("GET /api/accounts/{id}/secret", s.auth(s.handleSecret))
	mux.HandleFunc("POST /api/accounts/{id}/fetch-invite", s.auth(s.handleFetchInvite))
	mux.HandleFunc("POST /api/login/send-otp", s.auth(s.handleLoginSendOTP))
	mux.HandleFunc("POST /api/login/verify", s.auth(s.handleLoginVerify))
	mux.HandleFunc("POST /api/accounts/check-all", s.auth(s.handleCheckAll))
	mux.HandleFunc("POST /api/accounts/fetch-invite-all", s.auth(s.handleFetchInviteAll))
	mux.HandleFunc("POST /api/accounts/redeem-batch", s.auth(s.handleRedeemBatch))
	mux.HandleFunc("POST /api/accounts/{id}/check-status", s.auth(s.handleCheckStatus))
	mux.HandleFunc("POST /api/accounts/{id}/redeem", s.auth(s.handleRedeem))
	mux.HandleFunc("GET /api/settings", s.auth(s.handleGetSettings))
	mux.HandleFunc("PUT /api/settings", s.auth(s.handlePutSettings))
	mux.HandleFunc("POST /api/settings/test-proxy", s.auth(s.handleTestProxy))
	mux.HandleFunc("POST /api/accounts/{id}/touch", s.auth(s.handleTouch))
	mux.HandleFunc("GET /api/export", s.auth(s.handleExport))
	mux.HandleFunc("POST /api/import", s.auth(s.handleImport))
	return s.logMW(mux)
}

func (s *Server) logMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		if r.URL.Path == "/api/health" {
			return
		}
		log.Printf("%s %s (%s)", r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond))
	})
}

// auth 访问令牌校验（支持 X-Auth-Token 头或 ?token= 参数）。
func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.token != "" {
			got := r.Header.Get("X-Auth-Token")
			if got == "" {
				got = r.URL.Query().Get("token")
			}
			if subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) != 1 {
				writeErr(w, http.StatusUnauthorized, "未授权：访问令牌缺失或不正确")
				return
			}
		}
		next(w, r)
	}
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	b, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		http.Error(w, "页面资源缺失", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(b)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeOK(w, map[string]any{"status": "up", "accounts": s.store.Count()})
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	writeOK(w, map[string]any{
		"auth_required": s.token != "",
		"accounts":      s.store.Count(),
		"proxy":         effectiveProxy("", s.store.GetSettings().Proxy),
	})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体格式错误")
		return
	}
	if s.token == "" {
		writeOK(w, map[string]any{"auth_required": false})
		return
	}
	if subtle.ConstantTimeCompare([]byte(strings.TrimSpace(req.Token)), []byte(s.token)) != 1 {
		writeErr(w, http.StatusUnauthorized, "访问令牌不正确")
		return
	}
	writeOK(w, map[string]any{"auth_required": true, "verified": true})
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	writeOK(w, toViews(s.store.List()))
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	a, err := s.store.Get(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "账号不存在")
		return
	}
	writeOK(w, toView(a))
}

// parseAccountInput 校验并解析新增/更新请求体。
type accountInput struct {
	Email      string   `json:"email"`
	LoginType  string   `json:"login_type"`
	CodeEmail  string   `json:"code_email"`
	Password   string   `json:"password"`
	APIKey     string   `json:"api_key"`
	Cookie     string   `json:"cookie"`
	Proxy      string   `json:"proxy"`
	InviteCode string   `json:"invite_code"`
	Plan       string   `json:"plan"`
	Status     string   `json:"status"`
	Balance    string   `json:"balance"`
	ExpiresAt  string   `json:"expires_at"`
	Tags       []string `json:"tags"`
	Notes      string   `json:"notes"`
}

func (in *accountInput) validate() error {
	if strings.TrimSpace(in.Email) == "" {
		return fmt.Errorf("登录邮箱不能为空")
	}
	if in.ExpiresAt != "" {
		if _, err := time.Parse("2006-01-02", in.ExpiresAt); err != nil {
			return fmt.Errorf("到期时间格式应为 YYYY-MM-DD")
		}
	}
	if !validLoginType(strings.TrimSpace(in.LoginType)) {
		return fmt.Errorf("非法登录方式: %s", in.LoginType)
	}
	return nil
}

func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	var in accountInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体格式错误")
		return
	}
	if err := in.validate(); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	a := &Account{
		Email:      in.Email,
		LoginType:  in.LoginType,
		CodeEmail:  in.CodeEmail,
		Password:   s.crypto.Encrypt(in.Password),
		APIKey:     s.crypto.Encrypt(in.APIKey),
		Cookie:     s.crypto.Encrypt(in.Cookie),
		Proxy:      in.Proxy,
		InviteCode: in.InviteCode,
		Plan:       in.Plan,
		Status:     in.Status,
		Balance:    in.Balance,
		ExpiresAt:  in.ExpiresAt,
		Tags:       in.Tags,
		Notes:      in.Notes,
	}
	if a.Status == "" {
		a.Status = "active"
	}
	if a.Plan == "" {
		a.Plan = "free"
	}
	if err := s.store.Add(a); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeOK(w, toView(a))
}

// secretChange 约定："" / 缺省 = 不修改；"__CLEAR__" = 清空；其他 = 设置新值。
const secretClear = "__CLEAR__"

func (s *Server) handleUpdate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var in accountInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体格式错误")
		return
	}
	if err := in.validate(); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	err := s.store.Update(id, func(a *Account) error {
		a.Email = in.Email
		a.LoginType = strings.TrimSpace(in.LoginType)
		a.CodeEmail = in.CodeEmail
		if in.Password == secretClear {
			a.Password = ""
		} else if in.Password != "" {
			a.Password = s.crypto.Encrypt(in.Password)
		}
		if in.APIKey == secretClear {
			a.APIKey = ""
		} else if in.APIKey != "" {
			a.APIKey = s.crypto.Encrypt(in.APIKey)
		}
		if in.Cookie == secretClear {
			a.Cookie = ""
		} else if in.Cookie != "" {
			a.Cookie = s.crypto.Encrypt(in.Cookie)
		}
		a.Proxy = strings.TrimSpace(in.Proxy)
		a.InviteCode = in.InviteCode
		a.Plan = in.Plan
		a.Status = in.Status
		a.Balance = in.Balance
		a.ExpiresAt = in.ExpiresAt
		a.Tags = in.Tags
		a.Notes = in.Notes
		return nil
	})
	switch {
	case err == ErrNotFound:
		writeErr(w, http.StatusNotFound, "账号不存在")
	case err != nil:
		writeErr(w, http.StatusBadRequest, err.Error())
	default:
		a, _ := s.store.Get(id)
		writeOK(w, toView(a))
	}
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.store.Delete(r.PathValue("id")); err != nil {
		writeErr(w, http.StatusNotFound, "账号不存在")
		return
	}
	writeOK(w, map[string]any{"deleted": true})
}

// handleSecret 解密取回单个敏感字段：?field=password|api_key。
func (s *Server) handleSecret(w http.ResponseWriter, r *http.Request) {
	a, err := s.store.Get(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "账号不存在")
		return
	}
	field := r.URL.Query().Get("field")
	var raw string
	switch field {
	case "password":
		raw = a.Password
	case "api_key":
		raw = a.APIKey
	case "cookie":
		raw = a.Cookie
	default:
		writeErr(w, http.StatusBadRequest, "field 仅支持 password / api_key / cookie")
		return
	}
	plain, derr := s.crypto.Decrypt(raw)
	if derr != nil {
		writeErr(w, http.StatusInternalServerError, derr.Error())
		return
	}
	writeOK(w, map[string]any{"field": field, "value": plain})
}

// handleGetSettings 返回全局设置（代理等）。
func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	st := s.store.GetSettings()
	writeOK(w, map[string]any{
		"proxy":     st.Proxy,
		"env_proxy": strings.TrimSpace(os.Getenv("MUSE_PROXY")),
		"effective": effectiveProxy("", st.Proxy),
	})
}

// handlePutSettings 保存全局设置（当前仅代理地址）。
func (s *Server) handlePutSettings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Proxy string `json:"proxy"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体格式错误")
		return
	}
	if err := s.store.SaveSettings(Settings{Proxy: req.Proxy}); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeOK(w, map[string]any{"proxy": strings.TrimSpace(req.Proxy)})
}

// handleTestProxy 测试代理连通性（body 可携带待测代理；为空则测已保存/环境代理）。
func (s *Server) handleTestProxy(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Proxy string `json:"proxy"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	proxy := strings.TrimSpace(req.Proxy)
	if proxy == "" {
		proxy = effectiveProxy("", s.store.GetSettings().Proxy)
	}
	writeOK(w, testProxyFrom("https://muse.ai/", proxy, 20*time.Second))
}

func (s *Server) handleTouch(w http.ResponseWriter, r *http.Request) {
	if err := s.store.Touch(r.PathValue("id")); err != nil {
		writeErr(w, http.StatusNotFound, "账号不存在")
		return
	}
	writeOK(w, map[string]any{"touched": true})
}

// inviteRefreshOutcome 批量刷新邀请的单账号结果。
type inviteRefreshOutcome struct {
	ID            string `json:"id"`
	Email         string `json:"email"`
	OK            bool   `json:"ok"`
	Skipped       bool   `json:"skipped"`
	InviteState   string `json:"invite_state,omitempty"`
	Code          string `json:"code,omitempty"`
	NewCode       bool   `json:"new_code,omitempty"` // 本次刮开了新码（之前无码）
	MaxUses       int    `json:"max_uses"`
	UseCount      int    `json:"use_count"`
	UsesRemaining int    `json:"uses_remaining"`
	TopupTotal    int64  `json:"topup_total,omitempty"`     // 订阅加购/奖励额度（>0=已兑换他人码）
	HasRedeemed   bool   `json:"has_redeemed,omitempty"`    // 依据订阅数据的可靠判定
	SubSynced     bool   `json:"sub_synced,omitempty"`      // 订阅查询是否成功
	Error         string `json:"error,omitempty"`
	SyncedAt      string `json:"synced_at,omitempty"`
}

// inviteRefreshSummary 批量刷新邀请的汇总统计。
type inviteRefreshSummary struct {
	Total    int `json:"total"`
	Refreshed int `json:"refreshed"` // 成功拿到邀请码
	Acquired int `json:"acquired"`  // 其中首次刮出新码
	Exhausted int `json:"exhausted"` // 额度用尽（EXHAUSTED）
	Failed   int `json:"failed"`    // 其它错误（登录失效/真人验证/网络等）
	Skipped  int `json:"skipped"`   // 无 Cookie
}

// aggregateInviteRefresh 汇总批量刷新结果（纯函数，可测试）。
func aggregateInviteRefresh(outs []inviteRefreshOutcome) inviteRefreshSummary {
	sum := inviteRefreshSummary{Total: len(outs)}
	for _, o := range outs {
		switch {
		case o.Skipped:
			sum.Skipped++
		case o.OK:
			sum.Refreshed++
			if o.NewCode {
				sum.Acquired++
			}
		case o.InviteState == "EXHAUSTED" || strings.Contains(o.Error, "额度已用尽"):
			sum.Exhausted++
		default:
			sum.Failed++
		}
	}
	return sum
}

// runInviteRefresh 对单账号拉取邀请信息（刮码+订阅）并写回。
func (s *Server) runInviteRefresh(a *Account) inviteRefreshOutcome {
	out := inviteRefreshOutcome{ID: a.ID, Email: a.Email}
	defer func() {
		if r := recover(); r != nil {
			out.OK = false
			out.Error = fmt.Sprintf("刷新异常: %v", r)
		}
	}()
	if strings.TrimSpace(a.Cookie) == "" {
		out.Skipped = true
		out.Error = "未设置登录 Cookie，已跳过"
		return out
	}
	cookie, derr := s.crypto.Decrypt(a.Cookie)
	if derr != nil {
		out.Error = "Cookie 解密失败（主密钥不匹配）"
		return out
	}
	prevCode := a.InviteCode
	_, inv, raw, ferr := FetchInvite(cookie, accountIdentity(a), a.Proxy, s.store.GetSettings().Proxy)
	synced := now()
	if raw != nil {
		_ = s.store.Update(a.ID, func(x *Account) error {
			x.InviteInfo = string(raw)
			x.InviteSyncedAt = synced
			if inv != nil && inv.Code != "" {
				x.InviteCode = inv.Code
			}
			return nil
		})
	}
	if ferr != nil {
		if inv != nil {
			out.InviteState = inv.InviteState
		}
		out.Error = ferr.Error()
		return out
	}
	out.OK = true
	out.InviteState = inv.InviteState
	out.Code = inv.Code
	out.NewCode = inv.Code != "" && prevCode == ""
	out.MaxUses = inv.MaxUses
	out.UseCount = inv.UseCount
	out.UsesRemaining = inv.UsesRemaining
	out.SyncedAt = synced
	if inv.Subscription != nil {
		out.SubSynced = true
		out.TopupTotal = inv.Subscription.TopupTotal
		out.HasRedeemed = inv.Subscription.HasRedeemed
	}
	return out
}

// handleFetchInviteAll 批量刷新邀请状态（默认全部，可传 {"ids":[...]} 限定选中项；并发 3，每账号独立指纹）。
// 适配场景：新账号首次刮码、邀请码使用次数实时同步、已兑换状态订阅判定。
func (s *Server) handleFetchInviteAll(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IDs []string `json:"ids"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req) // 容忍空 body：空 = 全部账号
	accounts := s.selectedAccounts(req.IDs)
	const concurrency = 3
	outs := make([]inviteRefreshOutcome, len(accounts))
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for i, a := range accounts {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, a *Account) {
			defer wg.Done()
			defer func() { <-sem }()
			outs[i] = s.runInviteRefresh(a)
		}(i, a)
	}
	wg.Wait()
	writeOK(w, map[string]any{"results": outs, "summary": aggregateInviteRefresh(outs)})
}

// handleFetchInvite 服务端代为请求 muse.ai 邀请接口并保存快照。
func (s *Server) handleFetchInvite(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	a, err := s.store.Get(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "账号不存在")
		return
	}
	cookie, derr := s.crypto.Decrypt(a.Cookie)
	if derr != nil {
		writeErr(w, http.StatusInternalServerError, "Cookie 解密失败（主密钥不匹配）")
		return
	}
	pubKey, inv, raw, ferr := FetchInvite(cookie, accountIdentity(a), a.Proxy, s.store.GetSettings().Proxy)
	if ferr != nil {
		// 出错时若拿到了部分快照（如 invite_state=EXHAUSTED / 风控响应），也留档便于排查与展示
		if raw != nil {
			synced := now()
			_ = s.store.Update(id, func(x *Account) error {
				x.InviteInfo = string(raw)
				x.InviteSyncedAt = synced
				return nil
			})
		}
		writeErr(w, http.StatusBadGateway, ferr.Error())
		return
	}
	synced := now()
	if uerr := s.store.Update(id, func(x *Account) error {
		x.InviteInfo = string(raw)
		x.InviteSyncedAt = synced
		if inv.Code != "" {
			x.InviteCode = inv.Code
		}
		return nil
	}); uerr != nil {
		writeErr(w, http.StatusInternalServerError, uerr.Error())
		return
	}
	writeOK(w, map[string]any{"invite": inv, "publishable_key": pubKey, "synced_at": synced})
}

// mapAdmissionStatus 体检状态 → 账号状态映射（与单账号/一键体检共用）：
// admitted→active / disabled→banned / region_limited→region_limited /
// auth_expired→unverified；challenge/forbidden/unknown 不改状态（返回空串）。
func mapAdmissionStatus(state string) string {
	switch state {
	case "admitted":
		return "active"
	case "disabled":
		return "banned"
	case "region_limited":
		return "region_limited"
	case "auth_expired":
		return "unverified"
	}
	return ""
}

// checkOutcome 一键体检的单账号结果。
type checkOutcome struct {
	ID        string `json:"id"`
	Email     string `json:"email"`
	OK        bool   `json:"ok"`
	Skipped   bool   `json:"skipped"`           // 无 Cookie，未发起检测
	State     string `json:"state,omitempty"`    // 与单账号体检同源状态机
	Type      string `json:"type,omitempty"`
	VMState   string `json:"vm_state,omitempty"`
	Detail    string `json:"detail,omitempty"`
	NewStatus string `json:"new_status,omitempty"`
	Error     string `json:"error,omitempty"`
	CheckedAt string `json:"checked_at,omitempty"`
}

// runAccountCheck 对单个账号执行体检并写回账号记录（状态映射与单账号体检一致）。
// 并发安全：store.Update 内部持写锁，Crypto(GCM) 无状态。
func (s *Server) runAccountCheck(a *Account) checkOutcome {
	out := checkOutcome{ID: a.ID, Email: a.Email}
	defer func() {
		if r := recover(); r != nil {
			out.OK = false
			out.Error = fmt.Sprintf("体检异常: %v", r)
		}
	}()
	if strings.TrimSpace(a.Cookie) == "" {
		out.Skipped = true
		out.Error = "未设置登录 Cookie，已跳过"
		return out
	}
	cookie, derr := s.crypto.Decrypt(a.Cookie)
	if derr != nil {
		out.Error = "Cookie 解密失败（主密钥不匹配）"
		return out
	}
	res, ferr := CheckAdmission(cookie, accountIdentity(a), a.Proxy, s.store.GetSettings().Proxy)
	if ferr != nil {
		out.Error = ferr.Error()
		return out
	}
	newStatus := mapAdmissionStatus(res.State)
	checked := now()
	checkJSON, _ := json.Marshal(res)
	if uerr := s.store.Update(a.ID, func(x *Account) error {
		x.CheckInfo = string(checkJSON)
		x.CheckedAt = checked
		if newStatus != "" {
			x.Status = newStatus
		}
		return nil
	}); uerr != nil {
		out.Error = uerr.Error()
	}
	out.OK = true
	out.State = res.State
	out.Type = res.Type
	out.VMState = res.VMState
	out.Detail = res.Detail
	out.NewStatus = newStatus
	out.CheckedAt = checked
	return out
}

// checkSummary 一键体检汇总统计。
type checkSummary struct {
	Total         int `json:"total"`
	Checked       int `json:"checked"`
	Skipped       int `json:"skipped"` // 无 Cookie 未检测
	Failed        int `json:"failed"`  // 网络/解密等失败
	Admitted      int `json:"admitted"`
	Disabled      int `json:"disabled"`
	RegionLimited int `json:"region_limited"`
	AuthExpired   int `json:"auth_expired"`
	Challenge     int `json:"challenge"`
	Forbidden     int `json:"forbidden"`
	Unknown       int `json:"unknown"`
}

// aggregateCheckOutcomes 汇总单账号结果（纯函数，可测试）。
func aggregateCheckOutcomes(outs []checkOutcome) checkSummary {
	sum := checkSummary{Total: len(outs)}
	for _, o := range outs {
		switch {
		case o.Skipped:
			sum.Skipped++
		case !o.OK:
			sum.Failed++
		default:
			sum.Checked++
			switch o.State {
			case "admitted":
				sum.Admitted++
			case "disabled":
				sum.Disabled++
			case "region_limited":
				sum.RegionLimited++
			case "auth_expired":
				sum.AuthExpired++
			case "challenge":
				sum.Challenge++
			case "forbidden":
				sum.Forbidden++
			default:
				sum.Unknown++
			}
		}
	}
	return sum
}

// selectedAccounts 按 ids 取账号（空 ids = 全部）；忽略不存在/重复的 ID。
func (s *Server) selectedAccounts(ids []string) []*Account {
	if len(ids) == 0 {
		return s.store.Snapshot()
	}
	out := make([]*Account, 0, len(ids))
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		if a, err := s.store.Get(id); err == nil {
			out = append(out, a)
		}
	}
	return out
}

// handleCheckAll 一键体检：并发检测账号（默认全部，可传 {"ids":[...]} 限定选中项），
// 并发平台上限 3（防代理过载/风控），每个账号结果写回（与单账号体检同一映射逻辑），
// 返回明细与汇总统计。无 Cookie / 解密失败 / 网络失败的账号分别计为 skipped/failed，不中断整批。
func (s *Server) handleCheckAll(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IDs []string `json:"ids"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req) // 容忍空 body：空 = 全部账号
	accounts := s.selectedAccounts(req.IDs)
	const concurrency = 3
	outs := make([]checkOutcome, len(accounts))
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for i, a := range accounts {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, a *Account) {
			defer wg.Done()
			defer func() { <-sem }()
			outs[i] = s.runAccountCheck(a)
		}(i, a)
	}
	wg.Wait()
	writeOK(w, map[string]any{"results": outs, "summary": aggregateCheckOutcomes(outs)})
}

// handleCheckStatus 准入状态体检：POST /api/auth/check + 结果写入账号。
func (s *Server) handleCheckStatus(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	a, err := s.store.Get(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "账号不存在")
		return
	}
	cookie, derr := s.crypto.Decrypt(a.Cookie)
	if derr != nil {
		writeErr(w, http.StatusInternalServerError, "Cookie 解密失败（主密钥不匹配）")
		return
	}
	res, ferr := CheckAdmission(cookie, accountIdentity(a), a.Proxy, s.store.GetSettings().Proxy)
	if ferr != nil {
		writeErr(w, http.StatusBadGateway, ferr.Error())
		return
	}
	checked := now()
	checkJSON, _ := json.Marshal(res)
	// 状态映射见 mapAdmissionStatus（与一键体检共用）
	newStatus := mapAdmissionStatus(res.State)
	uerr := s.store.Update(id, func(x *Account) error {
		x.CheckInfo = string(checkJSON)
		x.CheckedAt = checked
		if newStatus != "" {
			x.Status = newStatus
		}
		return nil
	})
	if uerr != nil {
		writeErr(w, http.StatusInternalServerError, uerr.Error())
		return
	}
	writeOK(w, map[string]any{"admission": res, "status_updated": newStatus, "checked_at": checked})
}

// handleLoginSendOTP 登录第一步：restart + send-otp，返回会话 ID。
// proxy 可选：填账号专属代理；留空则用全局设置（与后续 API 调用同源）。
func (s *Server) handleLoginSendOTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email string `json:"email"`
		Proxy string `json:"proxy"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体格式错误")
		return
	}
	id, hasAccount, err := s.logins.LoginSendOTP(req.Email, effectiveProxy(req.Proxy, s.store.GetSettings().Proxy))
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeOK(w, map[string]any{"session_id": id, "has_muse_account": *hasAccount})
}

// handleLoginVerify 登录第二步：confirm-otp + save-account，返回完整 Cookie。
func (s *Server) handleLoginVerify(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID string `json:"session_id"`
		Code      string `json:"code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体格式错误")
		return
	}
	res, err := s.logins.LoginVerify(req.SessionID, req.Code)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeOK(w, res)
}

// mapRedeemStatus 兑换结果状态 → 账号状态映射（单个/批量兑换共用）：
// redeemed→active / auth_expired→unverified / region_limited→region_limited；
// already_redeemed/used_up/invalid/http-*/error 不改状态（返回空串）。
func mapRedeemStatus(state string) string {
	switch state {
	case "redeemed":
		return "active"
	case "auth_expired":
		return "unverified"
	case "region_limited":
		return "region_limited"
	}
	return ""
}

// redeemBatchOutcome 批量兑换的单账号结果。
type redeemBatchOutcome struct {
	ID        string `json:"id"`
	Email     string `json:"email"`
	OK        bool   `json:"ok"`                // 兑换链执行完成（含业务失败如 already_redeemed）
	Skipped   bool   `json:"skipped"`           // 无 Cookie，未发起兑换
	State     string `json:"state,omitempty"`    // redeemed/already_redeemed/used_up/invalid/auth_expired/region_limited/http-*/error
	VMID      string `json:"vm_id,omitempty"`
	OwnCode   string `json:"own_code,omitempty"`
	Reason    string `json:"reason,omitempty"`
	Detail    string `json:"detail,omitempty"`
	NewStatus string `json:"new_status,omitempty"`
	Error     string `json:"error,omitempty"`
	RedeemedAt string `json:"redeemed_at,omitempty"`
}

// redeemBatchSummary 批量兑换汇总统计。
type redeemBatchSummary struct {
	Total          int `json:"total"`
	Succeeded      int `json:"succeeded"`      // redeemed
	AlreadyRedeemed int `json:"already_redeemed"`
	UsedUp     int `json:"used_up"`
	Invalid    int `json:"invalid"`
	AuthExpired     int `json:"auth_expired"`
	RegionLimited  int `json:"region_limited"`
	Failed      int `json:"failed"`          // 网络/流程错误（error/http-*）
	Skipped     int `json:"skipped"`         // 无 Cookie
}

// aggregateRedeemBatch 汇总批量兑换结果（纯函数，可测试）。
func aggregateRedeemBatch(outs []redeemBatchOutcome) redeemBatchSummary {
	sum := redeemBatchSummary{Total: len(outs)}
	for _, o := range outs {
		switch {
		case o.Skipped:
			sum.Skipped++
		case !o.OK:
			sum.Failed++
		default:
			switch o.State {
			case "redeemed":
				sum.Succeeded++
			case "already_redeemed":
				sum.AlreadyRedeemed++
			case "used_up":
				sum.UsedUp++
			case "invalid":
				sum.Invalid++
			case "auth_expired":
				sum.AuthExpired++
			case "region_limited":
				sum.RegionLimited++
			default:
				sum.Failed++
			}
		}
	}
	return sum
}

// runRedeemBatchAccount 对单账号执行兑换链并写回（每个账号独立指纹）。
// 无 Cookie 计 skipped；解密/网络失败计 failed；均不中断整批。
func (s *Server) runRedeemBatchAccount(id, code string) (out redeemBatchOutcome) {
	defer func() {
		if r := recover(); r != nil {
			out.OK = false
			out.Error = fmt.Sprintf("兑换异常: %v", r)
		}
	}()
	a, err := s.store.Get(id)
	if err != nil {
		out.ID, out.OK, out.Error = id, false, "账号不存在"
		return out
	}
	out.ID, out.Email = a.ID, a.Email
	if strings.TrimSpace(a.Cookie) == "" {
		out.Skipped = true
		out.Error = "未设置登录 Cookie，已跳过"
		return out
	}
	cookie, derr := s.crypto.Decrypt(a.Cookie)
	if derr != nil {
		out.Error = "Cookie 解密失败（主密钥不匹配）"
		return out
	}
	res, rerr := RedeemInviteCode(cookie, accountIdentity(a), code, a.Proxy, s.store.GetSettings().Proxy)
	if rerr != nil {
		out.Error = rerr.Error()
		return out
	}
	redeemed := now()
	outJSON, _ := json.Marshal(res)
	newStatus := mapRedeemStatus(res.State)
	if uerr := s.store.Update(a.ID, func(x *Account) error {
		x.RedeemInfo = string(outJSON)
		x.RedeemedAt = redeemed
		if newStatus != "" {
			x.Status = newStatus
		}
		return nil
	}); uerr != nil {
		out.Error = uerr.Error()
		return out
	}
	out.OK = true
	out.State = res.State
	out.VMID = res.VMID
	out.OwnCode = res.OwnCode
	out.Reason = res.Reason
	out.Detail = res.Detail
	out.NewStatus = newStatus
	out.RedeemedAt = redeemed
	// 兑换成功后尽力刷新邀请信息（与单个兑换行为一致）
	if res.State == "redeemed" {
		if _, inv, raw, ferr := FetchInvite(cookie, accountIdentity(a), a.Proxy, s.store.GetSettings().Proxy); ferr == nil {
			_ = s.store.Update(a.ID, func(x *Account) error {
				x.InviteInfo = string(raw)
				x.InviteSyncedAt = redeemed
				if inv.Code != "" {
					x.InviteCode = inv.Code
				}
				return nil
			})
		}
	}
	return out
}

// handleRedeemBatch 邀请码批量兑换：同一码并发兑换多个选中账号（并发 3，
// 每账号独立指纹防关联），结果逐账号写回并返回明细+汇总统计。
// 请求体：{"code":"XXXXXX","ids":["acc_...",...]}
func (s *Server) handleRedeemBatch(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code string   `json:"code"`
		IDs  []string `json:"ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体格式错误")
		return
	}
	code := strings.ToUpper(strings.TrimSpace(req.Code))
	if code == "" {
		writeErr(w, http.StatusBadRequest, "请填写邀请码")
		return
	}
	if len(req.IDs) == 0 {
		writeErr(w, http.StatusBadRequest, "请至少选择一个账号")
		return
	}
	const concurrency = 3
	var wg sync.WaitGroup
	sem := make(chan struct{}, concurrency)
	outs := make([]redeemBatchOutcome, len(req.IDs))
	for i, id := range req.IDs {
		wg.Add(1)
		go func(i int, id string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			outs[i] = s.runRedeemBatchAccount(id, code)
		}(i, id)
	}
	wg.Wait()
	writeOK(w, map[string]any{"results": outs, "summary": aggregateRedeemBatch(outs), "code": code})
}

// handleRedeem 邀请码兑换链：唤醒 VM → stateful-sharing → redeem → 取回自己的码。
// 成功后顺带刷新邀请信息快照（与「🎁 邀请」按钮同源数据）。
func (s *Server) handleRedeem(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体格式错误")
		return
	}
	code := strings.ToUpper(strings.TrimSpace(req.Code))
	if code == "" {
		writeErr(w, http.StatusBadRequest, "请填写邀请码")
		return
	}
	a, err := s.store.Get(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "账号不存在")
		return
	}
	cookie, derr := s.crypto.Decrypt(a.Cookie)
	if derr != nil {
		writeErr(w, http.StatusInternalServerError, "Cookie 解密失败（主密钥不匹配）")
		return
	}
	out, rerr := RedeemInviteCode(cookie, accountIdentity(a), code, a.Proxy, s.store.GetSettings().Proxy)
	if rerr != nil {
		writeErr(w, http.StatusBadGateway, rerr.Error())
		return
	}
	redeemed := now()
	outJSON, _ := json.Marshal(out)
	// 状态映射见 mapRedeemStatus（与批量兑换共用）
	newStatus := mapRedeemStatus(out.State)
	if uerr := s.store.Update(id, func(x *Account) error {
		x.RedeemInfo = string(outJSON)
		x.RedeemedAt = redeemed
		if newStatus != "" {
			x.Status = newStatus
		}
		return nil
	}); uerr != nil {
		writeErr(w, http.StatusInternalServerError, uerr.Error())
		return
	}
	// 兑换成功后尽力刷新邀请信息（自己的码/剩余次数）
	if out.State == "redeemed" {
		if _, inv, raw, ferr := FetchInvite(cookie, accountIdentity(a), a.Proxy, s.store.GetSettings().Proxy); ferr == nil {
			_ = s.store.Update(id, func(x *Account) error {
				x.InviteInfo = string(raw)
				x.InviteSyncedAt = redeemed
				if inv.Code != "" {
					x.InviteCode = inv.Code
				}
				return nil
			})
		}
	}
	writeOK(w, map[string]any{"redeem": out, "status_updated": newStatus, "redeemed_at": redeemed})
}

// handleExport 导出备份。reveal=1 时导出明文（敏感），默认保留密文形态。
func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	reveal := r.URL.Query().Get("reveal") == "1"
	list := s.store.Snapshot() // 已按创建顺序正序
	out := make([]*Account, 0, len(list))
	for _, a := range list {
		cp := *a
		if reveal {
			for _, v := range []*string{&cp.Password, &cp.APIKey, &cp.Cookie} {
				if pt, err := s.crypto.Decrypt(*v); err == nil {
					*v = pt
				}
			}
		}
		out = append(out, &cp)
	}
	body, err := json.MarshalIndent(DBFile{Version: 1, UpdatedAt: now(), Accounts: out}, "", "  ")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "序列化失败")
		return
	}
	name := fmt.Sprintf("muse-accounts-backup-%s.json", time.Now().Format("20060102-150405"))
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	_, _ = w.Write(body)
}

// handleImport 导入：mode=merge（默认，按 ID/Email 去重跳过）或 replace（整体替换）。
func (s *Server) handleImport(w http.ResponseWriter, r *http.Request) {
	mode := r.URL.Query().Get("mode")
	if mode == "" {
		mode = "merge"
	}
	if mode != "merge" && mode != "replace" {
		writeErr(w, http.StatusBadRequest, "mode 仅支持 merge / replace")
		return
	}
	var db DBFile
	if err := json.NewDecoder(r.Body).Decode(&db); err != nil {
		writeErr(w, http.StatusBadRequest, "导入文件格式错误："+err.Error())
		return
	}
	if db.Accounts == nil {
		writeErr(w, http.StatusBadRequest, "导入文件缺少 accounts 数组")
		return
	}
	unconverted := 0
	for _, a := range db.Accounts {
		// enc: 前缀密文原样入库（同主密钥可直接使用）；明文则按当前主密钥加密。
		for _, f := range []*string{&a.Password, &a.APIKey} {
			if *f != "" && !IsEncrypted(*f) {
				*f = s.crypto.Encrypt(*f)
			}
		}
	}
	var added, imported, skipped int
	var err error
	if mode == "merge" {
		added, skipped, err = s.store.MergeImport(db.Accounts)
	} else {
		imported, err = s.store.ReplaceImport(db.Accounts)
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	res := map[string]any{"mode": mode, "accounts": s.store.Count()}
	if mode == "merge" {
		res["added"], res["skipped"] = added, skipped
	} else {
		res["imported"] = imported
	}
	res["plaintext_converted"] = true
	res["cipher_kept"] = unconverted
	writeOK(w, res)
}

// absPath 展示用绝对路径。
func absPath(p string) string {
	if ap, err := filepath.Abs(p); err == nil {
		return ap
	}
	return p
}
