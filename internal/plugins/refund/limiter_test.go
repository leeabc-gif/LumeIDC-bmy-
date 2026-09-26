package refund

import (
	"testing"
	"time"
)

func TestAdminRateLimiter_AllowsWithinLimit(t *testing.T) {
	l := newAdminRateLimiter()
	for i := 0; i < 5; i++ {
		ok, _ := l.allow("admin1|1.2.3.4|approve", 5)
		if !ok {
			t.Fatalf("第 %d 次应放行", i+1)
		}
	}
}

func TestAdminRateLimiter_RejectsBeyondLimit(t *testing.T) {
	l := newAdminRateLimiter()
	for i := 0; i < 3; i++ {
		if ok, _ := l.allow("k|approve", 3); !ok {
			t.Fatalf("前 3 次应放行，第 %d 次被拒", i+1)
		}
	}
	ok, retry := l.allow("k|approve", 3)
	if ok {
		t.Fatal("第 4 次应被限流")
	}
	if retry <= 0 || retry > 60*time.Second {
		t.Fatalf("retry-after 应该在 (0, window] 内，got %s", retry)
	}
}

func TestAdminRateLimiter_KeyIsolation(t *testing.T) {
	l := newAdminRateLimiter()
	for i := 0; i < 3; i++ {
		if ok, _ := l.allow("k1|approve", 3); !ok {
			t.Fatalf("k1 前 3 次应放行")
		}
	}
	// k1 已满，k2 不应受影响。
	for i := 0; i < 3; i++ {
		if ok, _ := l.allow("k2|approve", 3); !ok {
			t.Fatalf("k2 不应被 k1 限流，第 %d 次被拒", i+1)
		}
	}
}

func TestAdminRateLimiter_WindowSlides(t *testing.T) {
	l := newAdminRateLimiter()
	l.window = 50 * time.Millisecond // 测试用短窗
	for i := 0; i < 3; i++ {
		if ok, _ := l.allow("k|approve", 3); !ok {
			t.Fatalf("前 3 次应放行")
		}
	}
	if ok, _ := l.allow("k|approve", 3); ok {
		t.Fatal("第 4 次立即应被限流")
	}
	time.Sleep(60 * time.Millisecond)
	if ok, _ := l.allow("k|approve", 3); !ok {
		t.Fatal("窗口滑过后应再次放行")
	}
}

func TestAdminRateLimiter_ZeroMaxMeansNoLimit(t *testing.T) {
	l := newAdminRateLimiter()
	for i := 0; i < 100; i++ {
		if ok, _ := l.allow("k|approve", 0); !ok {
			t.Fatalf("max<=0 应无限流，第 %d 次被拒", i+1)
		}
	}
}
