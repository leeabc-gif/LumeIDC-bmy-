package middleware

import (
	"bufio"
	"bytes"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
)

// 保留的公共顶层路由段：自定义后台路径不得与之冲突，否则会劫持公共页面。
// ponytail: 需与 pages/auth/pay/install/assets 的 Register 及各 mux.HandleFunc 同步维护。
var reservedTopPaths = []string{
	"login", "register", "logout", "verify", "captcha", "auth",
	"products", "cart", "services", "buy", "user", "notifications",
	"order", "pay", "mock", "assets", "healthz", "readyz", "install",
	// 插件框架：/plugin/{name}/*（插件路由）、/plugins/client、/plugins/injections
	"plugin", "plugins",
}

var adminPathRE = regexp.MustCompile(`^/[A-Za-z0-9_-]{2,32}$`)

// ValidAdminPath 校验自定义后台路径：单段字母数字_-，不以 /admin 开头
// （否则响应改写会自噬），且不与公共顶层路由冲突。
func ValidAdminPath(custom string) bool {
	custom = strings.TrimSpace(custom)
	if !adminPathRE.MatchString(custom) || strings.HasPrefix(custom, "/admin") {
		return false
	}
	seg := strings.TrimPrefix(custom, "/")
	for _, r := range reservedTopPaths {
		if seg == r {
			return false
		}
	}
	return true
}

// rewriteAdminPath 把字符串中的 "/admin" 路径改写为 custom。
// 先替换 "/admin/" 再替换独立的 "/admin"，避免 "/adminxxx" 被误伤（custom 已校验不含 /admin 前缀）。
func rewriteAdminPath(s, custom string) string {
	if !strings.Contains(s, "/admin") {
		return s
	}
	s = strings.ReplaceAll(s, "/admin/", custom+"/")
	return strings.ReplaceAll(s, "/admin", custom)
}

// NoRewriteHeader 标记该响应无需做后台路径改写（SPA 外壳由前端按 /session 的
// admin.path 在运行时决定路径；其 HTML 中的资源名可能含 "admin" 字面量）。
const NoRewriteHeader = "X-Lume-No-Rewrite"

// DevProxyHeader 标记请求来自本地开发代理（vite.config.ts 的 /__api proxy 注入）。
// 开发期后台 SPA 与 Go 可能各用不同端口/路径，/session 需据此下发真实后台路径；
// 生产没有该头，匿名 /session 一律不下发自定义路径，避免泄漏后台入口。
const DevProxyHeader = "X-Lume-Dev-Proxy"

// IsDevProxy 判断请求是否来自本地开发代理。
func IsDevProxy(r *http.Request) bool {
	return r.Header.Get(DevProxyHeader) == "1"
}

func isBinaryContentType(ct string) bool {
	ct = strings.ToLower(strings.TrimSpace(ct))
	return strings.HasPrefix(ct, "image/") || strings.HasPrefix(ct, "video/") ||
		strings.HasPrefix(ct, "audio/") || strings.HasPrefix(ct, "font/") ||
		strings.HasPrefix(ct, "application/octet-stream")
}

// noRewriteContentType 无需改写的文本类型：JSON/事件流等结构化响应里的
// "/admin" 子串可能是数据而非路径引用（SPA/JSON API 依赖此行为），必须原样直通。
func noRewriteContentType(ct string) bool {
	ct = strings.ToLower(strings.TrimSpace(ct))
	if ct == "" {
		return false
	}
	if strings.HasPrefix(ct, "application/json") || strings.Contains(ct, "+json") {
		return true
	}
	if strings.HasPrefix(ct, "text/event-stream") || strings.HasPrefix(ct, "application/x-ndjson") {
		return true
	}
	return false
}

// AdminPathConfig 动态持有当前后台路径；站点设置保存时更新，无需重启。
type AdminPathConfig struct {
	mu     sync.RWMutex
	custom string
}

func NewAdminPathConfig(custom string) *AdminPathConfig {
	return &AdminPathConfig{custom: strings.TrimSpace(custom)}
}

// Set 运行期更新后台路径（空值=恢复默认 /admin）。
func (c *AdminPathConfig) Set(custom string) {
	c.mu.Lock()
	c.custom = strings.TrimSpace(custom)
	c.mu.Unlock()
}

// Get 当前后台路径（空=未启用自定义，用默认 /admin）。
func (c *AdminPathConfig) Get() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.custom
}

// AdminPath 自定义后台路径：把 custom/* 映射到内部 /admin/*，屏蔽默认 /admin 直连，
// 并把后台响应中的 /admin 引用改写为 custom（导航/表单/跳转跟随新路径）。
// 路径值来自 cfg，运行期可更新（立即生效）。仅对 custom 请求启用响应缓冲改写，
// 公共路径原样透传（避免影响 VNC/资源代理）。
func AdminPath(next http.Handler, cfg *AdminPathConfig) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		custom := cfg.Get()
		if custom == "" || !ValidAdminPath(custom) {
			next.ServeHTTP(w, r)
			return
		}
		p := r.URL.Path
		if p == "/admin" || strings.HasPrefix(p, "/admin/") {
			http.NotFound(w, r) // 屏蔽默认入口，不泄漏 custom
			return
		}
		// 静态资源不做改写：其文件名可能含 "admin" 字面量
		// （如 Vite 产物 /app/admin-<hash>.js），改写会破坏资源地址导致 404。
		if strings.HasPrefix(p, "/app/") || strings.HasPrefix(p, "/assets/") {
			next.ServeHTTP(w, r)
			return
		}
		if p == custom || p == custom+"/" {
			// 精确/尾斜杠 → 仪表盘（注册为精确 "GET /admin"，故不带尾斜杠）
			r2 := r.Clone(r.Context())
			r2.URL.RawPath = ""
			r2.URL.Path = "/admin"
			rw := &adminRewriteWriter{ResponseWriter: w, custom: cfg.Get, code: http.StatusOK}
			next.ServeHTTP(rw, r2)
			rw.flush()
			return
		}
		if strings.HasPrefix(p, custom+"/") {
			r2 := r.Clone(r.Context())
			r2.URL.RawPath = ""
			r2.URL.Path = "/admin" + strings.TrimPrefix(p, custom)
			rw := &adminRewriteWriter{ResponseWriter: w, custom: cfg.Get, code: http.StatusOK}
			next.ServeHTTP(rw, r2)
			rw.flush()
			return
		}
		next.ServeHTTP(w, r)
	})
}

// adminRewriteWriter 缓冲后台响应并改写 /admin → custom（Location 头与 text 响应体）。
// 文本响应缓冲到 flush 统一改写下发（并交由 net/http 重算 Content-Length）；二进制直通。
// custom 为取值函数：flush 时读取当前路径，保证"保存改路径"的跳转指向新路径。
type adminRewriteWriter struct {
	http.ResponseWriter
	custom      func() string
	code        int
	decided     bool
	passthrough bool
	forwarded   bool // 底层 WriteHeader 是否已下发
	buf         bytes.Buffer
}

func (rw *adminRewriteWriter) WriteHeader(code int) {
	rw.code = code
	if rw.decided {
		if rw.passthrough && !rw.forwarded {
			rw.ResponseWriter.WriteHeader(code)
			rw.forwarded = true
		}
		return
	}
	if isBinaryContentType(rw.Header().Get("Content-Type")) || noRewriteContentType(rw.Header().Get("Content-Type")) ||
		rw.Header().Get(NoRewriteHeader) != "" {
		rw.decided = true
		rw.passthrough = true
		rw.ResponseWriter.WriteHeader(code)
		rw.forwarded = true
		return
	}
	rw.decided = true // 文本：缓冲，延迟到 flush 下发
}

func (rw *adminRewriteWriter) Write(p []byte) (int, error) {
	if !rw.decided {
		ct := rw.Header().Get("Content-Type")
		if rw.Header().Get(NoRewriteHeader) != "" ||
			isBinaryContentType(ct) || noRewriteContentType(ct) ||
			isBinaryContentType(http.DetectContentType(p)) || noRewriteContentType(http.DetectContentType(p)) {
			rw.passthrough = true
		}
		rw.decided = true
		rw.code = http.StatusOK
	}
	if rw.passthrough {
		if !rw.forwarded {
			rw.ResponseWriter.WriteHeader(rw.code)
			rw.forwarded = true
		}
		return rw.ResponseWriter.Write(p)
	}
	rw.buf.Write(p)
	return len(p), nil
}

// flush 在 next.ServeHTTP 返回后把缓冲响应改写并下发；透传模式无需处理。
func (rw *adminRewriteWriter) flush() {
	if rw.passthrough {
		return
	}
	custom := rw.custom()
	if custom != "" {
		if loc := rw.Header().Get("Location"); loc != "" {
			rw.Header().Set("Location", rewriteAdminPath(loc, custom))
		}
	}
	rw.Header().Del("Content-Length") // 改写后长度变化，交由 net/http 重新计算
	if !rw.forwarded {
		rw.ResponseWriter.WriteHeader(rw.code) // 先下发 header，避免 body 先写导致隐式 200
		rw.forwarded = true
	}
	if rw.buf.Len() > 0 {
		body := rw.buf.Bytes()
		if custom != "" {
			body = []byte(rewriteAdminPath(string(body), custom))
		}
		rw.ResponseWriter.Write(body)
	}
}

func (rw *adminRewriteWriter) Flush() {
	if f, ok := rw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (rw *adminRewriteWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return rw.ResponseWriter.(http.Hijacker).Hijack()
}
