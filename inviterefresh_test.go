package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAggregateInviteRefresh(t *testing.T) {
	outs := []inviteRefreshOutcome{
		{Email: "a@x.com", OK: true, Code: "AAA111"},
		{Email: "b@x.com", OK: true, Code: "BBB222", NewCode: true},
		{Email: "c@x.com", InviteState: "EXHAUSTED", Error: "邀请额度已用尽（invite_state=EXHAUSTED），无可刮开的邀请码"},
		{Email: "d@x.com", OK: false, Error: "触发真人验证（checkpoint）"},
		{Email: "e@x.com", Skipped: true, Error: "未设置登录 Cookie，已跳过"},
	}
	sum := aggregateInviteRefresh(outs)
	if sum.Total != 5 || sum.Refreshed != 2 || sum.Acquired != 1 || sum.Exhausted != 1 || sum.Failed != 1 || sum.Skipped != 1 {
		t.Fatalf("汇总计数错误: %+v", sum)
	}
}

func TestAggregateInviteRefreshEmpty(t *testing.T) {
	if sum := aggregateInviteRefresh(nil); sum.Total != 0 {
		t.Fatalf("空列表应为零值: %+v", sum)
	}
}

// TestFetchInviteAllRoute 端到端：无 Cookie → skipped；密文损坏 → failed；不发起网络请求。
func TestFetchInviteAllRoute(t *testing.T) {
	st := newTestStore(t)
	if err := st.Add(&Account{Email: "nocookie@x.com", Status: "active", Plan: "free"}); err != nil {
		t.Fatal(err)
	}
	if err := st.Add(&Account{Email: "badenc@x.com", Status: "active", Plan: "free", Cookie: "enc:!!!"}); err != nil {
		t.Fatal(err)
	}
	cr, _ := NewCrypto(make([]byte, 32))
	srv := &Server{store: st, crypto: cr}
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/api/accounts/fetch-invite-all", "application/json", bytes.NewReader([]byte("{}")))
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
			Results []inviteRefreshOutcome `json:"results"`
			Summary inviteRefreshSummary   `json:"summary"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if !body.OK || len(body.Data.Results) != 2 {
		t.Fatalf("应返回 2 条结果: ok=%v n=%d", body.OK, len(body.Data.Results))
	}
	sum := body.Data.Summary
	if sum.Total != 2 || sum.Skipped != 1 || sum.Failed != 1 || sum.Refreshed != 0 {
		t.Fatalf("汇总应为 total=2 skipped=1 failed=1, got %+v", sum)
	}
}