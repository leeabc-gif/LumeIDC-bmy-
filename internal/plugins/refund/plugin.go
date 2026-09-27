// Package refund 用户自助退款插件：用户对已支付订单提交退款申请，
// 管理员审核通过后由框架统一执行核心退款（Payment.Refund），执行失败自动
// 回滚为待审可重试。支持可退期限、退款方式、原因选项、审核模式与通知开关。
package refund

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"io/fs"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"lumeidc/internal/money"
	"lumeidc/internal/plugin"
)

const Name = "refund"

// 插件事件（供 webhooknotify 等订阅）。
const (
	EventRefundRequestCreated = "refund_request.created"
	EventRefundApproved       = "refund_request.approved"
	EventRefundRejected       = "refund_request.rejected"
)

// RefundPayload 退款事件负载。
type RefundPayload struct {
	RequestID int64  `json:"requestId"`
	OrderID   int64  `json:"orderId"`
	UserID    int64  `json:"userId"`
	Amount    string `json:"amount"`
	Status    string `json:"status"`
}

// 申请状态。
const (
	statusPending   = "pending"
	statusApproved  = "approved"
	statusRejected  = "rejected"
	statusWithdrawn = "withdrawn"
)

// statusLabels 状态中文标签。
var statusLabels = map[string]string{
	statusPending:   "待审核",
	statusApproved:  "已通过",
	statusRejected:  "已驳回",
	statusWithdrawn: "已撤回",
}

// methodLabels 退款方式中文标签。
var methodLabels = map[string]string{
	"balance": "退回余额",
	"gateway": "原路退回",
}

// 退款方式常量。
const (
	methodBalance = "balance"
	methodGateway = "gateway"
)

// 配置默认值（框架配置页只在保存后写 settings，未保存时 Config 返回空串，
// 故读取处按此兜底，保证与配置页展示的默认值一致）。
const (
	defWindowDays  = "7"
	defMethods     = `["balance"]`
	defReasons     = "不想要了\n功能不符合预期\n重复购买\n服务不稳定\n其他"
	defReviewMode  = "manual"
	defAutoMax     = "0"
	defMaxAmount   = "0"
	defNotifyAdmin = "1"
	defNotifyUser  = "1"
)

func init() {
	plugin.RegisterEvent(EventRefundRequestCreated, "用户提交退款申请")
	plugin.RegisterEvent(EventRefundApproved, "退款申请审核通过")
	plugin.RegisterEvent(EventRefundRejected, "退款申请被驳回")
	plugin.Register(&Plugin{})
}

// Plugin 用户自助退款插件。
type Plugin struct {
	host     *plugin.Host
	requests *Requests
	// adminLimiter 管理端审核操作限流器（按 adminID|IP|op 三重键；懒加载）。
	// 拒防审核被脚本批量点击 / 误操作刷量；纯内存无外部依赖，语义见 plugin.RateLimiter。
	adminLimiterOnce sync.Once
	adminLimiter     *plugin.RateLimiter
}

func (p *Plugin) limiter() *plugin.RateLimiter {
	p.adminLimiterOnce.Do(func() {
		p.adminLimiter = plugin.NewRateLimiter()
	})
	return p.adminLimiter
}

func (p *Plugin) Info() plugin.Info {
	return plugin.Info{
		Name:        Name,
		Title:       "用户自助退款",
		Version:     "1.0.0",
		Description: "用户对已支付订单自助提交退款申请，管理员审核通过后自动执行核心退款；支持可退期限、退款方式、金额上限、自动审核与通知配置。",
	}
}

func (p *Plugin) Init(h *plugin.Host) error {
	p.host = h
	p.requests = NewRequests(h.DB)
	return nil
}

//go:embed migrations/*.sql
var migrationsFS embed.FS

func (p *Plugin) Migrations() fs.FS { return migrationsFS }

func (p *Plugin) AdminMenu() plugin.MenuItem {
	return plugin.MenuItem{Title: "退款管理", Icon: "ri:refund-line", Parent: plugin.MenuGroupUsers}
}

func (p *Plugin) ClientPage() plugin.MenuItem {
	return plugin.MenuItem{Title: "申请退款", Icon: "ri:refund-line", To: "/plugin/refund"}
}

func (p *Plugin) ConfigSchema() []plugin.ConfigField {
	return []plugin.ConfigField{
		{Key: "refundWindowDays", Title: "可退期限（天）", Type: "number", Default: defWindowDays,
			Tip: "支付后 N 天内可申请，0 表示不限"},
		{Key: "refundMethods", Title: "允许的退款方式", Type: "multiselect", Default: defMethods,
			Options: []plugin.ConfigOption{
				{Value: "balance", Label: "退回余额"},
				{Value: "gateway", Label: "原路退回（仅记账，需人工处理）"},
			}},
		{Key: "refundReasons", Title: "退款原因选项", Type: "textarea", Default: defReasons,
			Tip: "每行一个；用户下拉选择"},
		{Key: "reviewMode", Title: "审核模式", Type: "select", Default: defReviewMode,
			Options: []plugin.ConfigOption{
				{Value: "manual", Label: "人工审核"},
				{Value: "auto", Label: "金额≤阈值自动通过"},
			}},
		{Key: "autoApproveMax", Title: "自动通过金额阈值", Type: "number", Default: defAutoMax,
			Tip: "0 表示不自动通过；仅自动模式生效"},
		{Key: "refundMaxAmount", Title: "单笔退款上限", Type: "number", Default: defMaxAmount,
			Tip: "0 表示不限"},
		{Key: "notifyAdminOnSubmit", Title: "新申请通知管理员", Type: "switch", Default: defNotifyAdmin},
		{Key: "notifyUserOnResult", Title: "审核结果通知用户", Type: "switch", Default: defNotifyUser},
		// ---- 多渠道机器人通知 ----
		{Key: "notifyWecom", Title: "企业微信通知", Type: "switch", Default: "0"},
		{Key: "notifyWecomUrl", Title: "企业微信机器人 Webhook", Type: "text",
			Tip: "开启后新退款申请/审核结果将推送到该机器人"},
		{Key: "notifyDingtalk", Title: "钉钉通知", Type: "switch", Default: "0"},
		{Key: "notifyDingtalkUrl", Title: "钉钉机器人 Webhook", Type: "text",
			Tip: "开启后新退款申请/审核结果将推送到该机器人"},
		{Key: "notifyFeishu", Title: "飞书通知", Type: "switch", Default: "0"},
		{Key: "notifyFeishuUrl", Title: "飞书机器人 Webhook", Type: "text",
			Tip: "开启后新退款申请/审核结果将推送到该机器人"},
	}
}

func (p *Plugin) RegisterAdminRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /list", p.adminList)
	mux.HandleFunc("GET /stats", p.adminStats)
	mux.HandleFunc("POST /{id}/approve", p.adminApprove)
	mux.HandleFunc("POST /{id}/reject", p.adminReject)
	// 商品退款规则管理
	mux.HandleFunc("GET /product-rules", p.adminProductRules)
	mux.HandleFunc("GET /products", p.adminProducts)
	mux.HandleFunc("POST /product-rules", p.adminSaveProductRule)
	mux.HandleFunc("DELETE /product-rules/{id}", p.adminDeleteProductRule)
}

func (p *Plugin) RegisterClientRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /my", p.clientMy)
	mux.HandleFunc("GET /my-stats", p.clientMyStats)
	mux.HandleFunc("GET /eligible-orders", p.clientEligibleOrders)
	mux.HandleFunc("POST /create", p.clientCreate)
	mux.HandleFunc("POST /{id}/withdraw", p.clientWithdraw)
}

// ---- 配置读取（空值兜底默认） ----

// cfg 读取配置；settings 未写入时回退默认值。
func (p *Plugin) cfg(ctx context.Context, key, def string) string {
	if v := strings.TrimSpace(p.host.Config(ctx, key)); v != "" {
		return v
	}
	return def
}

// cfgInt 读取非负整数配置；非法回退默认值。
func (p *Plugin) cfgInt(ctx context.Context, key string, def int) int {
	n, err := strconv.Atoi(p.cfg(ctx, key, strconv.Itoa(def)))
	if err != nil || n < 0 {
		return def
	}
	return n
}

// cfgCents 读取金额配置（元）为分值；非法回退默认值。
func (p *Plugin) cfgCents(ctx context.Context, key string, def int64) int64 {
	_, cents, err := money.ParseNonNegative(p.cfg(ctx, key, "0"), 999999999999)
	if err != nil {
		return def
	}
	return cents
}

// cfgBool 读取开关配置；未设置时按默认值。
func (p *Plugin) cfgBool(ctx context.Context, key string, def bool) bool {
	v := strings.TrimSpace(p.host.Config(ctx, key))
	if v == "" {
		return def
	}
	return parseBool(v)
}

// cfgList 读取多选/多行配置为选项列表（multiselect 存 JSON 数组字符串）。
func (p *Plugin) cfgList(ctx context.Context, key, def string) []string {
	raw := p.cfg(ctx, key, def)
	if strings.HasPrefix(raw, "[") {
		var arr []string
		if err := json.Unmarshal([]byte(raw), &arr); err == nil {
			out := make([]string, 0, len(arr))
			for _, v := range arr {
				if v = strings.TrimSpace(v); v != "" {
					out = append(out, v)
				}
			}
			return out
		}
	}
	return splitLines(raw)
}

// clientOptions 前台申请表单选项（方式/原因/上限/期限/审核模式）。
func (p *Plugin) clientOptions(ctx context.Context) map[string]any {
	methods := make([]map[string]string, 0, 2)
	for _, m := range p.cfgList(ctx, "refundMethods", defMethods) {
		methods = append(methods, map[string]string{"value": m, "label": methodLabels[m]})
	}
	return map[string]any{
		"methods":  methods,
		"reasons":  splitLines(p.cfg(ctx, "refundReasons", defReasons)),
		"maxCents": p.cfgCents(ctx, "refundMaxAmount", 0),
		"maxText":  money.FormatCents(p.cfgCents(ctx, "refundMaxAmount", 0)),
		"windowDays": p.cfgInt(ctx, "refundWindowDays", 7),
	}
}

// ---- 纯函数（供单测） ----

// cycleDays 订单计费周期对应的天数（按天退款用）。
func cycleDays(cycle string) int {
	switch cycle {
	case "monthly":
		return 30
	case "quarterly":
		return 90
	case "yearly":
		return 365
	default:
		return 30
	}
}

// proratedRefundable 按天退款：可退金额 = 订单金额 × (1 - 已用天数/周期天数)。
// 已用天数不超过周期天数；支付时间无效时回退全额。
func proratedRefundable(orderCents int64, paidAt sql.NullTime, cycle string) int64 {
	if !paidAt.Valid {
		return orderCents
	}
	total := int64(cycleDays(cycle))
	if total <= 0 {
		return orderCents
	}
	used := int64(time.Since(paidAt.Time).Hours() / 24)
	if used < 0 {
		used = 0
	}
	if used >= total {
		return 0
	}
	return orderCents * (total - used) / total
}

// withinWindow 支付时间是否仍在可退期限内；days<=0 表示不限。
func withinWindow(paidAt, now time.Time, days int) bool {
	if days <= 0 {
		return true
	}
	return !now.After(paidAt.Add(time.Duration(days) * 24 * time.Hour))
}

// shouldAutoApprove 自动通过判定：仅自动审核模式、阈值>0 且金额不超过阈值。
func shouldAutoApprove(mode string, maxCents, amountCents int64) bool {
	return mode == "auto" && maxCents > 0 && amountCents <= maxCents
}

// splitLines 解析多行文本配置为选项（去首尾空白与空行，保持顺序去重）。
func splitLines(s string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, 8)
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || seen[line] {
			continue
		}
		seen[line] = true
		out = append(out, line)
	}
	return out
}

// contains 选项白名单校验。
func contains(opts []string, v string) bool {
	for _, o := range opts {
		if o == v {
			return true
		}
	}
	return false
}

// isValidDecimal 校验字符串是否为合法非负十进制数（允许空串由调用方处理）。
func isValidDecimal(s string) bool {
	if s == "" {
		return false
	}
	seenDot := false
	for i, ch := range s {
		if ch == '.' {
			if seenDot || i == 0 || i == len(s)-1 {
				return false
			}
			seenDot = true
			continue
		}
		if ch < '0' || ch > '9' {
			return false
		}
	}
	return true
}

// ---- 管理端审核操作限流（防刷 / 防误操作） ----

// checkAdminRate 管理员入口统一节流闸门（approve/reject 共用）。
// 限流强度取插件配置 adminApproveRatePerMin（<=0 表示关闭，本地开发/单管理员机房）；
// 键为 <adminID>|<ip>|<op>，限额语义详见 plugin.RateLimiter.AllowAdmin。
func (p *Plugin) checkAdminRate(r *http.Request, adminID int64, op string) (bool, time.Duration) {
	rate := p.cfgInt(r.Context(), "adminApproveRatePerMin", 30)
	return p.limiter().AllowAdmin(r, rate, adminID, op)
}
