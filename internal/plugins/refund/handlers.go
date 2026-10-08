package refund

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"lumeidc/internal/money"
	"lumeidc/internal/plugin"
)

// parseBool 表单布尔解析约定：空/"0"/"false" 为 false，其余为 true。
func parseBool(v string) bool {
	return v != "" && v != "0" && v != "false"
}

// parseFormValues 解析请求体：JSON（application/json）或表单，统一为 map[string]string。
func parseFormValues(r *http.Request) (map[string]string, error) {
	if strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "application/json") {
		dec := json.NewDecoder(r.Body)
		dec.UseNumber()
		var raw map[string]any
		if err := dec.Decode(&raw); err != nil {
			return nil, err
		}
		vals := map[string]string{}
		for k, v := range raw {
			switch tv := v.(type) {
			case string:
				vals[k] = tv
			case bool:
				if tv {
					vals[k] = "1"
				}
			case json.Number:
				vals[k] = tv.String()
			}
		}
		return vals, nil
	}
	if err := r.ParseForm(); err != nil {
		return nil, err
	}
	vals := map[string]string{}
	for k := range r.PostForm {
		vals[k] = r.PostFormValue(k)
	}
	return vals, nil
}

// pageParam 分页参数：page 失败或 <1 → 1；limit 失败或 <1 或 >50 → 10。
func pageParam(q url.Values) (page, limit int) {
	page, err := strconv.Atoi(q.Get("page"))
	if err != nil || page < 1 {
		page = 1
	}
	limit, err = strconv.Atoi(q.Get("limit"))
	if err != nil || limit < 1 || limit > 50 {
		limit = 10
	}
	return page, limit
}

// fmtNullTime NullTime 格式化（无效返回空串）。
func fmtNullTime(t sql.NullTime) string {
	if !t.Valid {
		return ""
	}
	return t.Time.Format("2006-01-02 15:04")
}

// requestItem 申请 JSON 视图（admin=true 附带用户邮箱与处理人 ID）。
func requestItem(r Row, admin bool) map[string]any {
	m := map[string]any{
		"id":            r.ID,
		"order_id":      r.OrderID,
		"user_id":       r.UserID,
		"user_name":     r.UserName,
		"amount":        r.Amount,
		"reason":        r.Reason,
		"detail":        r.Detail,
		"method":        r.Method,
		"method_label":  methodLabels[r.Method],
		"status":        r.Status,
		"status_label":  statusLabels[r.Status],
		"handle_note":   r.HandleNote,
		"refund_id":     r.RefundID.Int64,
		"order_amount":  r.OrderAmount,
		"order_cycle":   r.OrderCycle,
		"order_paid_at": fmtNullTime(r.OrderPaidAt),
		"handled_at":    fmtNullTime(r.HandledAt),
		"created_at":    r.CreatedAt.Format("2006-01-02 15:04"),
		"updated_at":    r.UpdatedAt.Format("2006-01-02 15:04"),
	}
	if admin {
		m["user_email"] = r.UserEmail
		m["handled_by"] = r.HandledBy.Int64
	}
	return m
}

func (p *Plugin) adminList(w http.ResponseWriter, r *http.Request) {
	if !plugin.AdminOK(w, r) {
		return
	}
	q := r.URL.Query()
	page, limit := pageParam(q)
	rows, total, err := p.requests.AdminList(r.Context(), Filter{
		Keyword: strings.TrimSpace(q.Get("keyword")),
		Status:  strings.TrimSpace(q.Get("status")),
		Page:    page,
		Limit:   limit,
	})
	if err != nil {
		plugin.JSONFail(w, "查询失败")
		return
	}
	list := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		list = append(list, requestItem(row, true))
	}
	plugin.WriteJSON(w, map[string]any{"ok": 1, "list": list, "total": total, "page": page, "limit": limit})
}

func (p *Plugin) adminStats(w http.ResponseWriter, r *http.Request) {
	if !plugin.AdminOK(w, r) {
		return
	}
	pending, approved, rejected, err := p.requests.Stats(r.Context())
	if err != nil {
		plugin.JSONFail(w, "查询失败")
		return
	}
	plugin.WriteJSON(w, map[string]any{"ok": 1, "stats": map[string]int{
		"pending": pending, "approved": approved, "rejected": rejected,
	}})
}

func (p *Plugin) adminApprove(w http.ResponseWriter, r *http.Request) {
	if !plugin.AdminOK(w, r) {
		return
	}
	sess, ok := plugin.AdminSession(w, r)
	if !ok {
		return
	}
	if allowed, retry := p.checkAdminRate(r, sess.UserID, "approve"); !allowed {
		w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())+1))
		plugin.StatusFail(w, http.StatusTooManyRequests, "操作过于频繁，请稍后再试")
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		plugin.StatusFail(w, 400, "参数错误")
		return
	}
	ctx := r.Context()
	req, err := p.requests.Get(ctx, id)
	if err != nil {
		plugin.JSONFail(w, "查询失败")
		return
	}
	if req == nil {
		plugin.StatusFail(w, 404, "申请不存在")
		return
	}
	if req.Status != statusPending {
		plugin.JSONFail(w, "该申请已处理")
		return
	}
	if msg := p.approveFlow(ctx, req, sql.NullInt64{Int64: sess.UserID, Valid: true}); msg != "" {
		plugin.JSONFail(w, msg)
		return
	}
	plugin.WriteJSON(w, map[string]any{"ok": 1})
}

func (p *Plugin) adminReject(w http.ResponseWriter, r *http.Request) {
	if !plugin.AdminOK(w, r) {
		return
	}
	sess, ok := plugin.AdminSession(w, r)
	if !ok {
		return
	}
	if allowed, retry := p.checkAdminRate(r, sess.UserID, "reject"); !allowed {
		w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())+1))
		plugin.StatusFail(w, http.StatusTooManyRequests, "操作过于频繁，请稍后再试")
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		plugin.StatusFail(w, 400, "参数错误")
		return
	}
	vals, err := parseFormValues(r)
	if err != nil {
		plugin.JSONFail(w, "请求格式错误")
		return
	}
	ctx := r.Context()
	req, err := p.requests.Get(ctx, id)
	if err != nil {
		plugin.JSONFail(w, "查询失败")
		return
	}
	if req == nil {
		plugin.StatusFail(w, 404, "申请不存在")
		return
	}
	if req.Status != statusPending {
		plugin.JSONFail(w, "该申请已处理")
		return
	}
	note := strings.TrimSpace(vals["handle_note"])
	if len([]rune(note)) > 500 {
		plugin.StatusFail(w, 400, "驳回备注过长")
		return
	}
	claimed, err := p.requests.Claim(ctx, id, statusRejected, sess.UserID, note)
	if err != nil {
		plugin.JSONFail(w, "操作失败")
		return
	}
	if !claimed {
		plugin.JSONFail(w, "该申请已被其他管理员处理")
		return
	}
	plugin.Emit(ctx, EventRefundRejected, RefundPayload{
		RequestID: req.ID, OrderID: req.OrderID, UserID: req.UserID,
		Amount: req.Amount, Status: statusRejected,
	})
	if p.cfgBool(ctx, "notifyUserOnResult", true) && p.host.Notify != nil {
		body := fmt.Sprintf("您对订单 #%d 的退款申请（%s 元）未通过审核。", req.OrderID, req.Amount)
		if note != "" {
			body += "原因：" + note
		}
		_ = p.host.Notify.Notify(ctx, req.UserID, "退款申请未通过", body)
	}
	// 多渠道机器人通知。
	rejectMsg := fmt.Sprintf("订单 #%d 的退款申请已驳回\n金额：%s 元", req.OrderID, req.Amount)
	if note != "" {
		rejectMsg += "\n原因：" + note
	}
	p.notifyChannels(ctx, "退款已驳回", rejectMsg)
	plugin.WriteJSON(w, map[string]any{"ok": 1})
}

// approveFlow 执行通过流程：原子占位 → 核心退款 → 回写退款单号 → 事件与通知。
// 返回空串表示成功；否则为失败原因（已回滚 pending，可重试）。
// 记账与通知失败不影响已执行的退款结果，但会通过 NotifyAdminOnce 告知管理员处理人复核。
func (p *Plugin) approveFlow(ctx context.Context, req *Request, admin sql.NullInt64) string {
	claimed, err := p.requests.ClaimBy(ctx, req.ID, statusApproved, admin, "")
	if err != nil {
		return "操作失败"
	}
	if !claimed {
		return "该申请已被其他管理员处理"
	}
	if p.host.Refunder == nil {
		_ = p.requests.Reopen(ctx, req.ID)
		return "退款能力未配置"
	}
	// 按商品规则计算原路退款手续费（仅 gateway 方式生效）。
	refundAmount := req.Amount
	feeNote := ""
	if req.Method == methodGateway {
		if pid, perr := p.requests.ProductIDByOrder(ctx, req.OrderID); perr == nil {
			if rule, rerr := p.requests.RuleForProduct(ctx, pid); rerr == nil && rule != nil {
				_, reqCents, _ := money.ParseNonNegative(req.Amount, 999999999999)
				_, rateBps, rerr := money.ParsePercent(rule.GatewayFeeRate)
				_, minCents, merr := money.ParseNonNegative(rule.GatewayFeeMin, 999999999999)
				if rerr == nil && merr == nil && (rateBps > 0 || minCents > 0) {
					feeCents := reqCents * rateBps / 10000
					if feeCents < minCents {
						feeCents = minCents
					}
					if feeCents >= reqCents {
						_ = p.requests.Reopen(ctx, req.ID)
						return "退款金额不足以支付手续费"
					}
					refundAmount = money.FormatCents(reqCents - feeCents)
					feeNote = fmt.Sprintf("（扣除手续费 %s 元，实退 %s 元）",
						money.FormatCents(feeCents), refundAmount)
				}
			}
		}
	}
	// Refunder.Refund 不返回退款单 ID；为了避免把"取最新ID"和"写回"之间被并发插入覆盖，
	// 将"读最新 ID + 写回"放进一个事务里（FOR UPDATE 锁住当前订单的最新 refunds 行）。
	// 即便失败，也已经完成了退款，状态保留 approved，仅通知管理员人工补 refund_id。
	refunderAdmin := admin.Int64
	if err := p.host.Refunder.Refund(ctx, refunderAdmin, req.OrderID, refundAmount,
		"用户自助退款："+req.Reason, req.Method); err != nil {
		_ = p.requests.Reopen(ctx, req.ID)
		return "退款执行失败：" + err.Error()
	}
	// 实退金额与申请金额不一致时（扣除了手续费），回写申请记录金额。
	if refundAmount != req.Amount {
		if err := p.requests.UpdateAmount(ctx, req.ID, refundAmount); err != nil {
			p.alertAdmin(ctx, req, "回写实退金额失败", err)
		}
		req.Amount = refundAmount
	}
	if _, err := p.requests.AttachRefundID(ctx, req.ID, req.OrderID); err != nil {
		p.alertAdmin(ctx, req, "回写核心退款单号失败", err)
	}
	// 退款后产品操作：按商品规则执行（暂停/终止），仅本地状态变更，不触达上游；
	// 状态变更后外发核心生命周期事件，webhook 类订阅方能感知服务被停用/删除。
	if pid, perr := p.requests.ProductIDByOrder(ctx, req.OrderID); perr == nil {
		if rule, rerr := p.requests.RuleForProduct(ctx, pid); rerr == nil && rule != nil {
			affected, aerr := p.requests.ApplyPostRefundAction(ctx, req.OrderID, rule.PostRefundAction)
			if aerr != nil {
				p.alertAdmin(ctx, req, "退款后产品操作失败", aerr)
			} else {
				evt := plugin.EventServiceSuspended
				if rule.PostRefundAction == "terminate" {
					evt = plugin.EventServiceTerminated
				}
				for _, s := range affected {
					plugin.Emit(ctx, evt, plugin.ServicePayload{
						ServiceID: s.ServiceID, UserID: s.UserID, ProductID: s.ProductID,
					})
				}
			}
		}
	}
	plugin.Emit(ctx, EventRefundApproved, RefundPayload{
		RequestID: req.ID, OrderID: req.OrderID, UserID: req.UserID,
		Amount: req.Amount, Status: statusApproved,
	})
	if p.cfgBool(ctx, "notifyUserOnResult", true) && p.host.Notify != nil {
		body := fmt.Sprintf("您对订单 #%d 的退款申请（%s 元）已通过审核并完成退款。%s",
			req.OrderID, req.Amount, feeNote)
		_ = p.host.Notify.Notify(ctx, req.UserID, "退款申请已通过", body)
	}
	// 多渠道机器人通知。
	notifyMsg := fmt.Sprintf("订单 #%d 的退款申请已通过\n金额：%s 元", req.OrderID, req.Amount)
	if feeNote != "" {
		notifyMsg += "\n" + feeNote
	}
	p.notifyChannels(ctx, "退款已通过", notifyMsg)
	return ""
}

// alertAdmin 退款执行后的"记账失败"统一通道：单条告警，按日去重，确保管理员会看到。
func (p *Plugin) alertAdmin(ctx context.Context, req *Request, subject string, cause error) {
	if p.host == nil || p.host.Notify == nil {
		return
	}
	key := fmt.Sprintf("refund_post:%s:%d:%s", subject, req.ID, time.Now().Format("2006-01-02"))
	body := fmt.Sprintf("退款申请 #%d（订单 #%d，用户 #%d，金额 %s 元）后续处理失败：%s。请人工复核。",
		req.ID, req.OrderID, req.UserID, req.Amount, cause.Error())
	if aerr := p.host.Notify.NotifyAdminOnce(ctx, key, "退款复核", subject, body); aerr != nil {
		// 此时记账已失败 + 告警又发不出去 = 双重静默，必须留日志供人工复核
		//（与 notifier.go alertChannelOnce 的失败处理保持一致）。
		log.Printf("[refund] 退款申请 #%d 复核告警发送失败（subject=%s）: %v", req.ID, subject, aerr)
	}
}

// ---- 商品退款规则管理 ----

var (
	validRefundRequirements = []string{"unlimited", "first_order", "first_order_of_product"}
	validWindowTypes        = []string{"days", "hours"}
	validRefundRules        = []string{"daily", "full"}
	validRefundTypes        = []string{"balance", "balance_gateway", "gateway_record"}
	validPostRefundActions  = []string{"none", "suspend", "terminate"}
)

func (p *Plugin) adminProductRules(w http.ResponseWriter, r *http.Request) {
	if !plugin.AdminOK(w, r) {
		return
	}
	rows, err := p.requests.ListProductRules(r.Context())
	if err != nil {
		plugin.JSONFail(w, "查询失败")
		return
	}
	list := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		list = append(list, map[string]any{
			"id":                  row.ID,
			"product_id":          row.ProductID,
			"product_name":        row.ProductName,
			"refund_requirement":  row.RefundRequirement,
			"window_type":         row.WindowType,
			"window_value":        row.WindowValue,
			"refund_rule":         row.RefundRule,
			"refund_type":         row.RefundType,
			"review_mode":         row.ReviewMode,
			"auto_approve_max":    row.AutoApproveMax,
			"gateway_fee_rate":    row.GatewayFeeRate,
			"gateway_fee_min":     row.GatewayFeeMin,
			"post_refund_action":  row.PostRefundAction,
		})
	}
	plugin.WriteJSON(w, map[string]any{"ok": 1, "list": list})
}

func (p *Plugin) adminProducts(w http.ResponseWriter, r *http.Request) {
	if !plugin.AdminOK(w, r) {
		return
	}
	rows, err := p.host.DB.QueryContext(r.Context(),
		`SELECT id, name FROM products WHERE hidden=false ORDER BY name`)
	if err != nil {
		plugin.JSONFail(w, "查询失败")
		return
	}
	defer rows.Close()
	type opt struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	}
	out := make([]opt, 0, 64)
	for rows.Next() {
		var o opt
		if err := rows.Scan(&o.ID, &o.Name); err != nil {
			plugin.JSONFail(w, "查询失败")
			return
		}
		out = append(out, o)
	}
	plugin.WriteJSON(w, map[string]any{"ok": 1, "list": out})
}

func (p *Plugin) adminSaveProductRule(w http.ResponseWriter, r *http.Request) {
	if !plugin.AdminOK(w, r) {
		return
	}
	vals, err := parseFormValues(r)
	if err != nil {
		plugin.JSONFail(w, "请求格式错误")
		return
	}
	productID, err := strconv.ParseInt(strings.TrimSpace(vals["product_id"]), 10, 64)
	if err != nil || productID < 0 {
		plugin.StatusFail(w, 400, "商品参数错误")
		return
	}
	requirement := strings.TrimSpace(vals["refund_requirement"])
	if !contains(validRefundRequirements, requirement) {
		plugin.StatusFail(w, 400, "退款要求参数错误")
		return
	}
	windowType := strings.TrimSpace(vals["window_type"])
	if !contains(validWindowTypes, windowType) {
		plugin.StatusFail(w, 400, "期限单位参数错误")
		return
	}
	windowValue, err := strconv.Atoi(strings.TrimSpace(vals["window_value"]))
	if err != nil || windowValue < 0 {
		plugin.StatusFail(w, 400, "期限数值参数错误")
		return
	}
	refundRule := strings.TrimSpace(vals["refund_rule"])
	if !contains(validRefundRules, refundRule) {
		plugin.StatusFail(w, 400, "退款规则参数错误")
		return
	}
	refundType := strings.TrimSpace(vals["refund_type"])
	if !contains(validRefundTypes, refundType) {
		plugin.StatusFail(w, 400, "退款类型参数错误")
		return
	}
	reviewMode := strings.TrimSpace(vals["review_mode"])
	if reviewMode != "manual" && reviewMode != "auto" {
		plugin.StatusFail(w, 400, "审核模式参数错误")
		return
	}
	postAction := strings.TrimSpace(vals["post_refund_action"])
	if !contains(validPostRefundActions, postAction) {
		plugin.StatusFail(w, 400, "退款后产品操作参数错误")
		return
	}
	autoMax := strings.TrimSpace(vals["auto_approve_max"])
	if autoMax == "" {
		autoMax = "0"
	}
	if !isValidDecimal(autoMax) {
		plugin.StatusFail(w, 400, "自动通过金额格式错误")
		return
	}
	feeRate := strings.TrimSpace(vals["gateway_fee_rate"])
	if feeRate == "" {
		feeRate = "0"
	}
	if !isValidDecimal(feeRate) {
		plugin.StatusFail(w, 400, "手续费比例格式错误")
		return
	}
	feeMin := strings.TrimSpace(vals["gateway_fee_min"])
	if feeMin == "" {
		feeMin = "0"
	}
	if !isValidDecimal(feeMin) {
		plugin.StatusFail(w, 400, "最低手续费格式错误")
		return
	}
	id, err := p.requests.UpsertProductRule(r.Context(), &ProductRule{
		ProductID:         productID,
		RefundRequirement: requirement,
		WindowType:        windowType,
		WindowValue:       windowValue,
		RefundRule:        refundRule,
		RefundType:        refundType,
		ReviewMode:        reviewMode,
		AutoApproveMax:    autoMax,
		GatewayFeeRate:    feeRate,
		GatewayFeeMin:     feeMin,
		PostRefundAction:  postAction,
	})
	if err != nil {
		plugin.JSONFail(w, "保存失败")
		return
	}
	plugin.WriteJSON(w, map[string]any{"ok": 1, "id": id})
}

func (p *Plugin) adminDeleteProductRule(w http.ResponseWriter, r *http.Request) {
	if !plugin.AdminOK(w, r) {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		plugin.StatusFail(w, 400, "参数错误")
		return
	}
	if err := p.requests.DeleteProductRule(r.Context(), id); err != nil {
		plugin.JSONFail(w, "删除失败")
		return
	}
	plugin.WriteJSON(w, map[string]any{"ok": 1})
}
