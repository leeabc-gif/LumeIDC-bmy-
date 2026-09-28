package refund

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"lumeidc/internal/money"
	"lumeidc/internal/plugin"
)

// clientMy 本人退款申请列表（分页）。
func (p *Plugin) clientMy(w http.ResponseWriter, r *http.Request) {
	userID, ok := plugin.RequireUserID(w, r)
	if !ok {
		return
	}
	page, limit := pageParam(r.URL.Query())
	rows, total, err := p.requests.ListByUser(r.Context(), userID, page, limit)
	if err != nil {
		plugin.JSONFail(w, "查询失败")
		return
	}
	list := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		list = append(list, requestItem(row, false))
	}
	plugin.WriteJSON(w, map[string]any{"ok": 1, "list": list, "total": total, "page": page, "limit": limit})
}

// clientMyStats 本人申请状态统计（待审核/已通过/已驳回），供前台统计卡使用。
func (p *Plugin) clientMyStats(w http.ResponseWriter, r *http.Request) {
	userID, ok := plugin.RequireUserID(w, r)
	if !ok {
		return
	}
	pending, approved, rejected, err := p.requests.StatsByUser(r.Context(), userID)
	if err != nil {
		plugin.JSONFail(w, "查询失败")
		return
	}
	plugin.WriteJSON(w, map[string]any{"ok": 1, "stats": map[string]int{
		"pending": pending, "approved": approved, "rejected": rejected,
	}})
}

// clientEligibleOrders 可退订单 + 表单选项（方式/原因/上限/期限），供申请页一次性取数。
//
// 分页：?limit&offset，limit 默认 100，硬上限 maxEligibleLimit=500；超过上限会自动 clamp，
// 避免客户端传 limit=100000 把整张 orders 表扫一遍。响应附带 has_more 让前端知道是否
// 还有下一页（防"前 100 条看起来全可退结果后面还有"的游标陷阱）。
func (p *Plugin) clientEligibleOrders(w http.ResponseWriter, r *http.Request) {
	userID, ok := plugin.RequireUserID(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	limit, offset := parseEligiblePage(r)
	orders, err := p.requests.EligibleOrders(ctx, userID, p.cfgInt(ctx, "refundWindowDays", defWindowDaysN), limit+1, offset)
	if err != nil {
		plugin.JSONFail(w, "查询失败")
		return
	}
	hasMore := false
	if len(orders) > limit {
		hasMore = true
		orders = orders[:limit]
	}
	globalMethods := p.cfgList(ctx, "refundMethods", defMethods)
	now := time.Now()
	list := make([]map[string]any, 0, len(orders))
	for _, o := range orders {
		// 按商品规则二次过滤可退期限：规则窗口优先于全局窗口。
		rule, _ := p.requests.RuleForProduct(ctx, o.ProductID)
		if rule != nil && !withinProductWindow(o.PaidAt, now, rule.WindowType, rule.WindowValue) {
			continue
		}
		// 按商品规则确定该订单允许的退款方式（规则优先，无规则回退全局）。
		allowed := globalMethods
		if rule != nil {
			allowed = allowedMethodsForRule(rule.RefundType, globalMethods)
		}
		// 按天退款：规则为 daily 时，展示的可退余额按已用天数折算。
		refundable := o.Refundable
		if rule != nil && rule.RefundRule == "daily" {
			if _, cents, perr := money.ParseNonNegative(o.Refundable, 999999999999); perr == nil {
				prorated := proratedRefundable(cents, o.PaidAt, o.Cycle)
				if prorated < cents {
					refundable = money.FormatCents(prorated)
				}
			}
		}
		list = append(list, map[string]any{
			"id":              o.ID,
			"amount":          o.Amount,
			"cycle":           o.Cycle,
			"paid_at":         fmtNullTime(o.PaidAt),
			"refundable":      refundable,
			"allowed_methods": allowed,
		})
	}
	out := p.clientOptions(ctx)
	out["ok"] = 1
	out["orders"] = list
	out["has_more"] = hasMore
	out["limit"] = limit
	out["offset"] = offset
	plugin.WriteJSON(w, out)
}

// maxEligibleLimit 可退订单接口单次硬上限。即便客户端传 limit=1_000_000，我们也只
// 给 500 条（多 1 用于探测 has_more），并附 has_more=true —— 防"内存 DoS / DB 扫描
// 整张 orders 表"。同时也是单页 UX 的合理上界。
const maxEligibleLimit = 500

// defaultEligibleLimit 前端默认拉多少（与历史行为一致：100）。
const defaultEligibleLimit = 100

// parseEligiblePage 把 ?limit&offset 解析成安全的分页参数。负数 / 超大都会被 clamp。
func parseEligiblePage(r *http.Request) (limit, offset int) {
	limit = defaultEligibleLimit
	offset = 0
	if v := strings.TrimSpace(r.URL.Query().Get("limit")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if v := strings.TrimSpace(r.URL.Query().Get("offset")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			offset = n
		}
	}
	if limit > maxEligibleLimit {
		limit = maxEligibleLimit
	}
	if limit < 1 {
		limit = 1
	}
	return limit, offset
}

// withinProductWindow 按商品规则的窗口类型/数值判断订单是否在可退期限内。
// windowValue<=0 表示不限。
func withinProductWindow(paidAt sql.NullTime, now time.Time, windowType string, windowValue int) bool {
	if windowValue <= 0 {
		return true
	}
	if !paidAt.Valid {
		return false
	}
	var d time.Duration
	switch windowType {
	case "hours":
		d = time.Duration(windowValue) * time.Hour
	default:
		d = time.Duration(windowValue) * 24 * time.Hour
	}
	return !now.After(paidAt.Time.Add(d))
}

// allowedMethodsForRule 按商品规则 refund_type 映射允许的退款方式。
func allowedMethodsForRule(refundType string, fallback []string) []string {
	switch refundType {
	case "balance":
		return []string{methodBalance}
	case "balance_gateway":
		return []string{methodBalance, methodGateway}
	case "gateway_record":
		return []string{methodGateway}
	default:
		return fallback
	}
}

// clientCreate 用户提交退款申请：校验订单归属/期限/金额/方式/原因后落库，
// 命中自动通过条件时内联执行 approveFlow（失败自动回退待审）。
func (p *Plugin) clientCreate(w http.ResponseWriter, r *http.Request) {
	userID, ok := plugin.RequireUserID(w, r)
	if !ok {
		return
	}
	vals, err := parseFormValues(r)
	if err != nil {
		plugin.JSONFail(w, "请求格式错误")
		return
	}
	orderID, err := strconv.ParseInt(strings.TrimSpace(vals["order_id"]), 10, 64)
	if err != nil || orderID <= 0 {
		plugin.StatusFail(w, 400, "参数错误")
		return
	}
	ctx := r.Context()

	order, err := p.requests.PaidOrderByUser(ctx, orderID, userID)
	if err != nil {
		plugin.JSONFail(w, "查询失败")
		return
	}
	if order == nil {
		plugin.JSONFail(w, "订单不存在或不可退款")
		return
	}
	// 按商品取退款规则（精确匹配优先，回退全局默认，无则 nil 走全局配置）。
	productID, _ := p.requests.ProductIDByOrder(ctx, orderID)
	rule, _ := p.requests.RuleForProduct(ctx, productID)

	// 退款要求：规则优先，未配置规则则不限。
	if rule != nil {
		switch rule.RefundRequirement {
		case "first_order":
			first, ferr := p.requests.IsFirstPaidOrder(ctx, orderID, userID)
			if ferr != nil {
				plugin.JSONFail(w, "查询失败")
				return
			}
			if !first {
				plugin.JSONFail(w, "仅限首笔订单可退款")
				return
			}
		case "first_order_of_product":
			first, ferr := p.requests.IsFirstPaidOrderOfProduct(ctx, orderID, userID, productID)
			if ferr != nil {
				plugin.JSONFail(w, "查询失败")
				return
			}
			if !first {
				plugin.JSONFail(w, "仅限该商品首笔订单可退款")
				return
			}
		}
	}

	// 可退期限：规则优先（支持天/小时），回退全局 refundWindowDays。
	if rule != nil {
		if !withinProductWindow(order.PaidAt, time.Now(), rule.WindowType, rule.WindowValue) {
			plugin.JSONFail(w, "已超过可退期限")
			return
		}
	} else if days := p.cfgInt(ctx, "refundWindowDays", defWindowDaysN); days > 0 {
		if !order.PaidAt.Valid {
			plugin.JSONFail(w, "订单支付时间异常，不可退款")
			return
		}
		if !withinWindow(order.PaidAt.Time, time.Now(), days) {
			plugin.JSONFail(w, "已超过可退期限")
			return
		}
	}
	amountText, amountCents, err := money.ParsePositive(strings.TrimSpace(vals["amount"]), 999999999999)
	if err != nil {
		plugin.JSONFail(w, "退款金额格式不正确")
		return
	}
	refunded, err := p.requests.RefundedTotal(ctx, orderID)
	if err != nil {
		plugin.JSONFail(w, "查询失败")
		return
	}
	_, orderCents, err := money.ParseNonNegative(order.Amount, 999999999999)
	if err != nil {
		plugin.JSONFail(w, "订单金额异常")
		return
	}
	_, refundedCents, err := money.ParseNonNegative(refunded, 999999999999)
	if err != nil {
		plugin.JSONFail(w, "查询失败")
		return
	}
	// 可退余额 = 订单实付 - 已退。
	refundable := orderCents - refundedCents
	// 按天退款：规则为 daily 时，可退余额按已用天数比例折算。
	if rule != nil && rule.RefundRule == "daily" {
		prorated := proratedRefundable(orderCents, order.PaidAt, order.Cycle)
		if prorated < refundable {
			refundable = prorated
		}
	}
	if refundable <= 0 || amountCents > refundable {
		plugin.JSONFail(w, "退款金额超出可退余额")
		return
	}
	if maxCents := p.cfgCents(ctx, "refundMaxAmount", 0); maxCents > 0 && amountCents > maxCents {
		plugin.JSONFail(w, "单笔退款金额超出上限")
		return
	}
	method := strings.TrimSpace(vals["method"])
	if method == "" {
		method = "balance"
	}
	// 退款方式白名单：规则优先（按 refund_type 映射），回退全局 refundMethods。
	var allowedMethods []string
	if rule != nil {
		switch rule.RefundType {
		case "balance":
			allowedMethods = []string{"balance"}
		case "balance_gateway":
			allowedMethods = []string{"balance", "gateway"}
		case "gateway_record":
			allowedMethods = []string{"gateway"}
		default:
			allowedMethods = p.cfgList(ctx, "refundMethods", defMethods)
		}
	} else {
		allowedMethods = p.cfgList(ctx, "refundMethods", defMethods)
	}
	if !contains(allowedMethods, method) {
		plugin.JSONFail(w, "不支持的退款方式")
		return
	}
	reason := strings.TrimSpace(vals["reason"])
	reasons := splitLines(p.cfg(ctx, "refundReasons", defReasons))
	if reason == "" {
		plugin.JSONFail(w, "请选择退款原因")
		return
	}
	if len(reasons) > 0 && !contains(reasons, reason) {
		plugin.JSONFail(w, "退款原因不在可选范围")
		return
	}
	detail := strings.TrimSpace(vals["detail"])
	if len([]rune(detail)) > 500 {
		plugin.JSONFail(w, "补充说明过长")
		return
	}

	id, err := p.requests.Create(ctx, &Request{
		OrderID: orderID, UserID: userID, Amount: amountText,
		Reason: reason, Detail: detail, Method: method, Status: statusPending,
	})
	if errors.Is(err, ErrExists) {
		plugin.JSONFail(w, "该订单有进行中的退款申请")
		return
	}
	if err != nil {
		plugin.JSONFail(w, "提交失败")
		return
	}
	plugin.Emit(ctx, EventRefundRequestCreated, RefundPayload{
		RequestID: id, OrderID: orderID, UserID: userID, Amount: amountText, Status: statusPending,
	})
	if p.cfgBool(ctx, "notifyAdminOnSubmit", true) && p.host.Notify != nil {
		body := fmt.Sprintf("用户 #%d 对订单 #%d 提交退款申请，金额 %s 元，原因：%s。", userID, orderID, amountText, reason)
		_ = p.host.Notify.NotifyAdminOnce(ctx, "refund_request:"+time.Now().Format("2006-01-02"),
			"退款申请", "新的退款申请", body)
	}
	// 多渠道机器人通知（企微/钉钉/飞书）。
	p.notifyChannels(ctx, "新退款申请",
		fmt.Sprintf("用户 #%d 对订单 #%d 提交退款申请\n金额：%s 元\n原因：%s", userID, orderID, amountText, reason))

	// 审核模式：规则优先，回退全局配置。
	reviewMode := p.cfg(ctx, "reviewMode", defReviewMode)
	autoMaxCents := p.cfgCents(ctx, "autoApproveMax", 0)
	if rule != nil {
		reviewMode = rule.ReviewMode
		if rule.AutoApproveMax != "" && rule.AutoApproveMax != "0" {
			_, autoMaxCents, _ = money.ParseNonNegative(rule.AutoApproveMax, 999999999999)
		} else {
			autoMaxCents = 0
		}
	}
	if shouldAutoApprove(reviewMode, autoMaxCents, amountCents) {
		req, gerr := p.requests.Get(ctx, id)
		if gerr == nil && req != nil {
			if msg := p.approveFlow(ctx, req, sql.NullInt64{}); msg != "" {
				plugin.WriteJSON(w, map[string]any{"ok": 1, "status": statusPending, "message": "已提交，等待人工审核"})
				return
			}
			plugin.WriteJSON(w, map[string]any{"ok": 1, "status": statusApproved, "message": "已通过并完成退款"})
			return
		}
	}
	plugin.WriteJSON(w, map[string]any{"ok": 1, "status": statusPending, "message": "已提交，等待审核"})
}

// clientWithdraw 用户撤回本人待审核申请。
func (p *Plugin) clientWithdraw(w http.ResponseWriter, r *http.Request) {
	userID, ok := plugin.RequireUserID(w, r)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		plugin.StatusFail(w, 400, "参数错误")
		return
	}
	done, err := p.requests.Withdraw(r.Context(), id, userID)
	if err != nil {
		plugin.JSONFail(w, "操作失败")
		return
	}
	if !done {
		plugin.JSONFail(w, "申请不存在或已处理")
		return
	}
	plugin.WriteJSON(w, map[string]any{"ok": 1})
}
