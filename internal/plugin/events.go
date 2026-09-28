package plugin

import (
	"context"
	"log"
	"sort"
	"sync"
	"time"
)

// EventHandler 事件订阅回调。返回 error 仅记录日志，不影响其他订阅者与主流程。
type EventHandler func(ctx context.Context, payload any) error

// FilterHandler 过滤器回调：接收并返回（可能修改后的）值。
// 返回 error 或 panic 时该过滤器被跳过（保留当前值继续后续过滤器）。
type FilterHandler func(ctx context.Context, value any) (any, error)

type subscriber struct {
	plugin string // 归属插件名（启用态过滤用）
	fn     EventHandler
}

type filterSubscriber struct {
	plugin string
	fn     FilterHandler
}

var (
	eventMu       sync.RWMutex
	eventHandlers = map[string][]subscriber{}
	filterHandles = map[string][]filterSubscriber{}
)

// Subscribe 订阅事件。通常由插件 Init 经 Host.Subscribe 调用（自动带插件归属）。
// 直接包级调用时归属为空串（不受启停过滤，供核心内部使用）。
func Subscribe(event string, fn EventHandler) {
	subscribeAs("", event, fn)
}

func subscribeAs(pluginName, event string, fn EventHandler) {
	eventMu.Lock()
	defer eventMu.Unlock()
	eventHandlers[event] = append(eventHandlers[event], subscriber{plugin: pluginName, fn: fn})
}

// SubscribeFilter 注册过滤器。包级调用归属为空串（核心用）；插件经 Host.SubscribeFilter。
func SubscribeFilter(name string, fn FilterHandler) {
	subscribeFilterAs("", name, fn)
}

func subscribeFilterAs(pluginName, name string, fn FilterHandler) {
	eventMu.Lock()
	defer eventMu.Unlock()
	filterHandles[name] = append(filterHandles[name], filterSubscriber{plugin: pluginName, fn: fn})
}

type eventNameKey struct{}

// EventName 返回当前正在分发的事件名（通配 "*" 订阅者据此区分事件；普通订阅为空也可用）。
func EventName(ctx context.Context) string {
	v, _ := ctx.Value(eventNameKey{}).(string)
	return v
}

// Emit 同步广播事件：按订阅顺序逐个执行；跳过已禁用插件的订阅者；
// 订阅 "*" 的处理器接收全部事件（webhook 类通用订阅用，晚注册插件的事件也能覆盖）。
// 每个 handler 独立 recover，panic/返回错误仅记录日志，绝不阻断主流程
// （对齐魔方 hook() 语义：插件崩不影响核心）。
// ponytail: 同步执行，插件慢逻辑会拖慢主流程；重活请 handler 内自行 go func()
// 并做好退出管理。异步事件队列入三期。
func Emit(ctx context.Context, event string, payload any) {
	eventMu.RLock()
	hs := append([]subscriber(nil), eventHandlers[event]...)
	hs = append(hs, eventHandlers["*"]...)
	eventMu.RUnlock()
	ctx = context.WithValue(ctx, eventNameKey{}, event)
	for _, s := range hs {
		if s.plugin != "" && !Enabled(s.plugin) {
			continue
		}
		func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("插件事件 %s 处理崩溃: %v", event, r)
				}
			}()
			if err := s.fn(ctx, payload); err != nil {
				log.Printf("插件事件 %s 处理失败: %v", event, err)
			}
		}()
	}
}

// ApplyFilters 链式执行过滤器：每个过滤器收到上一步的值并返回新值；
// 单个过滤器禁用/出错/panic 时保留当前值继续（记日志）。
// 返回值即最终值（无任何过滤器时原样返回）。
func ApplyFilters(ctx context.Context, name string, value any) any {
	eventMu.RLock()
	fs := append([]filterSubscriber(nil), filterHandles[name]...)
	eventMu.RUnlock()
	cur := value
	for _, f := range fs {
		if f.plugin != "" && !Enabled(f.plugin) {
			continue
		}
		next, ok := callFilter(ctx, name, f.fn, cur)
		if ok {
			cur = next
		}
	}
	return cur
}

func callFilter(ctx context.Context, name string, fn FilterHandler, value any) (out any, ok bool) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("插件过滤器 %s 处理崩溃: %v", name, r)
			out, ok = nil, false
		}
	}()
	out, err := fn(ctx, value)
	if err != nil {
		log.Printf("插件过滤器 %s 处理失败: %v", name, err)
		return nil, false
	}
	return out, true
}

// ---- 事件目录（订阅方据此展示可选事件，如 webhook 插件的事件勾选） ----

// EventMeta 事件目录条目。
type EventMeta struct {
	Name  string `json:"name"`
	Label string `json:"label"`
}

var (
	catalogMu sync.RWMutex
	catalog   = map[string]string{}
)

// RegisterEvent 登记事件（名称 + 中文标签）。核心事件在包初始化时预注册；
// 插件可在 init/Init 中登记自定义事件供订阅方发现。
func RegisterEvent(name, label string) {
	catalogMu.Lock()
	defer catalogMu.Unlock()
	catalog[name] = label
}

// EventCatalog 返回全部已登记事件（按名称排序，顺序稳定）。
func EventCatalog() []EventMeta {
	catalogMu.RLock()
	defer catalogMu.RUnlock()
	out := make([]EventMeta, 0, len(catalog))
	for name, label := range catalog {
		out = append(out, EventMeta{Name: name, Label: label})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// 核心事件常量；新增事件在此登记常量并 RegisterEvent 中文标签。埋点位置见各 service/handler。
const (
	EventOrderPaid         = "order.paid"          // payload: OrderPaidPayload
	EventOrderCreated      = "order.created"       // payload: OrderCreatedPayload
	EventServiceCreated    = "service.created"     // payload: ServicePayload
	EventServiceSuspended  = "service.suspended"   // payload: ServicePayload
	EventServiceTerminated = "service.terminated"  // payload: ServicePayload
	EventServiceRenewed    = "service.renewed"     // payload: ServicePayload
	EventServiceUnsuspended = "service.unsuspended" // payload: ServicePayload
	EventUserRegistered    = "user.registered"     // payload: UserPayload
	EventUserLogin         = "user.login"          // payload: UserPayload
	EventInvoiceExpired    = "invoice.expired"     // payload: InvoiceExpiredPayload
	EventRefundCreated     = "refund.created"      // payload: RefundPayload
	EventIdentitySubmitted = "identity.submitted"  // payload: UserPayload
	EventIdentityReviewed  = "identity.reviewed"   // payload: IdentityReviewedPayload
	EventServiceExpiring   = "service.expiring"    // payload: ServiceExpiringPayload
	EventPromotionEnding   = "promotion.ending"    // payload: PromotionEndingPayload
)

func init() {
	RegisterEvent(EventOrderPaid, "订单支付成功")
	RegisterEvent(EventOrderCreated, "订单创建")
	RegisterEvent(EventServiceCreated, "服务开通交付")
	RegisterEvent(EventServiceSuspended, "服务停用")
	RegisterEvent(EventServiceTerminated, "服务删除")
	RegisterEvent(EventServiceRenewed, "服务续费成功")
	RegisterEvent(EventServiceUnsuspended, "服务解除停用")
	RegisterEvent(EventUserRegistered, "用户注册成功")
	RegisterEvent(EventUserLogin, "用户登录")
	RegisterEvent(EventInvoiceExpired, "账单过期关闭")
	RegisterEvent(EventRefundCreated, "退款单创建")
	RegisterEvent(EventIdentitySubmitted, "实名认证提交")
	RegisterEvent(EventIdentityReviewed, "实名认证审核")
	RegisterEvent(EventServiceExpiring, "服务到期提醒已发送")
	RegisterEvent(EventPromotionEnding, "活动即将结束提醒已发送")
}

// OrderPaidPayload 订单/账单支付成功。
type OrderPaidPayload struct {
	OrderID   int64   `json:"orderId"`
	InvoiceID int64   `json:"invoiceId"`
	InvoiceNo string  `json:"invoiceNo"`
	UserID    int64   `json:"userId"`
	Amount    float64 `json:"amount"`
}

// OrderCreatedPayload 订单创建（Kind: new 新购 / renew 续费 / upgrade 升降级）。
type OrderCreatedPayload struct {
	OrderID   int64   `json:"orderId"`
	UserID    int64   `json:"userId"`
	ProductID int64   `json:"productId"`
	Amount    float64 `json:"amount"`
	Kind      string  `json:"kind"`
}

// RefundPayload 退款单创建。
type RefundPayload struct {
	RefundID int64   `json:"refundId"`
	OrderID  int64   `json:"orderId"`
	UserID   int64   `json:"userId"`
	Amount   float64 `json:"amount"`
	Reason   string  `json:"reason"`
}

// IdentityReviewedPayload 实名认证审核。
type IdentityReviewedPayload struct {
	UserID   int64 `json:"userId"`
	Approved bool  `json:"approved"`
}

// ServicePayload 服务实例生命周期事件（开通交付/停用/删除/续费）。
type ServicePayload struct {
	ServiceID int64 `json:"serviceId"`
	UserID    int64 `json:"userId"`
	ProductID int64 `json:"productId"`
}

// UserPayload 用户注册/登录。
type UserPayload struct {
	UserID int64 `json:"userId"`
}

// InvoiceExpiredPayload 账单过期关闭（cron 批量处理）。
type InvoiceExpiredPayload struct {
	InvoiceIDs []int64 `json:"invoiceIds"`
	Count      int     `json:"count"`
}

// ServiceExpiringPayload 服务到期提醒已发送（cron 按服务逐条触发）。
type ServiceExpiringPayload struct {
	ServiceID int64     `json:"serviceId"`
	UserID    int64     `json:"userId"`
	ProductID int64     `json:"productId"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// PromotionEndingPayload 活动即将结束提醒已发送（cron 按活动去重触发）。
type PromotionEndingPayload struct {
	PromotionID int64     `json:"promotionId"`
	Name        string    `json:"name"`
	EndsAt      time.Time `json:"endsAt"`
}

// NotificationMessage 通知发送前过滤器（notify.message）的可改载荷。
// 插件可改写 Title/Body/Values；Code 与 UserID 供判定场景，不应修改。
type NotificationMessage struct {
	UserID int64             `json:"userId"`
	Code   string            `json:"code"`
	Title  string            `json:"title"`
	Body   string            `json:"body"`
	Values map[string]string `json:"values,omitempty"`
}

// FilterNotifyMessage 通知发送前过滤器名。
const FilterNotifyMessage = "notify.message"
