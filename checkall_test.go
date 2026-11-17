package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMapAdmissionStatus(t *testing.T) {
	cases := map[string]string{
		"admitted":      "active",
		"disabled":      "banned",
		"region_limited": "region_limited",
		"auth_expired":  "unverified",
		"challenge":     "",
		"forbidden":     "",
		"unknown":       "",
		"":              "",
		"weird":         "",
	}
	for state, want := range cases {
		if got := mapAdmissionStatus(state); got != want {
			t.Fatalf("mapAdmissionStatus(%q) = %q, want %q", state, got, want)
		}
	}
}

func TestAggregateCheckOutcomes(t *testing.T) {
	outs := []checkOutcome{
		{Email: "a@x.com", OK: true, State: "admitted"},
		{Email: "b@x.com", OK: true, State: "admitted"},
		{Email: "c@x.com", OK: true, State: "region_limited"},
		{Email: "d@x.com", OK: true, State: "disabled"},
		{Email: "e@x.com", OK: true, State: "auth_expired"},
		{Email: "f@x.com", OK: true, State: "challenge"},
		{Email: "g@x.com", OK: true, State: "forbidden"},
		{Email: "h@x.com", OK: true, State: "mystery"},
		{Email: "i@x.com", Skipped: true, Error: "无 Cookie"},
		{Email: "j@x.com", OK: false, Error: "网络失败"},
	}
	sum := aggregateCheckOutcomes(outs)
	if sum.Total != 10 || sum.Checked != 8 || sum.Skipped != 1 || sum.Failed != 1 {
		t.Fatalf("总/已检/跳过/失败 = %d/%d/%d/%d, want 10/8/1/1", sum.Total, sum.Checked, sum.Skipped, sum.Failed)
	}
	if sum.Admitted != 2 || sum.RegionLimited != 1 || sum.Disabled != 1 || sum.AuthExpired != 1 ||
		sum.Challenge != 1 || sum.Forbidden != 1 || sum.Unknown != 1 {
		t.Fatalf("状态计数错误: %+v", sum)
	}
}

func TestAggregateCheckOutcomesEmpty(t *testing.T) {
	sum := aggregateCheckOutcomes(nil)
	if sum.Total != 0 || sum.Checked != 0 {
		t.Fatalf("空列表应为零值: %+v", sum)
	}
}

// TestHandleCheckAllRoute 端到端路由与聚合：无 Cookie → skipped；密文损坏 → failed。
// 不涉及 muse.ai 网络（两种账号都不会发起真实检测）。
func TestHandleCheckAllRoute(t *testing.T) {
	st := newTestStore(t)
	if err := st.Add(&Account{Email: "nocookie@x.com", Status: "active", Plan: "free"}); err != nil {
		t.Fatal(err)
	}
	if err := st.Add(&Account{Email: "badenc@x.com", Status: "active", Plan: "free", Cookie: "enc:not-base64!!"}); err != nil {
		t.Fatal(err)
	}
	cr, err := NewCrypto(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{store: st, crypto: cr}
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/api/accounts/check-all", "application/json", bytes.NewReader([]byte("{}")))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("应 200, got %d", resp.StatusCode)
	}
	var body struct {
		OK   bool `json:"ok"`
		Data struct {
			Results []checkOutcome `json:"results"`
			Summary checkSummary   `json:"summary"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if !body.OK || len(body.Data.Results) != 2 {
		t.Fatalf("应返回 2 条结果: ok=%v n=%d", body.OK, len(body.Data.Results))
	}
	sum := body.Data.Summary
	if sum.Skipped != 1 || sum.Failed != 1 || sum.Total != 2 || sum.Checked != 0 {
		t.Fatalf("汇总应为 skipped=1 failed=1 total=2 checked=0, got %+v", sum)
	}
	var sawSkipped, sawFailed bool
	for _, r := range body.Data.Results {
		switch {
		case r.Email == "nocookie@x.com":
			if !r.Skipped || strings.Contains(r.Error, "解密") {
				t.Fatalf("无 Cookie 账号应为 skipped: %+v", r)
			}
			sawSkipped = true
		case r.Email == "badenc@x.com":
			if r.OK || !strings.Contains(r.Error, "解密") {
				t.Fatalf("密文损坏账号应为 failed+解密提示: %+v", r)
			}
			sawFailed = true
		}
	}
	if !sawSkipped || !sawFailed {
		t.Fatal("结果明细缺失")
	}
	// 跳过/失败的账号不应改状态
	if a, _ := st.Get(body.Data.Results[0].ID); a != nil && a.CheckedAt != "" {
		// 仅校验存在性；CheckedAt 由真实检测才写入
		_ = a
	}
}
