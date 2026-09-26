package spaceship

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"lumeidc/internal/money"
	"lumeidc/internal/repo"
)

// ---- 续费（官方 POST /v1/domains/{domain}/renew） ----
//
// 官方要求：years（1-10）+ currentExpirationDate（当前到期时间，string <date-time>）。
// 续费与注册共用同一套资金规则：服务端定价 → 事务内先扣款 → 后调上游 → 失败回滚。

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

	// 3. 事务：扣款 → 调上游 → 落库 → 提交
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

	opID, err := c.RenewDomain(ctx, d.Domain, years, expiry)
	if err != nil {
		_ = tx.Rollback()
		return nil, errors.New("Spaceship 续费失败: " + err.Error())
	}

	if _, err := txRepo.CreateOrder(ctx, &OrderRow{
		DomainID:    sql.NullInt64{Int64: d.ID, Valid: true},
		Domain:      d.Domain,
		UserID:      d.UserID,
		Kind:        "renew",
		Years:       years,
		AmountCents: amountCents,
		Status:      "paid",
		Note:        sql.NullString{String: note, Valid: true},
	}); err != nil {
		_ = tx.Rollback()
		return nil, errors.New("订单落库失败: " + err.Error())
	}

	if _, err := txRepo.CreateOperation(ctx, &OperationRow{
		OperationID: opID,
		DomainID:    sql.NullInt64{Int64: d.ID, Valid: true},
		Domain:      d.Domain,
		OpType:      "domain_renew",
		Status:      "pending",
		StartedAt:   time.Now(),
	}); err != nil {
		_ = tx.Rollback()
		return nil, errors.New("操作记录落库失败: " + err.Error())
	}

	if err := tx.Commit(); err != nil {
		return nil, errors.New("事务提交失败: " + err.Error())
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

// refundByDomainOperation 注册/续费异步操作失败时，把已扣的钱退回用户账上。
// 幂等：同一订单只退一次（靠订单 status 判定）。
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
	amount := money.FormatCents(o.AmountCents)
	note := fmt.Sprintf("%s失败自动退款 %s", map[string]string{"register": "域名注册", "renew": "域名续费"}[kind], o.Domain)
	bal := repo.NewBalance(p.host.DB)
	if err := bal.AdminAdjust(ctx, o.UserID, amount, note); err != nil {
		return
	}
	_ = p.repo.MarkOrderRefunded(ctx, o.ID)
	if p.cfgBool(ctx, "enableNotify", true) && p.host.Notify != nil {
		_ = p.host.Notify.Notify(ctx, o.UserID, kindTitle(kind)+"失败已退款",
			fmt.Sprintf("域名 %s %s失败，已自动退回 %s 元", o.Domain, kindTitle(kind), amount))
	}
}

func kindTitle(kind string) string {
	if strings.TrimSpace(kind) == "renew" {
		return "续费"
	}
	return "注册"
}
