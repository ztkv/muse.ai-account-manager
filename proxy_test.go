package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestEffectiveProxyPriority(t *testing.T) {
	t.Setenv("MUSE_PROXY", "socks5://env:1080")
	cases := []struct {
		account, global, want string
	}{
		{"http://acc:1", "http://g:1", "http://acc:1"},  // 账号优先
		{"", "http://g:1", "http://g:1"},                // 全局次之
		{"  ", "http://g:1", "http://g:1"},              // 空白账号代理视为空
		{"", "", "socks5://env:1080"},                   // 环境变量兜底
		{"http://acc:1", "", "http://acc:1"},            // 无全局时账号仍生效
	}
	for _, c := range cases {
		if got := effectiveProxy(c.account, c.global); got != c.want {
			t.Fatalf("effectiveProxy(%q,%q)=%q, want %q", c.account, c.global, got, c.want)
		}
	}
}

func TestValidateProxyURL(t *testing.T) {
	for _, ok := range []string{"socks5://127.0.0.1:10808", "http://1.2.3.4:8080", "https://p.example.com"} {
		if err := validateProxyURL(ok); err != nil {
			t.Fatalf("%q 应合法: %v", ok, err)
		}
	}
	for _, bad := range []string{"ftp://x", "127.0.0.1:10808", "://", "abc"} {
		if err := validateProxyURL(bad); err == nil {
			t.Fatalf("%q 应非法", bad)
		}
	}
}

func TestProxyTestDirectOK(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer ts.Close()
	res := testProxyFrom(ts.URL, "", time.Second)
	if !res.OK || res.Status != 200 {
		t.Fatalf("直连应成功: %+v", res)
	}
	if res.LatencyMs < 0 {
		t.Fatalf("延迟不应为负: %+v", res)
	}
}

func TestProxyTestAnyStatusIsOK(t *testing.T) {
	// 收到任何 HTTP 响应（含 403）即视为代理链路连通
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
	}))
	defer ts.Close()
	res := testProxyFrom(ts.URL, "", time.Second)
	if !res.OK || res.Status != 403 {
		t.Fatalf("403 响应也应视为连通: %+v", res)
	}
}

func TestProxyTestInvalidProxy(t *testing.T) {
	res := testProxyFrom("http://127.0.0.1:1", "ftp://bad", time.Second)
	if res.OK || res.Error == "" {
		t.Fatalf("非法代理应报错: %+v", res)
	}
	if !strings.Contains(res.Error, "代理地址无效") {
		t.Fatalf("应提示代理地址无效: %s", res.Error)
	}
}

func TestProxyTestUnreachableHint(t *testing.T) {
	res := testProxyFrom("http://127.0.0.1:1", "", 500*time.Millisecond)
	if res.OK {
		t.Fatal("不可达目标不应 OK")
	}
	if !strings.Contains(res.Error, "提示") {
		t.Fatalf("网络失败应附代理配置提示: %s", res.Error)
	}
}
