package plugin

import (
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// RateLimiter 进程内滑动窗口限流器（默认 60s 窗口），供插件对管理端危险动作
// （审核/驳回/删除/重试等）做防刷与防误操作保护。纯内存、进程重启即清零，
// 这是有意为之：
//
//	（1）单进程内的高频连点/脚本刷才是真实风险，DB 持久化反而拖慢主链路；
//	（2）零外部依赖，插件随框架装上即用。
//
// 多实例部署时限额按进程生效（总量 = 单进程限额 × 实例数）。若确需全局限额，
// 应另行实现基于 Redis/DB 的限流器由插件自行注入，而非改动本类型。
type RateLimiter struct {
	mu      sync.Mutex
	buckets map[string][]time.Time
	window  time.Duration
}

// NewRateLimiter 创建 60s 窗口的滑动窗口限流器（零值不可用，须经本函数创建）。
func NewRateLimiter() *RateLimiter {
	return &RateLimiter{
		buckets: map[string][]time.Time{},
		window:  60 * time.Second,
	}
}

// Allow 记录一次访问并判断是否在限额内。返回 (allowed, retryAfter)；
// 被拒时 retryAfter 表示窗口腾出空位还需等待多久（配合 429 + Retry-After 头）。
// max <= 0 表示该 key 不限流。
func (l *RateLimiter) Allow(key string, max int) (bool, time.Duration) {
	if max <= 0 {
		return true, 0
	}
	now := time.Now()
	cutoff := now.Add(-l.window)
	l.mu.Lock()
	defer l.mu.Unlock()
	ts := l.buckets[key]
	// 丢弃窗口外的旧记录（时间升序，从前向后找首个在窗内的）
	i := 0
	for ; i < len(ts); i++ {
		if ts[i].After(cutoff) {
			break
		}
	}
	ts = ts[i:]
	if len(ts) >= max {
		// 最早一条滑出窗口的时刻 = 还需等待的时间
		return false, ts[0].Add(l.window).Sub(now)
	}
	ts = append(ts, now)
	l.buckets[key] = ts
	return true, 0
}

// AllowAdmin 管理端动作的限流判定：键 = "<adminID>|<ip>|<op>"，不同管理员、
// 来源 IP、操作之间互不占用配额。op 建议用 "approve"、"delete" 等动词，
// 与操作审计日志同名，便于事后定位是谁在刷。adminID <= 0（理论上不该发生，
// 保持健壮）退化为 IP-only 键。maxPerMin <= 0 表示不限流（本地开发/单管理员机房）。
func (l *RateLimiter) AllowAdmin(r *http.Request, maxPerMin int, adminID int64, op string) (bool, time.Duration) {
	if maxPerMin <= 0 {
		return true, 0
	}
	ip := ClientIP(r)
	key := ip
	if adminID > 0 {
		key = strconv.FormatInt(adminID, 10) + "|" + ip
	}
	return l.Allow(key+"|"+op, maxPerMin)
}

// ClientIP 取请求来源 IP：优先 X-Forwarded-For 首段（反向代理场景），
// 回退 X-Real-IP，最后 RemoteAddr（去端口）。
// 注意：直连暴露时 X-Forwarded-For 可被客户端伪造，故限流键只用于防误操作
// 与粗粒度防刷，不能作为安全边界；权限判定始终走服务端会话。
func ClientIP(r *http.Request) string {
	if v := strings.TrimSpace(r.Header.Get("X-Forwarded-For")); v != "" {
		if i := strings.IndexByte(v, ','); i >= 0 {
			v = v[:i]
		}
		return strings.TrimSpace(v)
	}
	if v := strings.TrimSpace(r.Header.Get("X-Real-IP")); v != "" {
		return v
	}
	host := r.RemoteAddr
	if i := strings.LastIndexByte(host, ':'); i >= 0 {
		host = host[:i]
	}
	return host
}
