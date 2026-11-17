package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newLoginMock 模拟 muse.ai 登录链四步端点。
// 返回服务器与一个状态记录器（校验请求契约用）。
func newLoginMock(t *testing.T) (*httptest.Server, *[]string) {
	t.Helper()
	var order []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		order = append(order, r.Method+" "+r.URL.Path)
		// 登录链请求必须带浏览器特征头与 CSRF（除 restart）
		if r.URL.Path != "/api/auth/native/restart" && r.Header.Get("Sec-Fetch-Site") != "same-origin" {
			t.Errorf("%s 缺少 Sec-Fetch-Site", r.URL.Path)
		}
		switch r.URL.Path {
		case "/api/auth/native/restart":
			if r.Header.Get("X-Hatch-CSRF-Token") != "" {
				t.Error("restart 不应带 CSRF 头")
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(201)
			_, _ = w.Write([]byte(`{"csrf_token":"TESTCSRF","flow_state":"started"}`))
		case "/api/auth/native/send-otp":
			if r.Header.Get("X-Hatch-CSRF-Token") != "TESTCSRF" {
				t.Error("send-otp 应携带 restart 返回的 CSRF")
			}
			var p map[string]any
			_ = json.NewDecoder(r.Body).Decode(&p)
			if p["contact_point"] == "" {
				t.Error("send-op 应含 contact_point")
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(201)
			_, _ = w.Write([]byte(`{"registration_state":"otp_sent","flow":"login","has_muse_account":true}`))
		case "/api/auth/native/confirm-otp":
			var p map[string]any
			_ = json.NewDecoder(r.Body).Decode(&p)
			if p["supports_muse_link_transparency"] != true {
				t.Error("confirm-otp 必须带 supports_muse_link_transparency:true")
			}
			w.Header().Set("Content-Type", "application/json")
			if p["otp_code"] != "123456" {
				w.WriteHeader(400)
				_, _ = w.Write([]byte(`{"type":"otpInvalid","title":"Invalid code","detail":"The code is incorrect","status":400}`))
				return
			}
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"redirect_to":"/","auth_meta_session":{"frl_access_token":"FRLTEST","frl_account_id":"1111222233334444"}}`))
		case "/api/auth/save-account":
			w.Header().Set("Set-Cookie", "hatch_sess=SESS-abc123; Path=/; HttpOnly; Secure")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"saved":true}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ts.Close)
	return ts, &order
}

func TestLoginChainSuccess(t *testing.T) {
	ts, order := newLoginMock(t)
	pool := newLoginSessionsWithBase(ts.URL)
	sid, hasAccount, err := pool.LoginSendOTP("user@test.com", "")
	if err != nil {
		t.Fatal(err)
	}
	if sid == "" {
		t.Fatal("应返回会话 ID")
	}
	if !*hasAccount {
		t.Fatal("has_muse_account 应为 true")
	}
	res, err := pool.LoginVerify(sid, "123456")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Cookie, "hatch_sess=SESS-abc123") {
		t.Fatalf("Cookie 应含 hatch_sess: %s", res.Cookie)
	}
	if res.HatchSessLen != len("SESS-abc123") {
		t.Fatalf("hatch_sess 长度错误: %d", res.HatchSessLen)
	}
	if res.AccountID != "1111222233334444" {
		t.Fatalf("account_id 错误: %s", res.AccountID)
	}
	want := "POST /api/auth/native/restart,POST /api/auth/native/send-otp,POST /api/auth/native/confirm-otp,POST /api/auth/save-account"
	if strings.Join(*order, ",") != want {
		t.Fatalf("链序错误: %v", *order)
	}
}

func TestLoginVerifyWrongCode(t *testing.T) {
	ts, _ := newLoginMock(t)
	pool := newLoginSessionsWithBase(ts.URL)
	sid, _, err := pool.LoginSendOTP("user@test.com", "")
	if err != nil {
		t.Fatal(err)
	}
	// mock 只接受 123456 → 用错误码应得到明确错误
	if _, err := pool.LoginVerify(sid, "000000"); err == nil {
		t.Fatal("错误验证码应报错")
	} else if !strings.Contains(err.Error(), "验证码") {
		t.Fatalf("错误信息应说明验证码问题: %v", err)
	}
}

func TestLoginSendOTPValidation(t *testing.T) {
	pool := newLoginSessions()
	if _, _, err := pool.LoginSendOTP("", ""); err == nil {
		t.Fatal("空邮箱应报错")
	}
	if _, _, err := pool.LoginSendOTP("not-an-email", ""); err == nil {
		t.Fatal("无效邮箱应报错")
	}
}

func TestLoginSessionExpiry(t *testing.T) {
	ts, _ := newLoginMock(t)
	pool := newLoginSessionsWithBase(ts.URL)
	sid, _, err := pool.LoginSendOTP("user@test.com", "")
	if err != nil {
		t.Fatal(err)
	}
	// 直接删掉会话模拟过期
	pool.mu.Lock()
	delete(pool.sessions, sid)
	pool.mu.Unlock()
	if _, err := pool.LoginVerify(sid, "123456"); err == nil {
		t.Fatal("过期会话应报错")
	} else if !strings.Contains(err.Error(), "过期") {
		t.Fatalf("错误应说明会话过期: %v", err)
	}
}

func TestLoginVerifyRepeatReturnsCached(t *testing.T) {
	ts, order := newLoginMock(t)
	pool := newLoginSessionsWithBase(ts.URL)
	sid, _, _ := pool.LoginSendOTP("user@test.com", "")
	res1, err := pool.LoginVerify(sid, "123456")
	if err != nil {
		t.Fatal(err)
	}
	res2, err := pool.LoginVerify(sid, "123456")
	if err != nil {
		t.Fatal(err)
	}
	if res1.Cookie != res2.Cookie {
		t.Fatal("重复 verify 应返回缓存结果")
	}
	// 链不应重复执行（4 步只跑一次）
	if len(*order) != 4 {
		t.Fatalf("重复 verify 不应重放链路: %v", *order)
	}
}

// newAccountSelectionMock 模拟 confirm-otp 返回 account_selection_required
// 的登录链（2026-11-16 区域受限账号实测场景）。
func newAccountSelectionMock(t *testing.T, confirmBody string) (*httptest.Server, *[]string) {
	t.Helper()
	var order []string
	var selBody []byte
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		order = append(order, r.Method+" "+r.URL.Path)
		switch r.URL.Path {
		case "/api/auth/native/restart":
			w.WriteHeader(201)
			_, _ = w.Write([]byte(`{"csrf_token":"TESTCSRF"}`))
		case "/api/auth/native/send-otp":
			w.WriteHeader(201)
			_, _ = w.Write([]byte(`{"registration_state":"otp_sent","has_muse_account":false}`))
		case "/api/auth/native/confirm-otp":
			var p map[string]any
			_ = json.NewDecoder(r.Body).Decode(&p)
			if p["otp_code"] != "123456" {
				w.WriteHeader(400)
				_, _ = w.Write([]byte(`{"type":"otpInvalid"}`))
				return
			}
			w.WriteHeader(200)
			_, _ = w.Write([]byte(confirmBody))
		case "/api/auth/native/select-account":
			if r.Header.Get("X-Hatch-CSRF-Token") != "TESTCSRF" {
				t.Error("select-account 应携带 CSRF")
			}
			selBody, _ = io.ReadAll(io.LimitReader(r.Body, 1<<16))
			if string(selBody) == `{"account_id":"BAD"}` {
				w.WriteHeader(403)
				_, _ = w.Write([]byte(`{"error":"Forbidden"}`))
				return
			}
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"redirect_to":"/","auth_meta_session":{"frl_access_token":"FRLSEL","frl_account_id":"777"}}`))
		case "/api/auth/save-account":
			w.Header().Set("Set-Cookie", "hatch_sess=SESS-sel; Path=/")
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"saved":true}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ts.Close)
	return ts, &order
}

func TestLoginAccountSelectionAuto(t *testing.T) {
	// confirm-otp 返回 account_selection_required + 单账号 → 自动选号完成登录
	ts, order := newAccountSelectionMock(t,
		`{"registration_state":"account_selection_required","accounts":[{"account_id":"0","account_status":"active","profiles":[{"platform":"abra"}]}]}`)
	pool := newLoginSessionsWithBase(ts.URL)
	sid, _, err := pool.LoginSendOTP("region@test.com", "")
	if err != nil {
		t.Fatal(err)
	}
	res, err := pool.LoginVerify(sid, "123456")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Cookie, "hatch_sess=SESS-sel") {
		t.Fatalf("选号后应拿到 hatch_sess: %s", res.Cookie)
	}
	if res.AccountID != "777" {
		t.Fatalf("account_id 应取 select-account 响应值: %s", res.AccountID)
	}
	want := "POST /api/auth/native/restart,POST /api/auth/native/send-otp,POST /api/auth/native/confirm-otp,POST /api/auth/native/select-account,POST /api/auth/save-account"
	if strings.Join(*order, ",") != want {
		t.Fatalf("链序错误: %v", *order)
	}
}

func TestLoginAccountSelectionPrefersActive(t *testing.T) {
	// 多账号：优先非 deactivated 的账号
	var gotID string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/auth/native/restart":
			w.WriteHeader(201)
			_, _ = w.Write([]byte(`{"csrf_token":"T"}`))
		case "/api/auth/native/send-otp":
			w.WriteHeader(201)
			_, _ = w.Write([]byte(`{}`))
		case "/api/auth/native/confirm-otp":
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"registration_state":"account_selection_required","accounts":[
				{"account_id":"dead","account_status":"deactivated"},
				{"account_id":"alive","account_status":""}]}`))
		case "/api/auth/native/select-account":
			b, _ := io.ReadAll(r.Body)
			var p map[string]any
			_ = json.Unmarshal(b, &p)
			gotID, _ = p["account_id"].(string)
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"auth_meta_session":{"frl_access_token":"X","frl_account_id":"1"}}`))
		case "/api/auth/save-account":
			w.Header().Set("Set-Cookie", "hatch_sess=S; Path=/")
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"saved":true}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()
	pool := newLoginSessionsWithBase(ts.URL)
	sid, _, _ := pool.LoginSendOTP("multi@test.com", "")
	if _, err := pool.LoginVerify(sid, "123456"); err != nil {
		t.Fatal(err)
	}
	if gotID != "alive" {
		t.Fatalf("应选择状态正常的账号, got %q", gotID)
	}
}

func TestLoginAccountSelectionNoAccounts(t *testing.T) {
	// account_selection_required 但无可用账号 → 明确报错
	ts, _ := newAccountSelectionMock(t, `{"registration_state":"account_selection_required","accounts":[]}`)
	pool := newLoginSessionsWithBase(ts.URL)
	sid, _, _ := pool.LoginSendOTP("x@test.com", "")
	_, err := pool.LoginVerify(sid, "123456")
	if err == nil || !strings.Contains(err.Error(), "选择账号") {
		t.Fatalf("无可用账号应报错: %v", err)
	}
}

func TestLoginAccountSelectionForbidden(t *testing.T) {
	// select-account 被风控拦截（403）→ 报 HTTP 状态
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/auth/native/restart":
			w.WriteHeader(201)
			_, _ = w.Write([]byte(`{"csrf_token":"T"}`))
		case "/api/auth/native/send-otp":
			w.WriteHeader(201)
			_, _ = w.Write([]byte(`{}`))
		case "/api/auth/native/confirm-otp":
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"registration_state":"account_selection_required","accounts":[{"account_id":"BAD"}]}`))
		case "/api/auth/native/select-account":
			w.WriteHeader(403)
			_, _ = w.Write([]byte(`{"error":"Forbidden"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()
	pool := newLoginSessionsWithBase(ts.URL)
	sid, _, _ := pool.LoginSendOTP("f@test.com", "")
	_, err := pool.LoginVerify(sid, "123456")
	if err == nil || !strings.Contains(err.Error(), "select-account 返回 HTTP 403") {
		t.Fatalf("select-account 403 应报错: %v", err)
	}
}

// TestLoginCheckpointOnConfirm200 confirm-otp 200 携带 checkpoint_url → 真人验证拦截。
func TestLoginCheckpointOnConfirm200(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/auth/native/restart":
			w.WriteHeader(201)
			_, _ = w.Write([]byte(`{"csrf_token":"T"}`))
		case "/api/auth/native/send-otp":
			w.WriteHeader(201)
			_, _ = w.Write([]byte(`{}`))
		case "/api/auth/native/confirm-otp":
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"redirect_to":"/","checkpoint_url":"/challenge/x"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()
	pool := newLoginSessionsWithBase(ts.URL)
	sid, _, _ := pool.LoginSendOTP("cp@test.com", "")
	_, err := pool.LoginVerify(sid, "123456")
	if err == nil || !strings.Contains(err.Error(), "真人验证") {
		t.Fatalf("checkpoint_url 应报真人验证拦截: %v", err)
	}
}

// TestLoginCheckpointOnErrorBody confirm-otp 错误响应体携带 checkpoint_url → 真人验证拦截。
func TestLoginCheckpointOnErrorBody(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/auth/native/restart":
			w.WriteHeader(201)
			_, _ = w.Write([]byte(`{"csrf_token":"T"}`))
		case "/api/auth/native/send-otp":
			w.WriteHeader(201)
			_, _ = w.Write([]byte(`{}`))
		case "/api/auth/native/confirm-otp":
			w.WriteHeader(403)
			_, _ = w.Write([]byte(`{"type":"checkpoint","checkpoint_url":"/challenge/y"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()
	pool := newLoginSessionsWithBase(ts.URL)
	sid, _, _ := pool.LoginSendOTP("cpe@test.com", "")
	_, err := pool.LoginVerify(sid, "123456")
	if err == nil || !strings.Contains(err.Error(), "真人验证") {
		t.Fatalf("错误体 checkpoint_url 应报真人验证拦截: %v", err)
	}
}
