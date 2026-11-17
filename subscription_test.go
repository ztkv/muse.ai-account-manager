package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// mockFlightSub 模拟订阅 Server Action 端点（POST /）。
func mockFlightSub(t *testing.T, status int, flight string) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Next-Action") != subActionID {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Accept") != "text/x-component" {
			t.Error("订阅查询应带 Accept: text/x-component")
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(flight))
	}))
	t.Cleanup(ts.Close)
	return ts
}

// 用户抓包的真实响应形态（2026-11-17，订阅 FREE + 加购额度 40000000）。
const realFlight = `0:{"a":"$@1","q":"","i":false}
1:{"success":true,"subscription":{"subscriptionType":"FREE","tier":{"tierId":"hatch_free","tierCode":"HATCH_FREE","isPaid":false},"usage":{"state":"METERED","percentUsed":0,"quotaStatus":"SUFFICIENT"},"topupBalance":40000000,"topupTotal":40000000,"topupEligible":true}}`

func TestFetchSubscriptionRealFlight(t *testing.T) {
	ts := mockFlightSub(t, http.StatusOK, realFlight)
	sub, err := fetchSubscriptionFrom(ts.URL, "hatch_sess=abc", "sub@x.com", "", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !sub.Success || sub.TierCode != "HATCH_FREE" || sub.SubscriptionType != "FREE" {
		t.Fatalf("层级解析错误: %+v", sub)
	}
	if sub.TopupTotal != 40000000 || sub.TopupBalance != 40000000 {
		t.Fatalf("加购额度解析错误: %+v", sub)
	}
	if !sub.HasRedeemed {
		t.Fatal("topupTotal>0 应派生 HasRedeemed=true")
	}
	if sub.PercentUsed != 0 || sub.QuotaStatus != "SUFFICIENT" || sub.State != "METERED" {
		t.Fatalf("用量解析错误: %+v", sub)
	}
	if ts.Config == nil {
		t.Fatal("unreachable")
	}
}

func TestFetchSubscriptionNoTopup(t *testing.T) {
	// topupTotal=0 → HasRedeemed=false（从未兑换/无加购）
	flight := `1:{"success":true,"subscription":{"subscriptionType":"FREE","tierCode":"HATCH_FREE","topupBalance":0,"topupTotal":0}}`
	ts := mockFlightSub(t, http.StatusOK, flight)
	sub, err := fetchSubscriptionFrom(ts.URL, "c=1", "sub@x.com", "", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if sub.HasRedeemed {
		t.Fatal("topupTotal=0 应 HasRedeemed=false")
	}
}

func TestFetchSubscriptionCheckpoint(t *testing.T) {
	ts := mockFlightSub(t, http.StatusForbidden, `{"type":"checkpointRequired","checkpoint_url":"https://auth.meta.com/checkpoints/1"}`)
	_, err := fetchSubscriptionFrom(ts.URL, "c=1", "sub@x.com", "", time.Second)
	if err == nil || !strings.Contains(err.Error(), "真人验证") {
		t.Fatalf("checkpoint 应报真人验证: %v", err)
	}
}

func TestFetchSubscriptionHTTPError(t *testing.T) {
	ts := mockFlightSub(t, http.StatusUnauthorized, `{"error":"session"}`)
	_, err := fetchSubscriptionFrom(ts.URL, "c=1", "sub@x.com", "", time.Second)
	if err == nil || !strings.Contains(err.Error(), "HTTP 401") {
		t.Fatalf("401 应报 HTTP 错误: %v", err)
	}
}

func TestFetchSubscriptionNoSubscriptionLine(t *testing.T) {
	// action ID 失效：200 但响应无 subscription 行（部署变更后的典型形态）
	ts := mockFlightSub(t, http.StatusOK, `0:{"a":"$@1"}
1:{"redirect":"/"}`)
	_, err := fetchSubscriptionFrom(ts.URL, "c=1", "sub@x.com", "", time.Second)
	if err == nil || !strings.Contains(err.Error(), "未返回数据") {
		t.Fatalf("无 subscription 应报 action ID 失效: %v", err)
	}
}

func TestFetchSubscriptionEmptyCookie(t *testing.T) {
	if _, err := fetchSubscriptionFrom("http://127.0.0.1:1", "", "sub@x.com", "", time.Second); err == nil {
		t.Fatal("空 Cookie 应报错")
	}
}

// TestFetchInviteWithSubscription 集成：邀请拉取链自动附带订阅信息（同步成功注入 snapshot）。
func TestFetchInviteWithSubscription(t *testing.T) {
	var mu = make(chan struct{}, 1)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/checkout/stripe/publishable-key":
			_, _ = w.Write([]byte(`{"publishable_key":"pk_live_X"}`))
		case r.URL.Path == "/api/hatch/invite":
			if r.Method == http.MethodPost {
				w.WriteHeader(200)
				_, _ = w.Write([]byte(`{"code":"DEMOXY","max_uses":30,"use_count":15}`))
				return
			}
			w.WriteHeader(200)
			_, _ = w.Write([]byte(mockInviteJSON))
		case r.URL.Path == "/" && r.Header.Get("Next-Action") == subActionID:
			w.WriteHeader(200)
			_, _ = w.Write([]byte(realFlight))
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()
	mu <- struct{}{} // 无实际并发用途，仅保持 channel 引用
	<-mu
	_, inv, raw, err := fetchInviteFullFrom(ts.URL, "hatch_sess=abc", "sub-int@x.com", "", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if inv.Subscription == nil || inv.Subscription.TopupTotal != 40000000 || !inv.Subscription.HasRedeemed {
		t.Fatalf("邀请链应附带订阅信息: %+v", inv.Subscription)
	}
	if !strings.Contains(string(raw), `"subscription"`) {
		t.Fatal("快照应包含 subscription 对象")
	}
}

// TestFetchInviteSubscriptionFailureNotBlocking 集成：订阅查询失败不阻断邀请主流程。
func TestFetchInviteSubscriptionFailureNotBlocking(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/checkout/stripe/publishable-key":
			_, _ = w.Write([]byte(`{}`))
		case r.URL.Path == "/api/hatch/invite":
			w.WriteHeader(200)
			_, _ = w.Write([]byte(mockInviteJSON))
		case r.URL.Path == "/":
			// 订阅端点 500
			w.WriteHeader(500)
			_, _ = w.Write([]byte(`{}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()
	_, inv, raw, err := fetchInviteFullFrom(ts.URL, "hatch_sess=abc", "sub-err@x.com", "", time.Second)
	if err != nil {
		t.Fatalf("订阅失败不应阻断邀请拉取: %v", err)
	}
	if inv.Subscription != nil {
		t.Fatal("订阅失败时 inv.Subscription 应为 nil")
	}
	if !strings.Contains(string(raw), "subscription_error") {
		t.Fatal("快照应记录 subscription_error")
	}
}