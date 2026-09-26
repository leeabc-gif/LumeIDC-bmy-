package refund

import (
	"net/http/httptest"
	"testing"
)

// P1：admin 入口限流。能拒绝高频审核防脚本批量点击 / 误操作刷量。
// 验证：同一 admin 同 IP 在 60s 内连续请求 >=30 次后被拒；不同 IP 不互相干扰。
func TestCheckAdminRate_BlocksAfterBurst(t *testing.T) {
	p := testPlugin(map[string]string{
		"plugin.refund.adminApproveRatePerMin": "30",
	})

	// 30 次同 admin/同 IP 全部放行
	for i := 0; i < 30; i++ {
		r := httptest.NewRequest("POST", "/x/approve", nil)
		r.RemoteAddr = "10.0.0.1:1111"
		ok, _ := p.checkAdminRate(r, 42, "approve")
		if !ok {
			t.Fatalf("第 %d 次应放行", i+1)
		}
	}
	// 第 31 次同 admin/同 IP 拒
	r := httptest.NewRequest("POST", "/x/approve", nil)
	r.RemoteAddr = "10.0.0.1:1111"
	ok, retry := p.checkAdminRate(r, 42, "approve")
	if ok {
		t.Fatal("第 31 次应被限流")
	}
	if retry <= 0 {
		t.Fatalf("retry-after 应为正，got %s", retry)
	}

	// 不同 IP（同样 30 次）应能继续放行 —— "防刷"按 IP+admin 双重键隔离。
	for i := 0; i < 30; i++ {
		r2 := httptest.NewRequest("POST", "/x/approve", nil)
		r2.RemoteAddr = "10.0.0.2:1111"
		if ok, _ := p.checkAdminRate(r2, 42, "approve"); !ok {
			t.Fatalf("不同 IP 不应被前一个 IP 限流，第 %d 次被拒", i+1)
		}
	}

	// 不同 admin（同 IP）同样不被限流 —— 限流键含 adminID，防止一个 admin
	// 把另一个 admin 的名额用光。
	for i := 0; i < 30; i++ {
		r3 := httptest.NewRequest("POST", "/x/approve", nil)
		r3.RemoteAddr = "10.0.0.1:1111"
		if ok, _ := p.checkAdminRate(r3, 99, "approve"); !ok {
			t.Fatalf("不同 admin ID 不应被前一个限流，第 %d 次被拒", i+1)
		}
	}

	// approve / reject 分别计入（key 第三段 = 操作名）—— 防止"在 approve 上把
	// 额度用光后连 reject 都点不动"。
	for i := 0; i < 30; i++ {
		r4 := httptest.NewRequest("POST", "/x/reject", nil)
		r4.RemoteAddr = "10.0.0.1:1111"
		if ok, _ := p.checkAdminRate(r4, 42, "reject"); !ok {
			t.Fatalf("reject 不应与 approve 共用限流配额，第 %d 次被拒", i+1)
		}
	}
}

// 配置 adminApproveRatePerMin=0 表示关闭限流（开发/小机房场景）。
func TestCheckAdminRate_DisabledByConfig(t *testing.T) {
	p := testPlugin(map[string]string{
		"plugin.refund.adminApproveRatePerMin": "0",
	})
	for i := 0; i < 100; i++ {
		r := httptest.NewRequest("POST", "/x/approve", nil)
		r.RemoteAddr = "10.0.0.1:1111"
		ok, _ := p.checkAdminRate(r, 42, "approve")
		if !ok {
			t.Fatalf("max=0 应无限流，第 %d 次被拒", i+1)
		}
	}
}

// 默认配置（未设置）走 schema 默认 30。
func TestCheckAdminRate_Default30(t *testing.T) {
	p := testPlugin(map[string]string{})
	for i := 0; i < 30; i++ {
		r := httptest.NewRequest("POST", "/x/approve", nil)
		r.RemoteAddr = "10.0.0.9:2222"
		if ok, _ := p.checkAdminRate(r, 7, "approve"); !ok {
			t.Fatalf("默认 30/min：第 %d 次应放行", i+1)
		}
	}
	r := httptest.NewRequest("POST", "/x/approve", nil)
	r.RemoteAddr = "10.0.0.9:2222"
	if ok, _ := p.checkAdminRate(r, 7, "approve"); ok {
		t.Fatal("默认 30/min：第 31 次应被限流")
	}
}
