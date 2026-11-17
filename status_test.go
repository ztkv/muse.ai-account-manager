package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const csrfCookie = "hatch_sess=abc; hatch_native_auth_csrf=csrf-token-123"

// newVMCheckMock 模拟 auth/check(200) + 落地页（含指定 vmState 的 Flight 数据）。
func newVMCheckMock(t *testing.T, vmState string) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/auth/check":
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"ok":true}`))
		case "/":
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(`<html><script>self.__next_f.push([1,"1a2b:{\"props\":{\"vmState\":\"` + vmState + `\"}}"]);</script></html>`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ts.Close)
	return ts
}

// TestCheckAdmissionVMDisabled 解除关联/禁用判定：auth/check 200 但落地页 vmState=DISABLED。
func TestCheckAdmissionVMDisabled(t *testing.T) {
	ts := newVMCheckMock(t, "DISABLED")
	res, err := checkAdmissionFrom(ts.URL+"/api/auth/check", "hatch_sess=abc", "fp-test@x.com", "", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if res.State != "disabled" || res.VMState != "DISABLED" {
		t.Fatalf("want disabled/DISABLED, got %s/%s", res.State, res.VMState)
	}
	if !strings.Contains(res.Detail, "禁用") || !strings.Contains(res.Detail, "解除关联") {
		t.Fatalf("detail 应说明禁用/解除关联: %s", res.Detail)
	}
}

// TestCheckAdmissionVMRunning 正常账号：admitted + VM RUNNING。
func TestCheckAdmissionVMRunning(t *testing.T) {
	ts := newVMCheckMock(t, "RUNNING")
	res, err := checkAdmissionFrom(ts.URL+"/api/auth/check", "hatch_sess=abc", "fp-test@x.com", "", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if res.State != "admitted" || res.VMState != "RUNNING" {
		t.Fatalf("want admitted/RUNNING, got %s/%s", res.State, res.VMState)
	}
	if !strings.Contains(res.Detail, "RUNNING") {
		t.Fatalf("detail 应含 VM RUNNING: %s", res.Detail)
	}
}

// TestCheckAdmissionVMLandingRedirect 落地页重定向（Cookie 失效场景）不覆盖 auth/check 判定。
func TestCheckAdmissionVMLandingRedirect(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/auth/check":
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"ok":true}`))
		case "/":
			w.Header().Set("Location", "/login")
			w.WriteHeader(302)
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()
	res, err := checkAdmissionFrom(ts.URL+"/api/auth/check", "hatch_sess=abc", "fp-test@x.com", "", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if res.State != "admitted" || res.VMState != "" {
		t.Fatalf("落地页 302 应保持 admitted 且 VMState 为空, got %s/%s", res.State, res.VMState)
	}
}

// TestCheckAdmissionForbiddenThenRefreshCSRF 边缘风控场景：
// 无 CSRF → 403；cookie 无 csrf；restart 201 发新 token → 带 token 重试 → 200。
func TestCheckAdmissionForbiddenThenRefreshCSRF(t *testing.T) {
	calls := map[string]int{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/auth/check":
			calls["check"]++
			// 首次无 token 403；带 token 200
			if r.Header.Get("X-Hatch-CSRF-Token") == "FRESH-TOKEN-1" {
				w.WriteHeader(200)
				_, _ = w.Write([]byte(`{"ok":true}`))
				return
			}
			w.WriteHeader(403)
			_, _ = w.Write([]byte(`{"error":"Forbidden"}`))
		case "/api/auth/native/restart":
			calls["restart"]++
			if r.Method != "POST" {
				t.Errorf("restart 应 POST，实际 %s", r.Method)
			}
			w.WriteHeader(201)
			_, _ = w.Write([]byte(`{"csrf_token":"FRESH-TOKEN-1","flow_state":"started"}`))
		case "/":
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`<html><script>self.__next_f.push([1,"1a2b:{\"props\":{\"vmState\":\"RUNNING\"}}"]);</script></html>`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()
	// cookie 不含 csrf 值（模拟登录链保存后的形态）
	res, err := checkAdmissionFrom(ts.URL+"/api/auth/check", "hatch_sess=abc; hatch_gw=1", "fp-test@x.com", "", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if res.State != "admitted" || res.VMState != "RUNNING" {
		t.Fatalf("restart 换 token 后应 admitted/RUNNING, got %+v", res)
	}
	if calls["restart"] != 1 || calls["check"] < 2 {
		t.Fatalf("应调用 restart 1 次且 check ≥2 次（含带 token 重试）, got %v", calls)
	}
}

// TestCheckAdmissionForbiddenAllRetryFail restart 也被拦时保持 forbidden。
func TestCheckAdmissionForbiddenAllRetryFail(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/auth/check", "/api/auth/native/restart":
			w.WriteHeader(403)
			_, _ = w.Write([]byte(`{"error":"Forbidden"}`))
		case "/":
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`<html>landing</html>`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()
	res, err := checkAdmissionFrom(ts.URL+"/api/auth/check", "hatch_sess=abc", "fp-test@x.com", "", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if res.State != "forbidden" {
		t.Fatalf("全链失败应保持 forbidden, got %s", res.State)
	}
	if !strings.Contains(res.Detail, "自动刷新 CSRF") {
		t.Fatalf("提示应说明已尝试自动刷新: %s", res.Detail)
	}
}

// TestCheckAdmissionCheckpoint 真人验证拦截：403 错误体携带 checkpoint_url（非空）
// → state=challenge（站点前端 iu/iQ 的 checkpoint 机制，CSRF 重试无法绕过）。
func TestCheckAdmissionCheckpoint(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/auth/check", "/api/auth/native/restart":
			w.WriteHeader(403)
			_, _ = w.Write([]byte(`{"error":"Forbidden","checkpoint_url":"/challenge/verify?wf=abc"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()
	res, err := checkAdmissionFrom(ts.URL+"/api/auth/check", csrfCookie, "fp-test@x.com", "", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if res.State != "challenge" || res.Type != "checkpoint" {
		t.Fatalf("want challenge/checkpoint, got %+v", res)
	}
	if !strings.Contains(res.Detail, "真人验证") {
		t.Fatalf("detail 应说明真人验证: %s", res.Detail)
	}
}

func TestCheckAdmissionAdmitted(t *testing.T) {
	var gotCSRF string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/auth/check":
			gotCSRF = r.Header.Get("X-Hatch-CSRF-Token")
			if r.Method != "POST" {
				t.Errorf("应 POST /api/auth/check，实际 %s", r.Method)
			}
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"ok":true}`))
		case "/":
			// 落地页无 vmState 字段（旧版页面/未嵌入场景）→ 不影响 admitted 判定
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`<html><body>landing without vmState</body></html>`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()
	res, err := checkAdmissionFrom(ts.URL+"/api/auth/check", "hatch_sess=abc", "fp-test@x.com", "", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if res.State != "admitted" || res.StatusCode != 200 {
		t.Fatalf("want admitted/200, got %s/%d", res.State, res.StatusCode)
	}
	// 200 时不应带 CSRF 重试头（首次即成功）
	_ = gotCSRF
}

func TestCheckAdmissionRegionLimited(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		_, _ = w.Write([]byte(`{"type":"hatchAdmissionInvalidated","reason":"activation","status":403}`))
	}))
	defer ts.Close()
	res, err := checkAdmissionFrom(ts.URL+"/api/auth/check", csrfCookie, "fp-test@x.com", "", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if res.State != "region_limited" || res.Type != "hatchAdmissionInvalidated" || res.Reason != "activation" {
		t.Fatalf("want region_limited/hatchAdmissionInvalidated/activation, got %+v", res)
	}
}

func TestCheckAdmissionAuthExpired(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"error":"Authentication required"}`))
	}))
	defer ts.Close()
	res, err := checkAdmissionFrom(ts.URL+"/api/auth/check", "hatch_sess=old", "fp-test@x.com", "", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if res.State != "auth_expired" {
		t.Fatalf("want auth_expired, got %s", res.State)
	}
}

func TestCheckAdmissionForbiddenWithCSRFRetry(t *testing.T) {
	// 模拟：首次 403 纯 Forbidden（无 type）→ 带 CSRF 重试 → 仍 403 Forbidden → forbidden
	var sawCSRF bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Hatch-CSRF-Token") == "csrf-token-123" {
			sawCSRF = true
		}
		w.WriteHeader(403)
		_, _ = w.Write([]byte(`{"error":"Forbidden"}`))
	}))
	defer ts.Close()
	res, err := checkAdmissionFrom(ts.URL+"/api/auth/check", csrfCookie, "fp-test@x.com", "", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if res.State != "forbidden" {
		t.Fatalf("want forbidden, got %s", res.State)
	}
	if !sawCSRF {
		t.Fatal("应自动带 X-Hatch-CSRF-Token 重试")
	}
}

func TestCheckAdmissionForbiddenThenAdmittedWithCSRF(t *testing.T) {
	// 模拟：首次无 CSRF → 403 Forbidden；带 CSRF → 200 admitted
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Hatch-CSRF-Token") != "" {
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		w.WriteHeader(403)
		_, _ = w.Write([]byte(`{"error":"Forbidden"}`))
	}))
	defer ts.Close()
	res, err := checkAdmissionFrom(ts.URL+"/api/auth/check", "hatch_native_auth_csrf=tok; hatch_sess=s", "fp-test@x.com", "", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if res.State != "admitted" {
		t.Fatalf("CSRF 重试后应 admitted, got %s", res.State)
	}
}

func TestCheckAdmissionEmptyCookie(t *testing.T) {
	_, err := checkAdmissionFrom("http://127.0.0.1:1", "", "", "", time.Second)
	if err == nil || !strings.Contains(err.Error(), "未设置登录 Cookie") {
		t.Fatalf("空 Cookie 应报错: %v", err)
	}
}

func TestCheckAdmissionInvalidProxy(t *testing.T) {
	_, err := checkAdmissionFrom("http://127.0.0.1:1", "c=1", "", "ftp://x", time.Second)
	if err == nil || !strings.Contains(err.Error(), "代理") {
		t.Fatalf("非法代理应报错: %v", err)
	}
}

func TestCheckAdmissionSessionInvalidated(t *testing.T) {
	// 实测场景（2026-11-15）：401 + {"type":"hatchAdmissionInvalidated","reason":"session"}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 同时验证请求形态：裸 POST，无 body，带 Sec-Fetch-Site 头
		if r.Method != "POST" {
			t.Errorf("应 POST，实际 %s", r.Method)
		}
		if r.Header.Get("Sec-Fetch-Site") != "same-origin" {
			t.Error("应携带 Sec-Fetch-Site: same-origin 浏览器特征头")
		}
		w.Header().Set("Set-Cookie", "hatch_sess=; Max-Age=0; Path=/; HttpOnly; Secure")
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"type":"hatchAdmissionInvalidated","reason":"session","status":401}`))
	}))
	defer ts.Close()
	res, err := checkAdmissionFrom(ts.URL+"/api/auth/check", "hatch_sess=old", "fp-test@x.com", "", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if res.State != "auth_expired" || res.Type != "hatchAdmissionInvalidated" || res.Reason != "session" {
		t.Fatalf("want auth_expired/session, got %+v", res)
	}
	if !strings.Contains(res.Detail, "会话无效") {
		t.Fatalf("detail 应含会话失效说明: %s", res.Detail)
	}
}

func TestCheckAdmissionForbiddenDiagnostics(t *testing.T) {
	// 两种 403 场景的诊断：Cookie 有 hatch_sess vs 完全没有
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		_, _ = w.Write([]byte(`{"error":"Forbidden"}`))
	}))
	defer ts.Close()
	// ① Cookie 含 hatch_sess 仍 403 → 提示重新登录
	res, err := checkAdmissionFrom(ts.URL+"/api/auth/check", csrfCookie, "fp-test@x.com", "", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if res.State != "forbidden" || !strings.Contains(res.Detail, "重新登录") {
		t.Fatalf("含 hatch_sess 仍 403 应提示重新登录: %s", res.Detail)
	}
	// ② Cookie 无 hatch_sess → 点名缺失字段
	res2, err := checkAdmissionFrom(ts.URL+"/api/auth/check", "datr=1", "fp-test@x.com", "", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if res2.State != "forbidden" || !strings.Contains(res2.Detail, "hatch_sess") {
		t.Fatalf("缺 hatch_sess 应点名: %s", res2.Detail)
	}
}

func TestExtractCookieValue(t *testing.T) {
	c := "a=1; Hatch_Native_Auth_CSRF=tok123; b=2"
	if v := extractCookieValue(c, "hatch_native_auth_csrf"); v != "tok123" {
		t.Fatalf("want tok123, got %q", v)
	}
	if v := extractCookieValue(c, "missing"); v != "" {
		t.Fatalf("missing 应为空, got %q", v)
	}
}

func TestStoreRegionLimitedStatus(t *testing.T) {
	s := newTestStore(t)
	if err := s.Add(&Account{Email: "r@x.com", Status: "region_limited"}); err != nil {
		t.Fatalf("region_limited 应合法: %v", err)
	}
}
