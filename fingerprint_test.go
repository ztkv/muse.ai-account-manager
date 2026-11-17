package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

var uaChromeRe = regexp.MustCompile(`Chrome/(\d+)\.0\.0\.0 Safari`)

// TestFingerprintStability 同一标识每次派生的指纹必须完全一致（哈希即存储，重启不变）。
func TestFingerprintStability(t *testing.T) {
	ids := []string{"alpha@mail.example", "beta@mail.example", "a1b2c3d4-uuid-id", "用户中文标识@x.com"}
	for _, id := range ids {
		a := fingerprintFor(id)
		for i := 0; i < 5; i++ {
			b := fingerprintFor(id)
			if *a != *b {
				t.Fatalf("标识 %q 指纹不稳定: %+v vs %+v", id, a, b)
			}
		}
	}
}

// TestFingerprintDistinctAccounts 不同账号指纹高概率互不相同（含 UA 与语言多样性）。
func TestFingerprintDistinctAccounts(t *testing.T) {
	type key struct{ ua, lang, chua, plat string }
	seen := map[key]int{}
	uaSeen := map[string]int{}
	const n = 30
	for i := 0; i < n; i++ {
		fp := fingerprintFor("acct" + string(rune('a'+i)) + "@example.com")
		seen[key{fp.UserAgent, fp.AcceptLanguage, fp.SecChUA, fp.SecChUAPlatform}]++
		uaSeen[fp.UserAgent]++
	}
	if len(seen) < 18 {
		t.Fatalf("30 个账号应有 ≥18 种不同指纹组合，实际 %d", len(seen))
	}
	if len(uaSeen) < 10 {
		t.Fatalf("30 个账号应有 ≥10 种不同 UA，实际 %d", len(uaSeen))
	}
}

// TestFingerprintConsistency 指纹内部一致性：sec-ch-ua 版本 = UA Chrome 版本；
// UA 平台片段与 sec-ch-ua-platform 严格对应。
func TestFingerprintConsistency(t *testing.T) {
	platOfUA := map[string]string{
		"Windows NT 10.0; Win64; x64":      "Windows",
		"Macintosh; Intel Mac OS X 10_15_7": "macOS",
		"X11; Linux x86_64":                 "Linux",
	}
	for _, id := range []string{"a@x.com", "b@x.com", "c@x.com", "d@x.com", "e@x.com"} {
		fp := fingerprintFor(id)
		m := uaChromeRe.FindStringSubmatch(fp.UserAgent)
		if m == nil {
			t.Fatalf("UA 格式错误: %q", fp.UserAgent)
		}
		if m[1] != fp.ChromeMajor {
			t.Fatalf("UA 版本 %s 与 ChromeMajor %s 不一致", m[1], fp.ChromeMajor)
		}
		if !strings.Contains(fp.SecChUA, `v="`+fp.ChromeMajor+`"`) {
			t.Fatalf("sec-ch-ua 版本应与 UA 一致: %q / %q", fp.SecChUA, fp.ChromeMajor)
		}
		matched := false
		for frag, plat := range platOfUA {
			if strings.Contains(fp.UserAgent, "("+frag+")") {
				if fp.SecChUAPlatform != `"`+plat+`"` {
					t.Fatalf("平台不一致: UA 含 %q 但 platform=%q", frag, fp.SecChUAPlatform)
				}
				matched = true
			}
		}
		if !matched {
			t.Fatalf("UA 平台片段不在池内: %q", fp.UserAgent)
		}
		if fp.AcceptLanguage == "" {
			t.Fatal("AcceptLanguage 不应为空")
		}
	}
}

// TestFingerprintDefaultEmpty 空标识 → 默认指纹（与旧版固定头一致，行为不变）。
func TestFingerprintDefaultEmpty(t *testing.T) {
	for _, id := range []string{"", "   "} {
		fp := fingerprintFor(id)
		def := defaultFingerprint()
		if *fp != *def {
			t.Fatalf("空标识 %q 应为默认指纹: %+v vs %+v", id, fp, def)
		}
	}
	if defaultFingerprint().UserAgent != defaultInviteUA {
		t.Fatal("默认指纹 UA 应等于 defaultInviteUA（兼容旧行为）")
	}
}

// TestFingerprintCaseInsensitive 标识大小写与首尾空白不影响派生结果。
func TestFingerprintCaseInsensitive(t *testing.T) {
	a := fingerprintFor("User@X.COM")
	b := fingerprintFor("  user@x.com ")
	if *a != *b {
		t.Fatalf("大小写/空白规范化失败: %+v vs %+v", a, b)
	}
}

// TestApplyFingerprintHeaders 指纹头写入与 nil 回退。
func TestApplyFingerprintHeaders(t *testing.T) {
	fp := fingerprintFor("hdr@x.com")
	h := http.Header{}
	applyFingerprintHeaders(h, fp)
	if h.Get("User-Agent") != fp.UserAgent || h.Get("Accept-Language") != fp.AcceptLanguage {
		t.Fatalf("指纹头未正确写入: %+v", h)
	}
	if h.Get("sec-ch-ua") != fp.SecChUA || h.Get("sec-ch-ua-platform") != fp.SecChUAPlatform {
		t.Fatalf("sec-ch-ua 未正确写入: %+v", h)
	}
	if h.Get("sec-ch-ua-mobile") != "?0" {
		t.Fatalf("sec-ch-ua-mobile 应为 ?0: %q", h.Get("sec-ch-ua-mobile"))
	}
	// nil → 默认指纹
	h2 := http.Header{}
	applyFingerprintHeaders(h2, nil)
	if h2.Get("User-Agent") != defaultInviteUA {
		t.Fatalf("nil 指纹应回退默认 UA: %q", h2.Get("User-Agent"))
	}
}

// TestAccountIdentity 指纹标识取值：优先邮箱，无邮箱退回 ID。
func TestAccountIdentity(t *testing.T) {
	if got := accountIdentity(&Account{ID: "id1", Email: "a@x.com"}); got != "a@x.com" {
		t.Fatalf("应优先邮箱, got %q", got)
	}
	if got := accountIdentity(&Account{ID: "id2"}); got != "id2" {
		t.Fatalf("无邮箱应退回 ID, got %q", got)
	}
	if accountIdentity(nil) != "" {
		t.Fatal("nil 账号应为空标识")
	}
}

// TestFingerprintThreadedThroughRedeemChain 端到端：兑换链每一步都用同一账号指纹；
// 不同账号指纹不同（至少 UA 或语言其一不同）。
func TestFingerprintThreadedThroughRedeemChain(t *testing.T) {
	var mu sync.Mutex
	run := func(identity string) map[string]string {
		seen := map[string]string{} // path → User-Agent
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			seen[r.URL.Path] = r.Header.Get("User-Agent")
			seen[r.URL.Path+"|lang"] = r.Header.Get("Accept-Language")
			mu.Unlock()
			switch r.URL.Path {
			case "/":
				_, _ = w.Write([]byte(`{"vmId":"` + testVMID + `"}`))
			case "/api/hatch/vm/wake", "/api/hatch/stateful-sharing":
				w.WriteHeader(200)
			case "/api/hatch/invite-code/redeem":
				w.WriteHeader(200)
				_, _ = w.Write([]byte(`{"success":true}`))
			case "/api/hatch/invite":
				w.WriteHeader(200)
				_, _ = w.Write([]byte(`{"code":"OWNFP1"}`))
			default:
				http.NotFound(w, r)
			}
		}))
		defer ts.Close()
		out, err := redeemChainFrom(ts.URL, "hatch_sess=abc", "AB12CD", identity, "", time.Second)
		if err != nil {
			t.Fatalf("%s: %v", identity, err)
		}
		if out.State != "redeemed" {
			t.Fatalf("%s: 兑换应成功, got %+v", identity, out)
		}
		return seen
	}

	want := map[string]bool{
		"/": true, "/api/hatch/vm/wake": true, "/api/hatch/stateful-sharing": true,
		"/api/hatch/invite-code/redeem": true, "/api/hatch/invite": true,
	}
	seenA := run("fp-a@example.com")
	fpA := fingerprintFor("fp-a@example.com")
	for path := range want {
		if seenA[path] != fpA.UserAgent {
			t.Fatalf("路径 %s UA 应为账号指纹, got %q want %q", path, seenA[path], fpA.UserAgent)
		}
		if seenA[path+"|lang"] != fpA.AcceptLanguage {
			t.Fatalf("路径 %s Accept-Language 应为账号指纹, got %q", path, seenA[path+"|lang"])
		}
	}
	seenB := run("fp-b@example.com")
	fpB := fingerprintFor("fp-b@example.com")
	if seenB["/api/hatch/invite-code/redeem"] != fpB.UserAgent {
		t.Fatalf("账号 B 兑换步 UA 应为其指纹, got %q want %q", seenB["/api/hatch/invite-code/redeem"], fpB.UserAgent)
	}
	if fpA.UserAgent == fpB.UserAgent && fpA.AcceptLanguage == fpB.AcceptLanguage {
		t.Fatal("两个测试账号指纹完全相同（选择标识需调整）")
	}
}

// TestFingerprintThreadedThroughCheck 体检链：auth/check、restart、落地页都用同一账号指纹。
func TestFingerprintThreadedThroughCheck(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]string{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen[r.URL.Path] = r.Header.Get("User-Agent")
		mu.Unlock()
		switch r.URL.Path {
		case "/api/auth/check":
			// 首次 403 → restart 换 token → 带 token 200（覆盖两级重试全链）
			if r.Header.Get("X-Hatch-CSRF-Token") == "FRESH-FP" {
				w.WriteHeader(200)
				_, _ = w.Write([]byte(`{"ok":true}`))
				return
			}
			w.WriteHeader(403)
			_, _ = w.Write([]byte(`{"error":"Forbidden"}`))
		case "/api/auth/native/restart":
			w.WriteHeader(201)
			_, _ = w.Write([]byte(`{"csrf_token":"FRESH-FP"}`))
		case "/":
			_, _ = w.Write([]byte(`<html><script>self.__next_f.push([1,"x:{\"props\":{\"vmState\":\"RUNNING\"}}"]);</script></html>`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()
	identity := "check-fp@example.com"
	fp := fingerprintFor(identity)
	res, err := checkAdmissionFrom(ts.URL+"/api/auth/check", "hatch_sess=abc", identity, "", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if res.State != "admitted" || res.VMState != "RUNNING" {
		t.Fatalf("应 admitted/RUNNING, got %+v", res)
	}
	for _, path := range []string{"/api/auth/check", "/api/auth/native/restart", "/"} {
		if seen[path] != fp.UserAgent {
			t.Fatalf("路径 %s UA 应为账号指纹, got %q want %q", path, seen[path], fp.UserAgent)
		}
	}
}

// TestLoginUsesAccountFingerprint 验证码登录链请求头随邮箱派生指纹。
func TestLoginUsesAccountFingerprint(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]string{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen[r.URL.Path] = r.Header.Get("User-Agent")
		mu.Unlock()
		switch r.URL.Path {
		case "/api/auth/native/restart":
			w.WriteHeader(201)
			_, _ = w.Write([]byte(`{"csrf_token":"T1"}`))
		case "/api/auth/native/send-otp":
			w.WriteHeader(201)
			_, _ = w.Write([]byte(`{"registration_state":"otp_sent","flow":"login","has_muse_account":true}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()
	pool := newLoginSessionsWithBase(ts.URL)
	email := "login-fp@Example.COM"
	if _, _, err := pool.LoginSendOTP(email, ""); err != nil {
		t.Fatalf("登录链失败: %v", err)
	}
	fp := fingerprintFor(email)
	for _, path := range []string{"/api/auth/native/restart", "/api/auth/native/send-otp"} {
		if seen[path] != fp.UserAgent {
			t.Fatalf("路径 %s UA 应为邮箱派生指纹, got %q want %q", path, seen[path], fp.UserAgent)
		}
	}
}

// TestFingerprintJSONSerializable 指纹结构可直接 JSON 序列化（诊断/日志用）。
func TestFingerprintJSONSerializable(t *testing.T) {
	b, err := json.Marshal(fingerprintFor("json@x.com"))
	if err != nil || !strings.Contains(string(b), "UserAgent") {
		t.Fatalf("指纹应可 JSON 序列化: %s %v", b, err)
	}
}
