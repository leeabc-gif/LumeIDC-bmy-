package spaceship

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"lumeidc/internal/money"
	"lumeidc/internal/repo"
)

// ---- 续费（官方 POST /v1/domains/{domain}/renew） ----
//
// 官方要求：years（1-10）+ currentExpirationDate（当前到期时间，string <date-time>）。
// 续费与注册共用同一套资金规则：服务端定价 → 短事务扣款 → 事务外调上游 → 失败原子退款。

// renewResult 续费成功后的返回体。
type renewResult struct {
	DomainID    int64  `json:"domainId"`
	OperationID string `json:"operationId"`
	Status      string `json:"status"`
	AmountCents int64  `json:"amountCents"`
	Amount      string `json:"amount"`
	Msg         string `json:"msg,omitempty"`
}

// renewInput 续费入参（同样不含价格字段）。
type renewInput struct {
	DomainID int64 // 本地 domains 表 id
	Years    int
	ActorID  int64 // 操作者：前台为本人 userID，后台为管理员 id
	IsAdmin  bool  // 后台代续费
	Note     string
}

// renewInternal 统一续费入口（前台自助 / 后台代续费共用）。
// 归属用户＝域名所有人；后台代续费同样从该用户余额扣款。
func (p *Plugin) renewInternal(ctx context.Context, c *Client, in renewInput) (*renewResult, error) {
	if in.DomainID <= 0 {
		return nil, errors.New("参数错误")
	}
	d, err := p.repo.GetDomain(ctx, in.DomainID)
	if err != nil {
		if errors.Is(err, ErrDomainNotFound) {
			return nil, ErrDomainNotFound
		}
		return nil, err
	}
	if !in.IsAdmin && in.ActorID > 0 && d.UserID != in.ActorID {
		return nil, ErrDomainNotFound
	}
	if d.Status != "active" {
		return nil, errors.New("仅 active（已注册成功）的域名可续费，当前状态：" + d.Status)
	}

	// 0. 同域防重：已有 pending 续费操作时前置拒绝（硬保证见迁移 003 的部分唯一索引）。
	//    否则并发续费一败一成时，败者按 LatestPaidOrder 认领可能退掉成功那笔的订单。
	pending, err := p.repo.HasPendingOp(ctx, d.ID, "domain_renew")
	if err != nil {
		return nil, errors.New("续费状态查询失败: " + err.Error())
	}
	if pending {
		return nil, errors.New("该域名已有续费请求处理中，请等待处理完成后再试")
	}

	years := in.Years
	if !yearsOK(years) {
		return nil, ErrYearsOutOfRange
	}

	// 1. 服务端定价（续费价目表，客户端传价一律忽略）
	amountCents, _, err := p.priceOf(ctx, d.Domain, "renew", years)
	if err != nil {
		return nil, err
	}
	if amountCents <= 0 {
		return nil, errors.New("该后缀未配置续费价格，请联系管理员")
	}

	// 2. 官方必填的当前到期时间：本地为准，缺失时向上游查询。
	//    注意官方文档要求 currentExpirationDate 为 string <date-time>，不能传毫秒整数。
	var expiry time.Time
	if d.ExpiresAt.Valid {
		expiry = d.ExpiresAt.Time
	} else {
		info, err := c.GetDomain(ctx, d.Domain)
		if err != nil {
			return nil, errors.New("查询域名到期时间失败: " + err.Error())
		}
		if t, e := time.Parse(time.RFC3339, info.ExpiresAt); e == nil {
			expiry = t
		}
	}
	if expiry.IsZero() {
		return nil, errors.New("域名到期时间未知，无法续费（请等待注册结果回补后重试）")
	}

	amountStr := money.FormatCents(amountCents)
	note := in.Note
	if note == "" {
		note = fmt.Sprintf("域名续费 %s %d 年", d.Domain, years)
	}

	// 3. 短事务：扣款 + 订单落库后立即提交。
	//    上游调用绝不放进事务：ConsumeAmount 会对用户余额行 FOR UPDATE，
	//    事务横跨最长 30s 的上游 HTTP 会阻塞该用户所有余额操作并占死连接。
	tx, err := p.host.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, errors.New("事务开启失败: " + err.Error())
	}
	txRepo := p.repo.WithTx(tx)

	bal := repo.NewBalance(p.host.DB)
	if err := bal.ConsumeAmount(ctx, tx, d.UserID, amountStr, note); err != nil {
		_ = tx.Rollback()
		if errors.Is(err, repo.ErrInsufficientBalance) {
			return nil, errors.New("余额不足，请先充值")
		}
		return nil, errors.New("余额扣减失败: " + err.Error())
	}

	order := &OrderRow{
		DomainID:    sql.NullInt64{Int64: d.ID, Valid: true},
		Domain:      d.Domain,
		UserID:      d.UserID,
		Kind:        "renew",
		Years:       years,
		AmountCents: amountCents,
		Status:      "paid",
		Note:        sql.NullString{String: note, Valid: true},
	}
	orderID, err := txRepo.CreateOrder(ctx, order)
	if err != nil {
		_ = tx.Rollback()
		return nil, errors.New("订单落库失败: " + err.Error())
	}
	order.ID = orderID
	if err := tx.Commit(); err != nil {
		return nil, errors.New("事务提交失败: " + err.Error())
	}

	// 钱已扣：此后所有路径（上游调用/退款/落库）必须脱离请求 ctx——
	// 客户端断开会取消 r.Context()，退款若随 ctx 失败，订单将滞留 paid
	// 且无 operation 行供 cron 补救，形成资金悬挂。
	ctx = context.WithoutCancel(ctx)

	// 4. 上游续费（事务外）：失败 → 原子退款（先条件 UPDATE 认领订单，再回补余额）
	opID, err := c.RenewDomain(ctx, d.Domain, years, expiry)
	if err != nil {
		p.refundOrderPaid(ctx, order, "renew")
		return nil, errors.New("Spaceship 续费失败: " + err.Error())
	}

	// 5. 操作记录落库；失败则退款（钱不能因本地故障而悬挂），上游已受理须留痕对账
	tx2, err := p.host.DB.BeginTx(ctx, nil)
	if err != nil {
		p.refundOrderPaid(ctx, order, "renew")
		p.reportOrphanUpstream(ctx, order, opID, "renew", "事务开启失败")
		return nil, errors.New("事务开启失败: " + err.Error())
	}
	if _, err := txRepo.WithTx(tx2).CreateOperation(ctx, &OperationRow{
		OperationID: opID,
		DomainID:    sql.NullInt64{Int64: d.ID, Valid: true},
		Domain:      d.Domain,
		OpType:      "domain_renew",
		Status:      "pending",
		StartedAt:   time.Now(),
	}); err != nil {
		_ = tx2.Rollback()
		p.refundOrderPaid(ctx, order, "renew")
		p.reportOrphanUpstream(ctx, order, opID, "renew", err.Error())
		return nil, errors.New("操作记录落库失败（已退款）: " + err.Error())
	}
	if err := tx2.Commit(); err != nil {
		p.refundOrderPaid(ctx, order, "renew")
		p.reportOrphanUpstream(ctx, order, opID, "renew", "事务提交失败")
		return nil, errors.New("事务提交失败（已退款）: " + err.Error())
	}
	return &renewResult{
		DomainID:    d.ID,
		OperationID: opID,
		Status:      "pending",
		AmountCents: amountCents,
		Amount:      amountStr,
		Msg:         "续费请求已提交，等待 Spaceship 处理",
	}, nil
}

// writeRenewResult 输出续费结果。
func writeRenewResult(w http.ResponseWriter, res *renewResult) {
	writeOK(w, map[string]any{
		"domainId":    res.DomainID,
		"operationId": res.OperationID,
		"status":      res.Status,
		"amountCents": res.AmountCents,
		"amount":      res.Amount,
		"msg":         res.Msg,
	})
}

// ---- 失败自动退款 ----

// refundOrderPaid 原子退款：先用条件 UPDATE（WHERE status='paid'）认领订单，
// 认领成功（RowsAffected==1）才回补余额。并发/重复调用只有一个赢家，
// 杜绝 cron 与管理员手动重试同时走失败分支导致的双倍退款。
// 认领成功但加余额失败时打日志留痕（订单已标 refunded，需管理员人工补退）。
func (p *Plugin) refundOrderPaid(ctx context.Context, o *OrderRow, kind string) {
	claimed, err := p.repo.RefundOrderIfPaid(ctx, o.ID)
	if err != nil {
		log.Printf("[spaceship] 退款认领订单 %d 失败: %v", o.ID, err)
		return
	}
	if !claimed {
		return
	}
	amount := money.FormatCents(o.AmountCents)
	note := fmt.Sprintf("%s失败自动退款 %s", map[string]string{"register": "域名注册", "renew": "域名续费"}[kind], o.Domain)
	bal := repo.NewBalance(p.host.DB)
	if err := bal.AdminAdjust(ctx, o.UserID, amount, note); err != nil {
		// 订单已标 refunded 但余额未回补：留日志 + 告警管理员人工补退（按订单+日去重）。
		log.Printf("[spaceship] 订单 %d 已标记退款但余额回补失败（需人工对账）: %v", o.ID, err)
		if p.cfgBool(ctx, "enableNotify", true) && p.host.Notify != nil {
			alertKey := fmt.Sprintf("spaceship.refund.adjust:%d:%s", o.ID, time.Now().Format("2006-01-02"))
			_ = p.host.Notify.NotifyAdminOnce(ctx, alertKey, alertCategorySpaceshipPoll,
				"自动退款回补失败",
				fmt.Sprintf("订单 %d（%s，用户 %d，%s 元）已标记退款但余额回补失败: %v。请人工补退。",
					o.ID, o.Domain, o.UserID, amount, err))
		}
		return
	}
	if p.cfgBool(ctx, "enableNotify", true) && p.host.Notify != nil {
		_ = p.host.Notify.Notify(ctx, o.UserID, kindTitle(kind)+"失败已退款",
			fmt.Sprintf("域名 %s %s失败，已自动退回 %s 元", o.Domain, kindTitle(kind), amount))
	}
}

// refundByDomainOperation 注册/续费异步操作失败时，把已扣的钱退回用户账上。
// 幂等性由 RefundOrderIfPaid 的条件 UPDATE 保证：同一订单只可能被认领一次。
func (p *Plugin) refundByDomainOperation(ctx context.Context, op *OperationRow) {
	if !op.DomainID.Valid {
		return
	}
	kind := "register"
	if op.OpType == "domain_renew" {
		kind = "renew"
	}
	o, err := p.repo.LatestPaidOrder(ctx, op.DomainID.Int64, kind)
	if err != nil || o == nil || o.AmountCents <= 0 {
		return
	}
	p.refundOrderPaid(ctx, o, kind)
}

// reportOrphanUpstream 上游已受理但本地落库失败且已退款：把 opID 写入订单备注留痕，
// 并按操作+日去重告警管理员对账（否则上游异步结果彻底失联）。
func (p *Plugin) reportOrphanUpstream(ctx context.Context, o *OrderRow, opID, kind, cause string) {
	if o == nil || o.ID <= 0 || opID == "" {
		return
	}
	kindName := map[string]string{"register": "域名注册", "renew": "域名续费"}[kind]
	_ = p.repo.SetOrderNote(ctx, o.ID,
		fmt.Sprintf("%s失败自动退款 %s（上游已受理 opID=%s，本地落库失败: %s，需对账）", kindName, o.Domain, opID, cause))
	if p.cfgBool(ctx, "enableNotify", true) && p.host.Notify != nil {
		alertKey := fmt.Sprintf("spaceship.orphan:%s:%s", opID, time.Now().Format("2006-01-02"))
		_ = p.host.Notify.NotifyAdminOnce(ctx, alertKey, alertCategorySpaceshipPoll,
			"上游已受理但本地落库失败",
			fmt.Sprintf("域名 %s %s上游已受理（操作 %s）但本地落库失败（%s），款项已自动退款，请对账上游状态。",
				o.Domain, kindName, opID, cause))
	}
}

func kindTitle(kind string) string {
	if strings.TrimSpace(kind) == "renew" {
		return "续费"
	}
	return "注册"
}
