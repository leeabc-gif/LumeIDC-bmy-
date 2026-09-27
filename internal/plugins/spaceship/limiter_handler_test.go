package spaceship

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// checkAdminRate 默认 30 次/分钟；同 admin 同 op 同 IP 第 31 次被拒，
// 不同 op / admin / IP 各有独立配额（与 refund 同款语义，底层 plugin.RateLimiter）。
func TestCheckAdminRate_Default30(t *testing.T) {
	p := testPlugin(nil)
	mk := func(path, addr string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, path, nil)
		r.RemoteAddr = addr
		return r
	}
	for i := 0; i < 30; i++ {
		if ok, _ := p.checkAdminRate(mk("/operations/1/retry", "10.0.0.1:1111"), 7, "retry"); !ok {
			t.Fatalf("默认 30/min：第 %d 次应放行", i+1)
		}
	}
	ok, retry := p.checkAdminRate(mk("/operations/1/retry", "10.0.0.1:1111"), 7, "retry")
	if ok {
		t.Fatal("默认 30/min：第 31 次应被限流")
	}
	if retry <= 0 {
		t.Fatalf("retry-after 应为正，got %s", retry)
	}
	// 不同 op（delete）不占用 retry 的配额
	if ok, _ := p.checkAdminRate(mk("/domains/1/delete", "10.0.0.1:1111"), 7, "delete"); !ok {
		t.Fatal("不同 op 不应共用配额")
	}
	// 不同 admin 不互相干扰
	if ok, _ := p.checkAdminRate(mk("/operations/1/retry", "10.0.0.1:1111"), 8, "retry"); !ok {
		t.Fatal("不同 adminID 不应共用配额")
	}
	// 不同 IP 不互相干扰
	if ok, _ := p.checkAdminRate(mk("/operations/1/retry", "10.0.0.2:1111"), 7, "retry"); !ok {
		t.Fatal("不同 IP 不应共用配额")
	}
}

// 配置 adminOpRatePerMin=0 表示关闭限流（开发/小机房场景）。
func TestCheckAdminRate_DisabledByConfig(t *testing.T) {
	p := testPlugin(map[string]string{
		"plugin.spaceship.adminOpRatePerMin": "0",
	})
	for i := 0; i < 100; i++ {
		r := httptest.NewRequest(http.MethodPost, "/operations/1/retry", nil)
		r.RemoteAddr = "10.0.0.1:1111"
		if ok, _ := p.checkAdminRate(r, 7, "retry"); !ok {
			t.Fatalf("max=0 应无限流，第 %d 次被拒", i+1)
		}
	}
}

// 真 handler 端到端：retry 打满限额后第 31 次返回 429（StatusFail），
// 防止管理员连点把上游异步操作查询打爆。
func TestP2_AdminRetryHandler_RateLimited429(t *testing.T) {
	up := newFakeUpstream()
	defer up.Close()
	withFakeUpstream(t, up)
	p, d := newTestPlugin(t, up, nil)

	var opID int64
	if err := d.QueryRowContext(context.Background(),
		`INSERT INTO plugin_spaceship_operations (operation_id,domain,op_type,status,started_at)
		 VALUES('op-rl-1','rl-p2.com','domain_create','pending',$1) RETURNING id`, time.Now()).Scan(&opID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = d.ExecContext(context.Background(), `DELETE FROM plugin_spaceship_operations WHERE id=$1`, opID)
	})

	retryReq := func() *http.Request {
		r := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/operations/%d/retry", opID), strings.NewReader(`{}`))
		r.SetPathValue("id", fmt.Sprint(opID))
		r.RemoteAddr = "10.0.0.1:1111"
		return withAdmin(r)
	}

	// 前 30 次（默认限额）全部正常放行并触发轮询
	for i := 0; i < 30; i++ {
		code, res := call(p.adminRetryOperation, retryReq())
		if code != http.StatusOK || res["ok"] != float64(1) {
			t.Fatalf("第 %d 次应正常放行: code=%d body=%v", i+1, code, res)
		}
	}
	// 第 31 次被限流：429 + ok=0
	code, res := call(p.adminRetryOperation, retryReq())
	if code != http.StatusTooManyRequests {
		t.Fatalf("第 31 次应返回 429，got %d body=%v", code, res)
	}
	if res["ok"] == float64(1) {
		t.Fatalf("被限流不得返回 ok=1: %v", res)
	}
}

// 删除域名同受限额保护（op=delete 与 retry 独立计数）。
func TestP2_AdminDeleteDomainHandler_RateLimited429(t *testing.T) {
	up := newFakeUpstream()
	defer up.Close()
	p, _ := newTestPlugin(t, up, nil)
	// adminDeleteDomain 只做本地软删，id 无需真实存在即可验证限流闸门。
	delReq := func() *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/domains/999999/delete", nil)
		r.SetPathValue("id", "999999")
		r.RemoteAddr = "10.0.0.3:1111"
		return withAdmin(r)
	}
	for i := 0; i < 30; i++ {
		if code, _ := call(p.adminDeleteDomain, delReq()); code != http.StatusOK {
			t.Fatalf("第 %d 次应正常放行", i+1)
		}
	}
	if code, res := call(p.adminDeleteDomain, delReq()); code != http.StatusTooManyRequests {
		t.Fatalf("第 31 次应返回 429，got %d body=%v", code, res)
	}
}
