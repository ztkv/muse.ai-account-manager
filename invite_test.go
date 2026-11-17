package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const mockInviteJSON = `{
  "invite_state": "ELIGIBLE",
  "title": "邀请好友，获得额外使用额度",
  "code": "DEMOXY",
  "invite_link": "https://muse.ai/join",
  "max_uses": 30,
  "use_count": 2,
  "uses_remaining": 28,
  "has_redeemed_invite_code": false,
  "reward": {"title": "你已赚取 20 亿个 Muse 词元", "body": "2 位好友已兑现你的邀请码。"}
}`

// newMockMuse 模拟 muse.ai：先 publishable-key，再 GET/POST invite，记录请求顺序。
// invite 的 GET 与 POST 均返回同一个 mockInviteJSON（码已存在，POST 用于刷新计数）。
func newMockMuse(t *testing.T, pubKeyBody string, pubKeyStatus int) (*httptest.Server, *[]string) {
	var mu sync.Mutex
	var order []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		order = append(order, r.Method+" "+r.URL.Path)
		mu.Unlock()
		switch r.URL.Path {
		case "/api/checkout/stripe/publishable-key":
			if pubKeyStatus != 200 {
				w.WriteHeader(pubKeyStatus)
				_, _ = w.Write([]byte("blocked"))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(pubKeyBody))
		case "/api/hatch/invite":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(mockInviteJSON))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ts.Close)
	return ts, &order
}

// newInviteMock 精细模拟：GET/POST invite 分别可配置状态与响应体。
func newInviteMock(t *testing.T, getStatus int, getBody string, postStatus int, postBody string) (*httptest.Server, *[]string) {
	t.Helper()
	var mu sync.Mutex
	var order []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		order = append(order, r.Method+" "+r.URL.Path)
		mu.Unlock()
		switch r.URL.Path {
		case "/api/checkout/stripe/publishable-key":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"publishable_key":"pk_live_TESTKEY123"}`))
		case "/api/hatch/invite":
			w.Header().Set("Content-Type", "application/json")
			if r.Method == http.MethodPost {
				w.WriteHeader(postStatus)
				_, _ = w.Write([]byte(postBody))
				return
			}
			w.WriteHeader(getStatus)
			_, _ = w.Write([]byte(getBody))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ts.Close)
	return ts, &order
}

func TestFetchInviteFullFlow(t *testing.T) {
	ts, order := newMockMuse(t, `{"publishable_key":"pk_live_TESTKEY123"}`, 200)
	pubKey, inv, raw, err := fetchInviteFullFrom(ts.URL, "hatch_sess=abc123; hatch_gw=xyz", "fp-test@x.com", "", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	// 顺序：publishable-key → GET invite（展示信息）→ POST /（订阅查询，尽力而为）→ POST invite（刮码/实时计数）
	want := "GET /api/checkout/stripe/publishable-key,GET /api/hatch/invite,POST /,POST /api/hatch/invite"
	if strings.Join(*order, ",") != want {
		t.Fatalf("请求顺序错误: %v", *order)
	}
	if pubKey != "pk_live_TESTKEY123" {
		t.Fatalf("公钥提取错误: %q", pubKey)
	}
	if inv.Code != "DEMOXY" || inv.MaxUses != 30 || inv.UseCount != 2 || inv.UsesRemaining != 28 {
		t.Fatalf("解析错误: %+v", inv)
	}
	var snap map[string]any
	if err := json.Unmarshal(raw, &snap); err != nil {
		t.Fatal(err)
	}
	if snap["publishable_key"] != "pk_live_TESTKEY123" || snap["code"] != "DEMOXY" {
		t.Fatalf("快照合并错误: %v", snap)
	}
	if inv.PublishableKey != "" {
		t.Fatalf("MuseInvite 本体不应含 publishable_key（在快照层合并）: %v", inv.PublishableKey)
	}
}

// TestFetchInviteScratch 首次刮码：GET 返回 ELIGIBLE 且 code=null → POST 生成并返回邀请码。
func TestFetchInviteScratch(t *testing.T) {
	ts, order := newInviteMock(t,
		http.StatusOK, `{"invite_state":"ELIGIBLE","code":null,"max_uses":30,"use_count":0}`,
		http.StatusOK, `{"code":"NEWCD1","invite_link":"https://muse.ai/join","max_uses":30,"use_count":0,"share_text":"加入 muse.ai"}`)
	pubKey, inv, raw, err := fetchInviteFullFrom(ts.URL, "hatch_sess=abc", "scratch@test.com", "", time.Second)
	if err != nil {
		t.Fatalf("刮码应成功: %v", err)
	}
	if pubKey != "pk_live_TESTKEY123" {
		t.Fatalf("公钥错误: %q", pubKey)
	}
	if inv.Code != "NEWCD1" || inv.InviteState != "ELIGIBLE" || inv.ShareText != "加入 muse.ai" {
		t.Fatalf("刮码合并错误: %+v", inv)
	}
	if len(*order) != 4 {
		t.Fatalf("应为 4 次请求（含订阅查询尽力而为）: %v", *order)
	}
	var snap map[string]any
	_ = json.Unmarshal(raw, &snap)
	if snap["code"] != "NEWCD1" {
		t.Fatalf("快照应以 POST 实时数据为准: %v", snap["code"])
	}
}

// TestFetchInvitePostRefreshRealtime POST 的实时计数覆盖 GET 的陈旧计数。
func TestFetchInvitePostRefreshRealtime(t *testing.T) {
	// GET 陈旧：use_count=2；POST 实时：use_count=15
	ts, _ := newInviteMock(t,
		http.StatusOK, mockInviteJSON,
		http.StatusOK, `{"code":"DEMOXY","invite_link":"https://muse.ai/join","max_uses":30,"use_count":15,"share_text":null}`)
	_, inv, raw, err := fetchInviteFullFrom(ts.URL, "hatch_sess=abc", "live-count@test.example", "", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if inv.UseCount != 15 || inv.UsesRemaining != 15 {
		t.Fatalf("应以 POST 实时计数为准: use=%d remain=%d", inv.UseCount, inv.UsesRemaining)
	}
	var snap map[string]any
	_ = json.Unmarshal(raw, &snap)
	if snap["use_count"].(float64) != 15 {
		t.Fatalf("快照 use_count 应为 15: %v", snap["use_count"])
	}
}

// TestFetchInvitePostUnavailableNoCode 无码 + POST 502 → 明确分类错误（非「登录态失效」）。
func TestFetchInvitePostUnavailableNoCode(t *testing.T) {
	ts, _ := newInviteMock(t,
		http.StatusOK, `{"invite_state":"ELIGIBLE","code":null,"max_uses":30,"use_count":0}`,
		http.StatusBadGateway, `{"error":"Invite information is unavailable"}`)
	_, _, _, err := fetchInviteFullFrom(ts.URL, "hatch_sess=abc", "x@test.com", "", time.Second)
	if err == nil || !strings.Contains(err.Error(), "暂不可用") {
		t.Fatalf("POST 502 无码应报暂不可用: %v", err)
	}
}

// TestFetchInvitePostUnavailableWithCode 已有码 + POST 502 → 静默降级，保留 GET 数据。
func TestFetchInvitePostUnavailableWithCode(t *testing.T) {
	ts, _ := newInviteMock(t,
		http.StatusOK, mockInviteJSON,
		http.StatusBadGateway, `{"error":"Invite information is unavailable"}`)
	_, inv, raw, err := fetchInviteFullFrom(ts.URL, "hatch_sess=abc", "x@test.com", "", time.Second)
	if err != nil {
		t.Fatalf("有码时 POST 失败不应阻断: %v", err)
	}
	if inv.Code != "DEMOXY" {
		t.Fatalf("应保留 GET 码: %+v", inv)
	}
	var snap map[string]any
	_ = json.Unmarshal(raw, &snap)
	if snap["post_refresh_error"] == nil {
		t.Fatalf("快照应记录刷新失败原因: %v", snap)
	}
}

// TestFetchInviteExhausted EXHAUSTED 账号直接报额度用尽，不再发起 POST。
func TestFetchInviteExhausted(t *testing.T) {
	ts, order := newInviteMock(t,
		http.StatusOK, `{"invite_state":"EXHAUSTED","code":null,"max_uses":30,"use_count":30}`,
		http.StatusOK, `{}`)
	_, inv, _, err := fetchInviteFullFrom(ts.URL, "hatch_sess=abc", "ex@test.com", "", time.Second)
	if err == nil || !strings.Contains(err.Error(), "额度已用尽") {
		t.Fatalf("EXHAUSTED 应报额度用尽: %v", err)
	}
	if inv == nil || inv.InviteState != "EXHAUSTED" {
		t.Fatalf("应返回状态快照: %+v", inv)
	}
	if strings.Contains(strings.Join(*order, ","), "POST /api/hatch/invite") {
		t.Fatalf("EXHAUSTED 不应调 POST: %v", *order)
	}
}

// TestFetchInviteCheckpoint GET 403 + checkpoint_url → 真人验证错误。
func TestFetchInviteCheckpoint(t *testing.T) {
	ts, _ := newInviteMock(t,
		http.StatusForbidden, `{"type":"checkpointRequired","title":"Checkpoint required","checkpoint_url":"https://auth.meta.com/checkpoints/1","status":403}`,
		http.StatusOK, `{}`)
	_, _, _, err := fetchInviteFullFrom(ts.URL, "hatch_sess=abc", "cp@test.com", "", time.Second)
	if err == nil || !strings.Contains(err.Error(), "真人验证") {
		t.Fatalf("checkpoint 应报真人验证: %v", err)
	}
}

// TestFetchInvite401 GET 401 → 登录态失效错误。
func TestFetchInvite401(t *testing.T) {
	ts, _ := newInviteMock(t,
		http.StatusUnauthorized, `{"reason":"session_expired"}`,
		http.StatusOK, `{}`)
	_, _, _, err := fetchInviteFullFrom(ts.URL, "hatch_sess=old", "s@test.com", "", time.Second)
	if err == nil || !strings.Contains(err.Error(), "已失效") {
		t.Fatalf("401 应报登录失效: %v", err)
	}
}

func TestFetchInvitePubKeyJSONVariants(t *testing.T) {
	cases := []string{
		`{"publishable_key":"pk_live_A"}`,
		`{"publishableKey":"pk_live_B"}`,
		`{"key":"pk_live_C"}`,
		`pk_live_D_PLAINTEXT`,
		// muse.ai 实际响应格式（2026-11 实抓确认）
		`{"publishableKey":"pk_live_51TP6PeHHV3oDWH6YuDH4HuWS5XDK8W7MqlmMkVZ6lBTT8ZxjQhQg7NSMIPRmB0WM0MR5VL7Vu5kHTNZVqSCwLPDq00hyVo0bga"}`,
	}
	wants := []string{
		"pk_live_A",
		"pk_live_B",
		"pk_live_C",
		"pk_live_D_PLAINTEXT",
		"pk_live_51TP6PeHHV3oDWH6YuDH4HuWS5XDK8W7MqlmMkVZ6lBTT8ZxjQhQg7NSMIPRmB0WM0MR5VL7Vu5kHTNZVqSCwLPDq00hyVo0bga",
	}
	for i, body := range cases {
		ts, _ := newMockMuse(t, body, 200)
		pubKey, _, _, err := fetchInviteFullFrom(ts.URL, "c=1", "fp-test@x.com", "", time.Second)
		if err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		if pubKey != wants[i] {
			t.Fatalf("case %d: want %q got %q", i, wants[i], pubKey)
		}
	}
}

func TestFetchInvitePubKeyFailureDoesNotBlock(t *testing.T) {
	ts, order := newMockMuse(t, "", http.StatusInternalServerError)
	pubKey, inv, raw, err := fetchInviteFullFrom(ts.URL, "hatch_sess=abc", "fp-test@x.com", "", time.Second)
	if err != nil {
		t.Fatalf("前置密钥失败不应阻断邀请拉取: %v", err)
	}
	if pubKey != "" {
		t.Fatalf("前置失败时公钥应为空: %q", pubKey)
	}
	if inv.Code != "DEMOXY" {
		t.Fatal("邀请应正常解析")
	}
	var snap map[string]any
	_ = json.Unmarshal(raw, &snap)
	if _, ok := snap["publishable_key"]; ok {
		t.Fatal("前置失败时快照不应含 publishable_key")
	}
	// 前置失败后仍应发出 GET invite + POST /(订阅) + POST invite（共 4 次请求）
	if len(*order) != 4 {
		t.Fatalf("仍应请求四次: %v", *order)
	}
}

func TestFetchInviteEmptyCookie(t *testing.T) {
	_, _, _, err := fetchInviteFullFrom("http://127.0.0.1:1", "", "fp-test@x.com", "", time.Second)
	if err == nil || !strings.Contains(err.Error(), "未设置登录 Cookie") {
		t.Fatalf("空 Cookie 应报明确错误: %v", err)
	}
}

func TestFetchInviteHTTPStatus(t *testing.T) {
	ts, _ := newMockMuse(t, "", http.StatusForbidden)
	// pubkey 403 应降级，不应阻断
	_, _, _, err := fetchInviteFullFrom(ts.URL, "hatch_sess=abc", "fp-test@x.com", "", time.Second)
	if err != nil {
		t.Fatalf("pubkey 403 应降级，不应阻断: %v", err)
	}
}

func TestFetchInviteBadJSON(t *testing.T) {
	var mu sync.Mutex
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if strings.Contains(r.URL.Path, "publishable-key") {
			_, _ = w.Write([]byte(`{"publishable_key":"pk_live_X"}`))
			return
		}
		_, _ = w.Write([]byte("<html>login page</html>"))
	}))
	defer ts.Close()
	_, _, _, err := fetchInviteFullFrom(ts.URL, "hatch_sess=abc", "fp-test@x.com", "", time.Second)
	if err == nil || !strings.Contains(err.Error(), "JSON") {
		t.Fatalf("HTML 响应应报 JSON 错误: %v", err)
	}
}

// TestFetchInviteMissingCode 未知状态无码且 POST 也无码 → 明确「刮码后仍未返回邀请码」。
func TestFetchInviteMissingCode(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "publishable-key") {
			_, _ = w.Write([]byte(`{"publishable_key":"pk_live_X"}`))
			return
		}
		_, _ = w.Write([]byte(`{"invite_state":"SESSION_EXPIRED"}`))
	}))
	defer ts.Close()
	_, _, _, err := fetchInviteFullFrom(ts.URL, "hatch_sess=old", "fp-test@x.com", "", time.Second)
	if err == nil || !strings.Contains(err.Error(), "未返回邀请码") {
		t.Fatalf("未知状态无码应报未返回邀请码: %v", err)
	}
}

func TestFetchInviteInvalidProxy(t *testing.T) {
	_, _, _, err := fetchInviteFullFrom("http://127.0.0.1:1", "c=1", "fp-test@x.com", "ftp://bad", time.Second)
	if err == nil || !strings.Contains(err.Error(), "代理") {
		t.Fatalf("非法代理应报错: %v", err)
	}
}