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
	"lumeidc/internal/plugin"
	"lumeidc/internal/repo"
)

// ---- 统一注册流程（前台自助 / 后台代注册 共用） ----
//
// 修复的 P0 事故：
//  1. 后台代注册不扣款（白送域名）；
//  2. 价格由客户端 paidAmount 决定（填 0.01 元即可注册）；
//  3. 先调上游后扣款：余额不足或落库失败时域名已被注册走，形成孤儿域名/资损。
//
// 现在的顺序：服务端定价 → 短事务扣款+订单落库（立即提交，余额行锁不横跨 HTTP）
// → 事务外调上游 → 成功后落域名/操作记录；上游失败或落库失败走原子退款
// （先条件 UPDATE 认领订单再回补余额），绝不产生"钱扣了域名没有"或重复退款。

// registerResult 注册成功后的返回体。
type registerResult struct {
	DomainID    int64  `json:"domainId"`
	OperationID string `json:"operationId"`
	Status      string `json:"status"`
	AmountCents int64  `json:"amountCents"`
	Amount      string `json:"amount"`
	Msg         string `json:"msg,omitempty"`
}

// registerInput 注册入参。注意：不含任何价格字段——金额一律由服务端价目表决定。
type registerInput struct {
	Domain       string
	Years        int
	UserID       int64 // 归属用户（谁付钱）
	ContactID    int64 // 本地 contacts 表 id（<=0 时取该用户默认联系人）
	AdminID      int64 // 后台代注册时的操作者（仅用于日志/备注）
	AllowPremium bool  // 管理员确认溢价域名后置 true
	Note         string
}

// registerInternal 统一注册入口：服务端定价 + 短事务扣款 + 事务外调上游 + 失败原子退款。
// 返回 *registerResult；err 为业务错误（可直接提示给用户）。
func (p *Plugin) registerInternal(ctx context.Context, c *Client, in registerInput) (*registerResult, error) {
	domain := strings.TrimSpace(strings.ToLower(in.Domain))
	if domain == "" || !strings.Contains(domain, ".") {
		return nil, errors.New("域名格式错误")
	}
	// 年限统一：非法值一律拒绝，不再静默改写成默认年限。
	// 静默改写会让"用户选 10 年、按系统默认 1 年价扣款"这类资损事故无法被发现。
	years := in.Years
	if !yearsOK(years) {
		return nil, ErrYearsOutOfRange
	}
	if in.UserID <= 0 {
		return nil, errors.New("请指定归属用户")
	}

	// 1. 服务端定价（客户端传价一律忽略）
	amountCents, price, err := p.priceOf(ctx, domain, "register", years)
	if err != nil {
		return nil, err
	}
	if amountCents <= 0 {
		return nil, fmt.Errorf("后缀 .%s 未配置有效价格", price.TLD)
	}

	// 2. 可用性 / 溢价校验
	check, err := c.CheckOne(ctx, domain)
	if err != nil {
		return nil, errors.New("可用性查询失败: " + err.Error())
	}
	if !check.IsAvailable() {
		return nil, errors.New("域名不可注册（" + check.Result + "）")
	}
	// 溢价域名双重闸门：管理员逐单确认（in.AllowPremium）+ 插件总开关（allowPremium）。
	// 总开关默认关闭，开启后仍必须逐单确认报价，避免"上游溢价暴涨 → 按普通价卖穿成本"。
	if check.IsPremium() {
		if !p.cfgBool(ctx, "allowPremium", false) {
			return nil, errors.New("溢价域名未开启售卖（请在插件配置中开启「允许售卖溢价域名」）")
		}
		if !in.AllowPremium {
			return nil, errors.New("溢价域名需管理员确认价格后再注册")
		}
	}

	// 3. 取联系人（共享模板 user_id IS NULL 对所有人可用）
	contact, err := p.pickContact(ctx, in.UserID, in.ContactID)
	if err != nil {
		return nil, err
	}

	// 4. 本地域名重名校验（避免占用他人已注册记录）
	if _, err := p.repo.GetDomainByName(ctx, domain); err == nil {
		return nil, ErrDomainExists
	} else if err != ErrDomainNotFound {
		return nil, err
	}

	// 5. 短事务：扣款 + 订单落库后立即提交。
	//    上游调用绝不放进事务：ConsumeAmount 会对用户余额行 FOR UPDATE（见
	//    internal/repo/balance.go），事务横跨最长 30s 的上游 HTTP 会阻塞该用户
	//    所有余额操作并占死连接池连接，并发注册可耗尽连接。
	amountStr := money.FormatCents(amountCents)
	note := "域名注册 " + domain
	if in.AdminID > 0 {
		note = fmt.Sprintf("域名注册 %s（管理员 %d 代注册）", domain, in.AdminID)
	}
	tx, err := p.host.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, errors.New("事务开启失败: " + err.Error())
	}
	txRepo := p.repo.WithTx(tx)

	bal := repo.NewBalance(p.host.DB)
	if err := bal.ConsumeAmount(ctx, tx, in.UserID, amountStr, note); err != nil {
		_ = tx.Rollback()
		if errors.Is(err, repo.ErrInsufficientBalance) {
			return nil, errors.New("余额不足，请先充值")
		}
		return nil, errors.New("余额扣减失败: " + err.Error())
	}

	// 订单轨迹（对账用）：domain_id 先空，上游受理后再回填。
	order := &OrderRow{
		Domain:      domain,
		UserID:      in.UserID,
		Kind:        "register",
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

	// 6. 上游注册（事务外）：失败 → 原子退款（先条件 UPDATE 认领订单，再回补余额）
	opID, err := c.RegisterDomain(ctx, domain, RegisterOpts{
		Years:             years,
		AutoRenew:         false,
		PrivacyLevel:      p.cfg(ctx, "privacyLevel", defPrivacyLevel),
		UserConsent:       true,
		ContactRegistrant: contact.ContactID,
	})
	if err != nil {
		p.refundOrderPaid(ctx, order, "register")
		return nil, errors.New("Spaceship 注册失败: " + err.Error())
	}

	// 7. 域名/操作记录落库；失败则退款（钱不能因本地故障而悬挂），上游已受理须留痕对账
	tx2, err := p.host.DB.BeginTx(ctx, nil)
	if err != nil {
		p.refundOrderPaid(ctx, order, "register")
		p.reportOrphanUpstream(ctx, order, opID, "register", "事务开启失败")
		return nil, errors.New("事务开启失败（已退款）: " + err.Error())
	}
	tx2Repo := p.repo.WithTx(tx2)
	did, err := tx2Repo.CreateDomain(ctx, &DomainRow{
		UserID:          in.UserID,
		Domain:          domain,
		WhoisID:         sql.NullInt64{Int64: contact.ID, Valid: true},
		ContactID:       sql.NullString{String: contact.ContactID, Valid: true},
		Years:           years,
		PaidAmountCents: amountCents,
		Status:          "pending",
		PrivacyLevel:    p.cfg(ctx, "privacyLevel", defPrivacyLevel),
	})
	if err != nil {
		_ = tx2.Rollback()
		p.refundOrderPaid(ctx, order, "register")
		p.reportOrphanUpstream(ctx, order, opID, "register", err.Error())
		return nil, errors.New("域名落库失败（已退款）: " + err.Error())
	}

	if _, err := tx2Repo.CreateOperation(ctx, &OperationRow{
		OperationID: opID,
		DomainID:    sql.NullInt64{Int64: did, Valid: true},
		Domain:      domain,
		OpType:      "domain_create",
		Status:      "pending",
		StartedAt:   time.Now(),
	}); err != nil {
		_ = tx2.Rollback()
		p.refundOrderPaid(ctx, order, "register")
		p.reportOrphanUpstream(ctx, order, opID, "register", err.Error())
		return nil, errors.New("操作记录落库失败（已退款）: " + err.Error())
	}

	if err := tx2Repo.UpdateOrderDomain(ctx, order.ID, did); err != nil {
		_ = tx2.Rollback()
		p.refundOrderPaid(ctx, order, "register")
		p.reportOrphanUpstream(ctx, order, opID, "register", err.Error())
		return nil, errors.New("订单关联失败（已退款）: " + err.Error())
	}

	if err := tx2.Commit(); err != nil {
		p.refundOrderPaid(ctx, order, "register")
		p.reportOrphanUpstream(ctx, order, opID, "register", "事务提交失败")
		return nil, errors.New("事务提交失败（已退款）: " + err.Error())
	}
	return &registerResult{
		DomainID:    did,
		OperationID: opID,
		Status:      "pending",
		AmountCents: amountCents,
		Amount:      amountStr,
		Msg:         "注册请求已提交，等待 Spaceship 处理（通常 1-5 分钟）",
	}, nil
}

// pickContact 选取注册用联系人：指定 id 优先，否则取该用户默认联系人。
// 共享模板（user_id IS NULL）允许所有人引用——它是管理员下发的公共模板，
// 但删除/设为默认等"写"操作仅限其所有者（见 handlers 中的归属校验）。
func (p *Plugin) pickContact(ctx context.Context, userID, contactID int64) (*ContactRow, error) {
	if contactID > 0 {
		cnt, err := p.repo.GetContact(ctx, contactID)
		if err != nil {
			if errors.Is(err, ErrContactNotFound) {
				return nil, errors.New("联系人不存在")
			}
			return nil, err
		}
		if cnt.UserID.Valid && cnt.UserID.Int64 != userID {
			return nil, errors.New("无权限使用该联系人")
		}
		return cnt, nil
	}
	cnt, err := p.repo.GetDefaultContact(ctx, sql.NullInt64{Int64: userID, Valid: true})
	if err != nil {
		return nil, errors.New("请先创建或选择一个联系人")
	}
	return cnt, nil
}

// writeRegisterResult 输出注册结果（handler 复用）。
func writeRegisterResult(w http.ResponseWriter, res *registerResult) {
	writeOK(w, map[string]any{
		"domainId":    res.DomainID,
		"operationId": res.OperationID,
		"status":      res.Status,
		"amountCents": res.AmountCents,
		"amount":      res.Amount,
		"msg":         res.Msg,
	})
}

// failFromErr 把业务错误转成统一响应：余额不足用 402，其余沿用 200+ok:0（前端既有约定）。
func failFromErr(w http.ResponseWriter, err error) {
	if err == nil {
		return
	}
	if errors.Is(err, ErrPriceNotFound) || errors.Is(err, ErrTLDDisabled) ||
		errors.Is(err, ErrYearsOutOfRange) || errors.Is(err, ErrDomainExists) {
		plugin.StatusFail(w, http.StatusBadRequest, err.Error())
		return
	}
	if errors.Is(err, repo.ErrInsufficientBalance) ||
		strings.Contains(err.Error(), "余额不足") {
		plugin.StatusFail(w, http.StatusPaymentRequired, err.Error())
		return
	}
	plugin.JSONFail(w, err.Error())
}
