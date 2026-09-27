package violation

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"lumeidc/internal/plugin"
)

// fakeSettings 内存 settings（配置读取兜底；Host.Config 依赖 pluginName）。
type fakeSettings struct{ vals map[string]string }

func (f *fakeSettings) Get(ctx context.Context, key string) (string, error) { return f.vals[key], nil }
func (f *fakeSettings) Set(ctx context.Context, key, value string) error {
	f.vals[key] = value
	return nil
}

// testPlugin 构造仅带 host（内存 settings）的插件实例：纯限流/配置用例无需真库。
func testPlugin(vals map[string]string) *Plugin {
	p := &Plugin{}
	p.host = (&plugin.Host{Settings: &fakeSettings{vals: vals}}).ForPlugin(Name)
	return p
}

// checkAdminRate 默认 30 次/分钟；同 admin 同 op 同 IP 第 31 次被拒，
// 不同 op / admin / IP 各有独立配额（与 refund/spaceship 同款语义，底层 plugin.RateLimiter）。
func TestCheckAdminRate_Default30(t *testing.T) {
	p := testPlugin(nil)
	mk := func(path, addr string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, path, nil)
		r.RemoteAddr = addr
		return r
	}
	for i := 0; i < 30; i++ {
		if ok, _ := p.checkAdminRate(mk("/save", "10.0.0.1:1111"), 7, "save"); !ok {
			t.Fatalf("默认 30/min：第 %d 次应放行", i+1)
		}
	}
	ok, retry := p.checkAdminRate(mk("/save", "10.0.0.1:1111"), 7, "save")
	if ok {
		t.Fatal("默认 30/min：第 31 次应被限流")
	}
	if retry <= 0 {
		t.Fatalf("retry-after 应为正，got %s", retry)
	}
	// 不同 op（delete）不占用 save 的配额
	if ok, _ := p.checkAdminRate(mk("/1/delete", "10.0.0.1:1111"), 7, "delete"); !ok {
		t.Fatal("不同 op 不应共用配额")
	}
	// 不同 admin 不互相干扰
	if ok, _ := p.checkAdminRate(mk("/save", "10.0.0.1:1111"), 8, "save"); !ok {
		t.Fatal("不同 adminID 不应共用配额")
	}
	// 不同 IP 不互相干扰
	if ok, _ := p.checkAdminRate(mk("/save", "10.0.0.2:1111"), 7, "save"); !ok {
		t.Fatal("不同 IP 不应共用配额")
	}
}

// 配置 adminOpRatePerMin=0 表示关闭限流（开发/小机房场景）。
func TestCheckAdminRate_DisabledByConfig(t *testing.T) {
	p := testPlugin(map[string]string{
		"plugin.violation.adminOpRatePerMin": "0",
	})
	for i := 0; i < 100; i++ {
		r := httptest.NewRequest(http.MethodPost, "/save", nil)
		r.RemoteAddr = "10.0.0.1:1111"
		if ok, _ := p.checkAdminRate(r, 7, "save"); !ok {
			t.Fatalf("max=0 应无限流，第 %d 次被拒", i+1)
		}
	}
}

// 配置为非法值（非数字 / 负数）时回退默认 30，不被绕过限流。
func TestCheckAdminRate_InvalidConfigFallsBackToDefault(t *testing.T) {
	cases := []map[string]string{
		{"plugin.violation.adminOpRatePerMin": "abc"},
		{"plugin.violation.adminOpRatePerMin": "-5"},
	}
	for _, vals := range cases {
		p := testPlugin(vals)
		r := func() *http.Request {
			r := httptest.NewRequest(http.MethodPost, "/save", nil)
			r.RemoteAddr = "10.0.0.9:1111"
			return r
		}
		for i := 0; i < 30; i++ {
			if ok, _ := p.checkAdminRate(r(), 7, "save"); !ok {
				t.Fatalf("配置 %v：第 %d 次应放行", vals, i+1)
			}
		}
		if ok, _ := p.checkAdminRate(r(), 7, "save"); ok {
			t.Fatalf("配置 %v：应回退默认 30 并限流第 31 次", vals)
		}
	}
}

// Retry-After 语义：被拒时返回的等待时长必须落在 (0, 60s] 窗口内。
func TestCheckAdminRate_RetryAfterWithinWindow(t *testing.T) {
	p := testPlugin(nil)
	r := func() *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/1/delete", nil)
		r.RemoteAddr = "10.0.0.5:1111"
		return r
	}
	for i := 0; i < 30; i++ {
		_, _ = p.checkAdminRate(r(), 7, "delete")
	}
	ok, retry := p.checkAdminRate(r(), 7, "delete")
	if ok {
		t.Fatal("第 31 次应被限流")
	}
	if retry <= 0 || retry > 60*time.Second {
		t.Fatalf("retry-after 应落在 (0,60s]，got %s", retry)
	}
}
