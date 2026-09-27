package plugin

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRateLimiter_AllowsWithinLimit(t *testing.T) {
	l := NewRateLimiter()
	for i := 0; i < 5; i++ {
		ok, _ := l.Allow("admin1|1.2.3.4|approve", 5)
		if !ok {
			t.Fatalf("第 %d 次应放行", i+1)
		}
	}
}

func TestRateLimiter_RejectsBeyondLimit(t *testing.T) {
	l := NewRateLimiter()
	for i := 0; i < 3; i++ {
		if ok, _ := l.Allow("k|approve", 3); !ok {
			t.Fatalf("前 3 次应放行，第 %d 次被拒", i+1)
		}
	}
	ok, retry := l.Allow("k|approve", 3)
	if ok {
		t.Fatal("第 4 次应被限流")
	}
	if retry <= 0 || retry > 60*time.Second {
		t.Fatalf("retry-after 应该在 (0, window] 内，got %s", retry)
	}
}

func TestRateLimiter_KeyIsolation(t *testing.T) {
	l := NewRateLimiter()
	for i := 0; i < 3; i++ {
		if ok, _ := l.Allow("k1|approve", 3); !ok {
			t.Fatalf("k1 前 3 次应放行")
		}
	}
	// k1 已满，k2 不应受影响。
	for i := 0; i < 3; i++ {
		if ok, _ := l.Allow("k2|approve", 3); !ok {
			t.Fatalf("k2 不应被 k1 限流，第 %d 次被拒", i+1)
		}
	}
}

func TestRateLimiter_WindowSlides(t *testing.T) {
	l := NewRateLimiter()
	l.window = 50 * time.Millisecond // 测试用短窗
	for i := 0; i < 3; i++ {
		if ok, _ := l.Allow("k|approve", 3); !ok {
			t.Fatalf("前 3 次应放行")
		}
	}
	if ok, _ := l.Allow("k|approve", 3); ok {
		t.Fatal("第 4 次立即应被限流")
	}
	time.Sleep(60 * time.Millisecond)
	if ok, _ := l.Allow("k|approve", 3); !ok {
		t.Fatal("窗口滑过后应再次放行")
	}
}

func TestRateLimiter_ZeroMaxMeansNoLimit(t *testing.T) {
	l := NewRateLimiter()
	for i := 0; i < 100; i++ {
		if ok, _ := l.Allow("k|approve", 0); !ok {
			t.Fatalf("max<=0 应无限流，第 %d 次被拒", i+1)
		}
	}
}

// AllowAdmin：不同 adminID / op 互不占用配额；adminID<=0 退化为 IP-only 键。
func TestRateLimiter_AllowAdminKeyIsolation(t *testing.T) {
	l := NewRateLimiter()
	mk := func(ip string) *http.Request {
		r := httptest.NewRequest("POST", "/x", nil)
		r.RemoteAddr = ip + ":1111"
		return r
	}
	for i := 0; i < 3; i++ {
		if ok, _ := l.AllowAdmin(mk("10.0.0.1"), 3, 42, "approve"); !ok {
			t.Fatalf("admin42 approve 第 %d 次应放行", i+1)
		}
	}
	if ok, _ := l.AllowAdmin(mk("10.0.0.1"), 3, 42, "approve"); ok {
		t.Fatal("同 admin 同 op 同 IP 第 4 次应被限流")
	}
	// 不同 op / 不同 admin / 不同 IP 均有独立配额
	if ok, _ := l.AllowAdmin(mk("10.0.0.1"), 3, 42, "reject"); !ok {
		t.Fatal("不同 op 不应共用配额")
	}
	if ok, _ := l.AllowAdmin(mk("10.0.0.1"), 3, 99, "approve"); !ok {
		t.Fatal("不同 adminID 不应共用配额")
	}
	if ok, _ := l.AllowAdmin(mk("10.0.0.2"), 3, 42, "approve"); !ok {
		t.Fatal("不同 IP 不应共用配额")
	}
	// adminID<=0 走 IP-only 键，且 maxPerMin<=0 不限流
	if ok, _ := l.AllowAdmin(mk("10.0.0.3"), 3, 0, "approve"); !ok {
		t.Fatal("adminID=0 应放行（IP-only 键）")
	}
	for i := 0; i < 10; i++ {
		if ok, _ := l.AllowAdmin(mk("10.0.0.3"), 0, 0, "approve"); !ok {
			t.Fatalf("maxPerMin<=0 应无限流，第 %d 次被拒", i+1)
		}
	}
}

func TestClientIP(t *testing.T) {
	// X-Forwarded-For 取首段（代理链最左为真实客户端）
	r := httptest.NewRequest("GET", "/x", nil)
	r.Header.Set("X-Forwarded-For", "1.2.3.4, 10.0.0.1, 10.0.0.2")
	if got := ClientIP(r); got != "1.2.3.4" {
		t.Fatalf("X-Forwarded-For 应取首段，got %q", got)
	}
	// 无 XFF 时回退 X-Real-IP
	r = httptest.NewRequest("GET", "/x", nil)
	r.Header.Set("X-Real-IP", "5.6.7.8")
	if got := ClientIP(r); got != "5.6.7.8" {
		t.Fatalf("应回退 X-Real-IP，got %q", got)
	}
	// 两者都无取 RemoteAddr 并去端口
	r = httptest.NewRequest("GET", "/x", nil)
	r.RemoteAddr = "9.9.9.9:54321"
	if got := ClientIP(r); got != "9.9.9.9" {
		t.Fatalf("应取 RemoteAddr 去端口，got %q", got)
	}
	// XFF 优先于 X-Real-IP
	r = httptest.NewRequest("GET", "/x", nil)
	r.Header.Set("X-Forwarded-For", "1.1.1.1")
	r.Header.Set("X-Real-IP", "5.6.7.8")
	if got := ClientIP(r); got != "1.1.1.1" {
		t.Fatalf("X-Forwarded-For 应优先，got %q", got)
	}
}
