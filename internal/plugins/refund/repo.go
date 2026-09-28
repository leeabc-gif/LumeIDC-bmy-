package refund

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// ErrExists 同一订单已存在进行中的退款申请（部分唯一索引冲突）。
var ErrExists = errors.New("已存在进行中的退款申请")

// Requests 退款申请数据访问（表 plugin_refund_requests）。
type Requests struct{ db *sql.DB }

func NewRequests(db *sql.DB) *Requests { return &Requests{db: db} }

// Request 退款申请（字段同表）。
type Request struct {
	ID         int64
	OrderID    int64
	UserID     int64
	Amount     string
	Reason     string
	Detail     string
	Method     string
	Status     string
	HandleNote string
	HandledBy  sql.NullInt64
	HandledAt  sql.NullTime
	RefundID   sql.NullInt64
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// Row 列表行（附带用户与订单信息）。
type Row struct {
	Request
	UserName    string
	UserEmail   string
	OrderAmount string
	OrderCycle  string
	OrderPaidAt sql.NullTime
}

const requestCols = `id,order_id,user_id,amount,reason,detail,method,status,handle_note,handled_by,handled_at,refund_id,created_at,updated_at`

func scanRequest(row interface{ Scan(...any) error }) (*Request, error) {
	var q Request
	if err := row.Scan(&q.ID, &q.OrderID, &q.UserID, &q.Amount, &q.Reason, &q.Detail,
		&q.Method, &q.Status, &q.HandleNote, &q.HandledBy, &q.HandledAt, &q.RefundID,
		&q.CreatedAt, &q.UpdatedAt); err != nil {
		return nil, err
	}
	return &q, nil
}

func scanRow(row interface{ Scan(...any) error }) (*Row, error) {
	var r Row
	if err := row.Scan(&r.ID, &r.OrderID, &r.UserID, &r.Amount, &r.Reason, &r.Detail,
		&r.Method, &r.Status, &r.HandleNote, &r.HandledBy, &r.HandledAt, &r.RefundID,
		&r.CreatedAt, &r.UpdatedAt, &r.UserName, &r.UserEmail, &r.OrderAmount,
		&r.OrderCycle, &r.OrderPaidAt); err != nil {
		return nil, err
	}
	return &r, nil
}

// Create 新增申请；同订单已有进行中申请返回 ErrExists。
func (r *Requests) Create(ctx context.Context, q *Request) (int64, error) {
	var id int64
	err := r.db.QueryRowContext(ctx,
		`INSERT INTO plugin_refund_requests
		 (order_id,user_id,amount,reason,detail,method,status)
		 VALUES($1,$2,$3,$4,$5,$6,$7)
		 ON CONFLICT (order_id) WHERE status='pending' DO NOTHING
		 RETURNING id`,
		q.OrderID, q.UserID, q.Amount, q.Reason, q.Detail, q.Method, q.Status).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrExists
	}
	return id, err
}

// Claim 原子占位：仅 pending 可流转为 approved/rejected，同时写入处理人与
// 处理备注，返回是否抢到（防双审；备注随占位一次落库，避免半完成状态）。
func (r *Requests) Claim(ctx context.Context, id int64, status string, adminID int64, note string) (bool, error) {
	res, err := r.db.ExecContext(ctx,
		`UPDATE plugin_refund_requests
		 SET status=$2,handled_by=$3,handled_at=now(),handle_note=$4,updated_at=now()
		 WHERE id=$1 AND status='pending'`,
		id, status, adminID, note)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// ClaimBy 与 Claim 等价，但处理人支持 NULL（系统自动通过场景：无人值守时写 NULL 区别于 0/未知）。
func (r *Requests) ClaimBy(ctx context.Context, id int64, status string, adminID sql.NullInt64, note string) (bool, error) {
	res, err := r.db.ExecContext(ctx,
		`UPDATE plugin_refund_requests
		 SET status=$2,handled_by=$3,handled_at=now(),handle_note=$4,updated_at=now()
		 WHERE id=$1 AND status='pending'`,
		id, status, adminID, note)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// Reopen 退款执行失败回滚为 pending（保留申请痕迹可重试）。
func (r *Requests) Reopen(ctx context.Context, id int64) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE plugin_refund_requests
		 SET status='pending',handled_by=NULL,handled_at=NULL,updated_at=now() WHERE id=$1`, id)
	return err
}

// SetRefundID 回写核心退款单 ID（同订单同时仅一条 pending，退款单无歧义）。
func (r *Requests) SetRefundID(ctx context.Context, id, refundID int64) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE plugin_refund_requests SET refund_id=$2,updated_at=now() WHERE id=$1`, id, refundID)
	return err
}

// AttachRefundID 在单事务中以 FOR UPDATE 取最新 refunds.id 并写入 plugin_refund_requests，
// 消除"取最新ID → 写回"之间的并发空隙（避免错拿/覆盖其他请求的退款单）。
func (r *Requests) AttachRefundID(ctx context.Context, id, orderID int64) (int64, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	var refundID sql.NullInt64
	if err := tx.QueryRowContext(ctx,
		`SELECT id FROM refunds WHERE order_id=$1 ORDER BY id DESC FOR UPDATE`, orderID).Scan(&refundID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, err
	}
	if !refundID.Valid {
		return 0, tx.Commit()
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE plugin_refund_requests SET refund_id=$2,updated_at=now() WHERE id=$1`,
		id, refundID.Int64); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return refundID.Int64, nil
}

// UpdateAmount 回写实际退款金额（如扣除手续费后与申请金额不一致）。
func (r *Requests) UpdateAmount(ctx context.Context, id int64, amount string) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE plugin_refund_requests SET amount=$2,updated_at=now() WHERE id=$1`, id, amount)
	return err
}

// LatestRefundID 取订单最新核心退款单 ID；无则 0。
func (r *Requests) LatestRefundID(ctx context.Context, orderID int64) (int64, error) {
	var id sql.NullInt64
	err := r.db.QueryRowContext(ctx,
		`SELECT id FROM refunds WHERE order_id=$1 ORDER BY id DESC LIMIT 1`, orderID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return id.Int64, err
}

// Withdraw 用户撤回（限本人 + pending），返回是否成功。
func (r *Requests) Withdraw(ctx context.Context, id, userID int64) (bool, error) {
	res, err := r.db.ExecContext(ctx,
		`UPDATE plugin_refund_requests SET status='withdrawn',updated_at=now()
		 WHERE id=$1 AND user_id=$2 AND status='pending'`, id, userID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// Get 按 ID 取申请；不存在返回 (nil, nil)。
func (r *Requests) Get(ctx context.Context, id int64) (*Request, error) {
	q, err := scanRequest(r.db.QueryRowContext(ctx,
		`SELECT `+requestCols+` FROM plugin_refund_requests WHERE id=$1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return q, err
}

// Filter 后台列表筛选（Keyword 匹配用户邮箱/昵称、订单号、原因）。
type Filter struct {
	Keyword string
	Status  string // 空=全部
	Page    int
	Limit   int
}

const listWhere = ` WHERE ($1='' OR u.email ILIKE '%'||$1||'%' OR coalesce(nullif(u.name,''),'') ILIKE '%'||$1||'%'
	              OR CAST(q.order_id AS TEXT) = $1 OR q.reason ILIKE '%'||$1||'%')
	           AND ($2='' OR q.status=$2)`

const listCols = `q.id,q.order_id,q.user_id,q.amount,q.reason,q.detail,q.method,q.status,q.handle_note,
        q.handled_by,q.handled_at,q.refund_id,q.created_at,q.updated_at,
        coalesce(nullif(u.name,''),''), u.email, coalesce(o.amount::text,''), coalesce(o.cycle,''), o.paid_at`

// AdminList 后台分页列表（JOIN users + orders），返回行与总数。
func (r *Requests) AdminList(ctx context.Context, f Filter) ([]Row, int, error) {
	var total int
	if err := r.db.QueryRowContext(ctx,
		`SELECT count(*) FROM plugin_refund_requests q JOIN users u ON u.id=q.user_id`+listWhere,
		f.Keyword, f.Status).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+listCols+`
		   FROM plugin_refund_requests q
		   JOIN users u ON u.id=q.user_id
		   LEFT JOIN orders o ON o.id=q.order_id`+listWhere+`
		   ORDER BY q.id DESC LIMIT $3 OFFSET $4`,
		f.Keyword, f.Status, f.Limit, (f.Page-1)*f.Limit)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := make([]Row, 0, f.Limit)
	for rows.Next() {
		row, err := scanRow(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, *row)
	}
	return out, total, rows.Err()
}

// ListByUser 本人申请分页列表（JOIN orders 取金额/周期/支付时间），返回行与总数。
func (r *Requests) ListByUser(ctx context.Context, userID int64, page, pageSize int) ([]Row, int, error) {
	var total int
	if err := r.db.QueryRowContext(ctx,
		`SELECT count(*) FROM plugin_refund_requests WHERE user_id=$1`, userID).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+listCols+`
		   FROM plugin_refund_requests q
		   JOIN users u ON u.id=q.user_id
		   LEFT JOIN orders o ON o.id=q.order_id
		   WHERE q.user_id=$1
		   ORDER BY q.id DESC LIMIT $2 OFFSET $3`,
		userID, pageSize, (page-1)*pageSize)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := make([]Row, 0, pageSize)
	for rows.Next() {
		row, err := scanRow(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, *row)
	}
	return out, total, rows.Err()
}

// Stats 状态统计（待审核/已通过/已驳回）。
func (r *Requests) Stats(ctx context.Context) (pending, approved, rejected int, err error) {
	err = r.db.QueryRowContext(ctx,
		`SELECT count(*) FILTER (WHERE status='pending'),
		        count(*) FILTER (WHERE status='approved'),
		        count(*) FILTER (WHERE status='rejected')
		   FROM plugin_refund_requests`).Scan(&pending, &approved, &rejected)
	return pending, approved, rejected, err
}

// StatsByUser 本人状态统计（待审核/已通过/已驳回），供前台统计卡使用。
func (r *Requests) StatsByUser(ctx context.Context, userID int64) (pending, approved, rejected int, err error) {
	err = r.db.QueryRowContext(ctx,
		`SELECT count(*) FILTER (WHERE status='pending'),
		        count(*) FILTER (WHERE status='approved'),
		        count(*) FILTER (WHERE status='rejected')
		   FROM plugin_refund_requests WHERE user_id=$1`, userID).Scan(&pending, &approved, &rejected)
	return pending, approved, rejected, err
}

// RefundedTotal 订单已退总额（核心 refunds 表 status='done'）；无则 "0"。
func (r *Requests) RefundedTotal(ctx context.Context, orderID int64) (string, error) {
	var total string
	err := r.db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(amount::numeric),0)::text FROM refunds
		 WHERE order_id=$1 AND status='done'`, orderID).Scan(&total)
	return total, err
}

// OrderRow 可退订单行（附可退余额）。
type OrderRow struct {
	ID         int64
	ProductID  int64
	Amount     string // 订单实付
	Cycle      string
	PaidAt     sql.NullTime
	Refundable string // 可退余额 = 实付 - 已退
}

// PaidOrder 本人已支付订单摘要（金额、周期与支付时间）。
type PaidOrder struct {
	Amount string
	Cycle  string
	PaidAt sql.NullTime
}

// PaidOrderByUser 取本人已支付订单（status=1）；非本人或未支付返回 (nil, nil)。
func (r *Requests) PaidOrderByUser(ctx context.Context, orderID, userID int64) (*PaidOrder, error) {
	var o PaidOrder
	err := r.db.QueryRowContext(ctx,
		`SELECT amount::text, cycle, paid_at FROM orders WHERE id=$1 AND user_id=$2 AND status=1`,
		orderID, userID).Scan(&o.Amount, &o.Cycle, &o.PaidAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &o, err
}

// EligibleOrders 本人可退订单：已支付、无进行中申请、可退余额>0、
// 在可退期限内（windowDays<=0 不限）。
//
// limit / offset：分页参数。limit 必须 >=1，调用方负责 clamp 到合理范围
// （handler 侧已 clamp 到 [1, maxEligibleLimit]）。SQL 多取 1 条用作"还有更多"
// 探测，多余的一行会在末尾从 out 中丢弃。
func (r *Requests) EligibleOrders(ctx context.Context, userID int64, windowDays, limit, offset int) ([]OrderRow, error) {
	if limit < 1 {
		limit = 1
	}
	if offset < 0 {
		offset = 0
	}
	noWindow := windowDays <= 0
	var cutoff time.Time
	if !noWindow {
		cutoff = time.Now().Add(-time.Duration(windowDays) * 24 * time.Hour)
	}
	// 多取一行探 has_more。
	fetch := limit + 1
	rows, err := r.db.QueryContext(ctx,
		`SELECT o.id, o.product_id, o.amount::text, o.cycle, o.paid_at,
		        (o.amount - COALESCE((SELECT SUM(rf.amount::numeric) FROM refunds rf
		                              WHERE rf.order_id=o.id AND rf.status='done'),0))::text
		   FROM orders o
		 WHERE o.user_id=$1 AND o.status=1
		   AND NOT EXISTS (SELECT 1 FROM plugin_refund_requests pr
		                    WHERE pr.order_id=o.id AND pr.status='pending')
		   AND o.amount > COALESCE((SELECT SUM(rf.amount::numeric) FROM refunds rf
		                            WHERE rf.order_id=o.id AND rf.status='done'),0)
		   AND ($2 OR o.paid_at > $3)
		 ORDER BY o.id DESC LIMIT $4 OFFSET $5`, userID, noWindow, cutoff, fetch, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]OrderRow, 0, limit)
	for rows.Next() {
		var o OrderRow
		if err := rows.Scan(&o.ID, &o.ProductID, &o.Amount, &o.Cycle, &o.PaidAt, &o.Refundable); err != nil {
			return nil, err
		}
		// 探到第 limit+1 条就停，多余的留给调用方判断 has_more。
		if len(out) >= limit {
			_ = rows.Close()
			break
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// ---- 商品级退款规则 ----

// ProductRule 商品退款规则（product_id=0 为全局默认）。
type ProductRule struct {
	ID                int64
	ProductID         int64
	RefundRequirement string // unlimited|first_order|first_order_of_product
	WindowType        string // days|hours
	WindowValue       int
	RefundRule        string // daily|full
	RefundType        string // balance|balance_gateway|gateway_record
	ReviewMode        string // manual|auto
	AutoApproveMax    string // 元
	GatewayFeeRate    string // 百分比
	GatewayFeeMin     string // 元
	PostRefundAction  string // none|suspend|terminate
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// ProductRuleRow 列表行（附带商品名）。
type ProductRuleRow struct {
	ProductRule
	ProductName string
}

const productRuleCols = `pr.id,pr.product_id,pr.refund_requirement,pr.window_type,pr.window_value,pr.refund_rule,pr.refund_type,pr.review_mode,pr.auto_approve_max::text,pr.gateway_fee_rate::text,pr.gateway_fee_min::text,pr.post_refund_action,pr.created_at,pr.updated_at`

func scanProductRule(row interface{ Scan(...any) error }) (*ProductRule, error) {
	var r ProductRule
	if err := row.Scan(&r.ID, &r.ProductID, &r.RefundRequirement, &r.WindowType, &r.WindowValue,
		&r.RefundRule, &r.RefundType, &r.ReviewMode, &r.AutoApproveMax, &r.GatewayFeeRate,
		&r.GatewayFeeMin, &r.PostRefundAction, &r.CreatedAt, &r.UpdatedAt); err != nil {
		return nil, err
	}
	return &r, nil
}

// ListProductRules 规则列表（含全局默认行，按 product_id=0 排最前，其余按商品名）。
func (r *Requests) ListProductRules(ctx context.Context) ([]ProductRuleRow, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+productRuleCols+`, coalesce(p.name,'全局默认')
		   FROM plugin_refund_product_rules pr
		   LEFT JOIN products p ON p.id=pr.product_id
		   ORDER BY (pr.product_id=0) DESC, p.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]ProductRuleRow, 0, 16)
	for rows.Next() {
		var row ProductRuleRow
		if err := rows.Scan(&row.ID, &row.ProductID, &row.RefundRequirement, &row.WindowType, &row.WindowValue,
			&row.RefundRule, &row.RefundType, &row.ReviewMode, &row.AutoApproveMax, &row.GatewayFeeRate,
			&row.GatewayFeeMin, &row.PostRefundAction, &row.CreatedAt, &row.UpdatedAt, &row.ProductName); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// UpsertProductRule 按 product_id 新增或更新规则。
func (r *Requests) UpsertProductRule(ctx context.Context, rule *ProductRule) (int64, error) {
	var id int64
	err := r.db.QueryRowContext(ctx,
		`INSERT INTO plugin_refund_product_rules
		 (product_id,refund_requirement,window_type,window_value,refund_rule,refund_type,review_mode,
		  auto_approve_max,gateway_fee_rate,gateway_fee_min,post_refund_action)
		 VALUES($1,$2,$3,$4,$5,$6,$7,$8::numeric,$9::numeric,$10::numeric,$11)
		 ON CONFLICT (product_id) DO UPDATE SET
		   refund_requirement=EXCLUDED.refund_requirement,
		   window_type=EXCLUDED.window_type,
		   window_value=EXCLUDED.window_value,
		   refund_rule=EXCLUDED.refund_rule,
		   refund_type=EXCLUDED.refund_type,
		   review_mode=EXCLUDED.review_mode,
		   auto_approve_max=EXCLUDED.auto_approve_max,
		   gateway_fee_rate=EXCLUDED.gateway_fee_rate,
		   gateway_fee_min=EXCLUDED.gateway_fee_min,
		   post_refund_action=EXCLUDED.post_refund_action,
		   updated_at=now()
		 RETURNING id`,
		rule.ProductID, rule.RefundRequirement, rule.WindowType, rule.WindowValue,
		rule.RefundRule, rule.RefundType, rule.ReviewMode, rule.AutoApproveMax,
		rule.GatewayFeeRate, rule.GatewayFeeMin, rule.PostRefundAction).Scan(&id)
	return id, err
}

// DeleteProductRule 按 ID 删除规则。
func (r *Requests) DeleteProductRule(ctx context.Context, id int64) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM plugin_refund_product_rules WHERE id=$1`, id)
	return err
}

// RuleForProduct 按商品 ID 取规则：精确匹配优先，回退全局默认；均无返回 (nil, nil)。
func (r *Requests) RuleForProduct(ctx context.Context, productID int64) (*ProductRule, error) {
	row := r.db.QueryRowContext(ctx,
		`SELECT `+productRuleCols+` FROM plugin_refund_product_rules pr
		 WHERE pr.product_id IN ($1,0) ORDER BY pr.product_id=$1 DESC LIMIT 1`, productID)
	rule, err := scanProductRule(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return rule, err
}

// ProductIDByOrder 取订单的 product_id；不存在返回 0。
func (r *Requests) ProductIDByOrder(ctx context.Context, orderID int64) (int64, error) {
	var pid int64
	err := r.db.QueryRowContext(ctx,
		`SELECT product_id FROM orders WHERE id=$1`, orderID).Scan(&pid)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return pid, err
}

// IsFirstPaidOrder 是否用户首笔已支付订单（仅本单）。
func (r *Requests) IsFirstPaidOrder(ctx context.Context, orderID, userID int64) (bool, error) {
	var n int
	err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM orders WHERE user_id=$1 AND status=1 AND id<>$2`,
		userID, orderID).Scan(&n)
	if err != nil {
		return false, err
	}
	return n == 0, nil
}

// IsFirstPaidOrderOfProduct 是否用户在该商品下的首笔已支付订单。
func (r *Requests) IsFirstPaidOrderOfProduct(ctx context.Context, orderID, userID, productID int64) (bool, error) {
	var n int
	err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM orders WHERE user_id=$1 AND status=1 AND product_id=$2 AND id<>$3`,
		userID, productID, orderID).Scan(&n)
	if err != nil {
		return false, err
	}
	return n == 0, nil
}

// ServiceRef 服务简要引用（退款后事件外发用）。
type ServiceRef struct {
	ServiceID int64
	UserID    int64
	ProductID int64
}

// ApplyPostRefundAction 退款后产品操作：suspend→status=2, terminate→status=3。
// 返回实际发生状态变更的服务（调用方据此外发 service.suspended/terminated 事件）。
// 仅本地状态变更，不触达上游停机/删除。
func (r *Requests) ApplyPostRefundAction(ctx context.Context, orderID int64, action string) ([]ServiceRef, error) {
	var newStatus int
	var cond string
	switch action {
	case "suspend":
		newStatus, cond = 2, "status=1"
	case "terminate":
		newStatus, cond = 3, "status<3"
	default:
		return nil, nil
	}
	rows, err := r.db.QueryContext(ctx,
		`UPDATE services SET status=$2 WHERE order_id=$1 AND `+cond+
			` RETURNING id,user_id,product_id`, orderID, newStatus)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ServiceRef
	for rows.Next() {
		var s ServiceRef
		if err := rows.Scan(&s.ServiceID, &s.UserID, &s.ProductID); err != nil {
			return out, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
