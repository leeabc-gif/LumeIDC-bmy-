package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestValidAdminPath(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{in: "/manage", want: true},
		{in: "/_panel2", want: true},
		{in: "", want: false},
		{in: "/", want: false},
		{in: "/admin", want: false},
		{in: "/admins", want: false}, // 以 /admin 开头
		{in: "/administrator", want: false},
		{in: "/a", want: false},        // 过短
		{in: "/a b", want: false},      // 含空格
		{in: "/a/b", want: false},      // 多段
		{in: "/login", want: false},    // 保留路由
		{in: "/services", want: false}, // 保留路由
		{in: "/plugin", want: false},   // 插件路由前缀
		{in: "/plugins", want: false},  // 插件公开端点前缀
		{in: "/manage/", want: false},  // 尾斜杠
	}
	for _, tc := range cases {
		if got := ValidAdminPath(tc.in); got != tc.want {
			t.Errorf("ValidAdminPath(%q)=%v，期望 %v", tc.in, got, tc.want)
		}
	}
}

func TestAdminPathRewrite(t *testing.T) {
	// 模拟后台处理器：返回一个含 /admin 链接的 HTML 页面。
	admin := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/admin/redirect" {
			http.Redirect(w, r, "/admin/site?ok=1", http.StatusFound)
			return
		}
		w.Write([]byte(`<a href="/admin">后台</a> <a href="/admin/users">用户</a>`))
	})
	h := AdminPath(admin, NewAdminPathConfig("/manage"))

	// 1. custom 路径 → 改写为内部 /admin，body 链接改写为 custom
	req := httptest.NewRequest(http.MethodGet, "/manage/users", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if !strings.Contains(w.Body.String(), "/manage/users") {
		t.Fatalf("body 未改写为 custom: %s", w.Body.String())
	}
	if strings.Contains(w.Body.String(), "/admin/") {
		t.Fatalf("body 仍含 /admin/: %s", w.Body.String())
	}

	// 1b. 精确 custom 路径 → 内部 /admin（仪表盘）
	req = httptest.NewRequest(http.MethodGet, "/manage", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if !strings.Contains(w.Body.String(), "/manage") || strings.Contains(w.Body.String(), "/admin/") {
		t.Fatalf("精确路径改写失败: %s", w.Body.String())
	}

	// 2. /admin 直连被屏蔽
	req = httptest.NewRequest(http.MethodGet, "/admin/users", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("/admin 直连应 404，实际 %d", w.Code)
	}

	// 3. Location 头改写
	req = httptest.NewRequest(http.MethodGet, "/manage/redirect", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusFound || w.Header().Get("Location") != "/manage/site?ok=1" {
		t.Fatalf("Location 改写失败: code=%d loc=%q", w.Code, w.Header().Get("Location"))
	}

	// 4. 公共路径原样透传（不被包装）
	req = httptest.NewRequest(http.MethodGet, "/services/5/console", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if !strings.Contains(w.Body.String(), "/admin/users") {
		t.Fatalf("公共路径 body 应原样包含 /admin/users: %s", w.Body.String())
	}
}

func TestAdminPathBinaryPassthrough(t *testing.T) {
	// 二进制响应（如证件照片）不应被缓冲改写
	img := []byte{0xff, 0xd8, 0xff, 0xe0, 0x00, 0x10, 0x4a, 0x46, 0x49, 0x46}
	admin := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		w.Write(img)
	})
	h := AdminPath(admin, NewAdminPathConfig("/manage"))
	req := httptest.NewRequest(http.MethodGet, "/manage/users/1/photo/front", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if !strings.EqualFold(w.Header().Get("Content-Type"), "image/jpeg") {
		t.Fatalf("Content-Type 应透传: %s", w.Header().Get("Content-Type"))
	}
	if w.Body.String() != string(img) {
		t.Fatalf("二进制 body 被改写: %x", w.Body.Bytes())
	}
}

func TestAdminPathInvalidCustomIsPassthrough(t *testing.T) {
	// 非法 custom 时中间件不启用（透传，/admin 不屏蔽）
	admin := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	})
	h := AdminPath(admin, NewAdminPathConfig("/login"))
	req := httptest.NewRequest(http.MethodGet, "/admin/users", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK || w.Body.String() != "ok" {
		t.Fatalf("非法 custom 应透传: code=%d body=%q", w.Code, w.Body.String())
	}
}

// 保存改路径后，重定向 Location 应指向新路径（flush 时读取当前 cfg）。
func TestAdminPathRedirectUsesNewPath(t *testing.T) {
	cfg := NewAdminPathConfig("/lumeidc1")
	admin := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cfg.Set("/lumeidc") // 模拟站点设置保存 admin_path=/lumeidc
		http.Redirect(w, r, "/admin/site?ok=1", http.StatusFound)
	})
	h := AdminPath(admin, cfg)
	req := httptest.NewRequest(http.MethodGet, "/lumeidc1/site", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if got := w.Header().Get("Location"); got != "/lumeidc/site?ok=1" {
		t.Fatalf("Location=%q，期望 /lumeidc/site?ok=1", got)
	}
}

func TestAdminPathDynamicUpdate(t *testing.T) {
	// 运行期 Set 立即生效，无需重启
	admin := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("admin"))
	})
	cfg := NewAdminPathConfig("")
	h := AdminPath(admin, cfg)

	req := httptest.NewRequest(http.MethodGet, "/admin/users", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("未配置时应放行 /admin，实际 %d", w.Code)
	}

	cfg.Set("/manage")
	req = httptest.NewRequest(http.MethodGet, "/admin/users", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("配置后 /admin 应 404，实际 %d", w.Code)
	}
	req = httptest.NewRequest(http.MethodGet, "/manage", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("/manage 应可达，实际 %d", w.Code)
	}

	cfg.Set("")
	req = httptest.NewRequest(http.MethodGet, "/admin/users", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("清空后 /admin 应恢复，实际 %d", w.Code)
	}
}

// TestAdminPathSkipsStaticAndMarkedResponses 后台路径改写不得破坏静态资源与
// SPA 外壳：Vite 产物名可能含 "admin" 字面量（/app/admin-<hash>.js），
// 一旦被改写成自定义路径，资源地址即 404。
func TestAdminPathSkipsStaticAndMarkedResponses(t *testing.T) {
	cfg := NewAdminPathConfig("/lumeidc")
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/app/"):
			w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
			w.Write([]byte(`var u="/app/admin-Bftth_t-.js"`))
		case strings.HasPrefix(r.URL.Path, "/admin/panel"):
			// 模拟 SPA 外壳：带免改写标记，内容含 /app/admin-*.js
			w.Header().Set(NoRewriteHeader, "1")
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Write([]byte(`<script src="/app/admin-Bftth_t-.js"></script>`))
		default:
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Write([]byte(`<a href="/admin/orders">订单</a>`))
		}
	})
	h := AdminPath(next, cfg)

	// 1) 静态资源直通：/app/admin-*.js 不被改写
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/app/admin-Bftth_t-.js", nil))
	if !strings.Contains(w.Body.String(), "/app/admin-Bftth_t-.js") {
		t.Fatalf("静态资源被改写: %q", w.Body.String())
	}

	// 2) 带免改写标记的 SPA 外壳直通
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/lumeidc/panel", nil))
	if !strings.Contains(w.Body.String(), "/app/admin-Bftth_t-.js") {
		t.Fatalf("SPA 外壳被改写: %q", w.Body.String())
	}

	// 3) 普通后台 HTML 仍应改写（保持既有能力）
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/lumeidc/orders", nil))
	if !strings.Contains(w.Body.String(), "/lumeidc/orders") {
		t.Fatalf("普通后台响应未被改写: %q", w.Body.String())
	}
}
