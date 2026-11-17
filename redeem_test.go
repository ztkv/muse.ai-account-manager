package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const testVMID = "1b2d3e4f-5a6b-4c7d-8e9f-0a1b2c3d4e5f"

// newRedeemMock 模拟 muse.ai 兑换链全部端点，记录请求序列与关键 body。
func newRedeemMock(t *testing.T, redeemBody string, redeemStatus int) (*httptest.Server, *[]string) {
	t.Helper()
	var mu sync.Mutex
	var order []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		order = append(order, r.Method + " " + r.URL.Path)
		mu.Unlock()
		// 全链请求都必须带浏览器特征头（实测缺 Sec-Fetch 会被边缘层 403）
		if r.Header.Get("Sec-Fetch-Site") == "" {
			t.Errorf("%s %s 缺少 Sec-Fetch-Site 头", r.Method, r.URL.Path)
		}
		switch r.URL.Path {
		case "/": // /?aymh_complete=1
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(`<html><script>self.__next_f.push([1,"abc:{\\"props\\":{\\"vmId\\":\\"` + testVMID + `\\"}}"]);</script></html>`))
		case "/api/hatch/vm/wake":
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"ok":true}`))
		case "/api/hatch/stateful-sharing":
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"ok":true}`))
		case "/api/hatch/invite-code/redeem":
			w.WriteHeader(redeemStatus)
			_, _ = w.Write([]byte(redeemBody))
		case "/api/hatch/invite":
			if r.Method != "POST" {
				t.Errorf("取回自己的码应为 POST，实际 %s", r.Method)
			}
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"code":"OWN9XY2","invite_state":"ELIGIBLE"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ts.Close)
	return ts, &order
}

func redeemOrder(ts *httptest.Server, order *[]string) string {
	return ts.URL
}

func TestRedeemFullChainSuccess(t *testing.T) {
	ts, order := newRedeemMock(t, `{"success":true,"redemptionStatus":"redeemed"}`, 200)
	out, err := redeemChainFrom(ts.URL, "hatch_sess=abc", "DEMOXY", "fp-test@x.com", "", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if out.State != "redeemed" {
		t.Fatalf("want redeemed, got %+v", out)
	}
	if out.OwnCode != "OWN9XY2" {
		t.Fatalf("应取回自己的码 OWN9XY2, got %q", out.OwnCode)
	}
	if out.VMID != testVMID {
		t.Fatalf("vmId 解析错误: %q", out.VMID)
	}
	// 链序校验：落地页 → wake → sharing → redeem → invite
	want := []string{"GET /", "POST /api/hatch/vm/wake", "POST /api/hatch/stateful-sharing",
		"POST /api/hatch/invite-code/redeem", "POST /api/hatch/invite"}
	if strings.Join(*order, ",") != strings.Join(want, ",") {
		t.Fatalf("链序错误: %v", *order)
	}
	if !strings.Contains(out.Detail, "OWN9XY2") {
		t.Fatalf("detail 应含自己的码: %s", out.Detail)
	}
}

func TestRedeemWakeBodyContract(t *testing.T) {
	// 校验 wake body 契约：wake_trigger=mount / away_ms=0 / retry_count=0 + vm_id
	var wakeBody string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			_, _ = w.Write([]byte(`{"vmId":"` + testVMID + `"}`))
		case "/api/hatch/vm/wake":
			buf := make([]byte, 1024)
			n, _ := r.Body.Read(buf)
			wakeBody = string(buf[:n])
			w.WriteHeader(200)
		case "/api/hatch/stateful-sharing":
			w.WriteHeader(200)
		case "/api/hatch/invite-code/redeem":
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"success":false,"reason":"invalid"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()
	_, err := redeemChainFrom(ts.URL, "hatch_sess=abc", "AB12CD", "fp-test@x.com", "", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	for _, frag := range []string{`"wake_trigger":"mount"`, `"away_ms":0`, `"retry_count":0`, `"` + testVMID + `"`} {
		if !strings.Contains(wakeBody, frag) {
			t.Fatalf("wake body 缺 %s: %s", frag, wakeBody)
		}
	}
	if !strings.Contains(wakeBody, `"connect_attempt_id":"`) || !strings.Contains(wakeBody, `"open_id":"`) {
		t.Fatalf("wake body 应含 connect_attempt_id/open_id uuid: %s", wakeBody)
	}
}

func TestRedeemAlreadyRedeemedAndUsedUp(t *testing.T) {
	for _, reason := range []string{"already_redeemed", "used_up"} {
		ts, order := newRedeemMock(t, `{"success":false,"reason":"`+reason+`"}`, 200)
		out, err := redeemChainFrom(ts.URL, "hatch_sess=abc", "ZZZZZZ", "fp-test@x.com", "", time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if out.State != reason {
			t.Fatalf("want %s, got %+v", reason, out)
		}
		// 失败时不应请求取码
		joined := strings.Join(*order, ",")
		_ = joined
		if len(*order) != 4 {
			t.Fatalf("失败链不应含取码步骤: %v", *order)
		}
		ts.Close()
		_ = redeemOrder(ts, order)
	}
}

func TestRedeemInvalidWithDetail(t *testing.T) {
	ts, _ := newRedeemMock(t, `{"success":false,"reason":"invalid","detail":"この招待コードは存在しません"}`, 200)
	defer ts.Close()
	out, err := redeemChainFrom(ts.URL, "hatch_sess=abc", "NOPE00", "fp-test@x.com", "", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if out.State != "invalid" || !strings.Contains(out.Detail, "この招待コード") {
		t.Fatalf("invalid 应保留服务端 detail: %+v", out)
	}
}

func TestRedeemAuthExpired(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			_, _ = w.Write([]byte(`{"vmId":"` + testVMID + `"}`))
		case "/api/hatch/vm/wake", "/api/hatch/stateful-sharing":
			w.WriteHeader(200)
		case "/api/hatch/invite-code/redeem":
			w.WriteHeader(401)
			_, _ = w.Write([]byte(`{"type":"hatchAdmissionInvalidated","reason":"session"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()
	out, err := redeemChainFrom(ts.URL, "hatch_sess=old", "AB12CD", "fp-test@x.com", "", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if out.State != "auth_expired" || !strings.Contains(out.Detail, "重新登录") {
		t.Fatalf("401 应映射 auth_expired: %+v", out)
	}
}

func TestRedeemNoVMID(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`<html><body>landing without vm</body></html>`))
	}))
	defer ts.Close()
	out, err := redeemChainFrom(ts.URL, "hatch_sess=abc", "AB12CD", "fp-test@x.com", "", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if out.State != "error" || !strings.Contains(out.Detail, "vmId") {
		t.Fatalf("无 vmId 应报 error: %+v", out)
	}
}

func TestRedeemWakeFailureTolerated(t *testing.T) {
	// 协议语义：wake 尽力而为——wake 500 仍继续兑换
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			_, _ = w.Write([]byte(`{"vmId":"` + testVMID + `"}`))
		case "/api/hatch/vm/wake":
			w.WriteHeader(500)
		case "/api/hatch/stateful-sharing":
			w.WriteHeader(500)
		case "/api/hatch/invite-code/redeem":
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"success":true}`))
		case "/api/hatch/invite":
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"code":"OWN123"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()
	out, err := redeemChainFrom(ts.URL, "hatch_sess=abc", "AB12CD", "fp-test@x.com", "", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if out.State != "redeemed" || out.OwnCode != "OWN123" {
		t.Fatalf("wake/sharing 失败应被容忍: %+v", out)
	}
}

func TestRedeemValidations(t *testing.T) {
	// 空 Cookie
	_, err := redeemChainFrom("http://127.0.0.1:1", "", "AB12CD", "", "", time.Second)
	if err == nil || !strings.Contains(err.Error(), "未设置登录 Cookie") {
		t.Fatalf("空 Cookie 应报错: %v", err)
	}
	// 空邀请码
	_, err = redeemChainFrom("http://127.0.0.1:1", "hatch_sess=abc", "  ", "", "", time.Second)
	if err == nil || !strings.Contains(err.Error(), "邀请码不能为空") {
		t.Fatalf("空邀请码应报错: %v", err)
	}
	// 非法代理
	_, err = redeemChainFrom("http://127.0.0.1:1", "hatch_sess=abc", "AB12CD", "", "ftp://bad", time.Second)
	if err == nil || !strings.Contains(err.Error(), "代理") {
		t.Fatalf("非法代理应报错: %v", err)
	}
}

func TestRedeemCodeNormalizedUpper(t *testing.T) {
	var gotCode string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			_, _ = w.Write([]byte(`{"vmId":"` + testVMID + `"}`))
		case "/api/hatch/vm/wake", "/api/hatch/stateful-sharing":
			w.WriteHeader(200)
		case "/api/hatch/invite-code/redeem":
			buf := make([]byte, 512)
			n, _ := r.Body.Read(buf)
			gotCode = string(buf[:n])
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"success":false,"reason":"invalid"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()
	_, _ = redeemChainFrom(ts.URL, "hatch_sess=abc", "ab12cd", "fp-test@x.com", "", time.Second)
	if !strings.Contains(gotCode, `"code":"AB12CD"`) {
		t.Fatalf("邀请码应规范化为大写: %s", gotCode)
	}
}
