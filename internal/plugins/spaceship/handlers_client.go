package spaceship

import (
	"database/sql"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"lumeidc/internal/money"
	"lumeidc/internal/plugin"
)

// ---- 域名查询 ----

func (p *Plugin) clientCheck(w http.ResponseWriter, r *http.Request) {
	userID, ok := plugin.RequireUserID(w, r)
	if !ok {
		return
	}
	_ = userID
	c := p.cfgClient(r.Context())
	if c == nil {
		plugin.JSONFail(w, "Spaceship API Key/Secret 未配置")
		return
	}
	var body struct {
		Domains []string `json:"domains"`
		Years   int      `json:"years"`
	}
	if err := readJSON(r, &body); err != nil || len(body.Domains) == 0 {
		plugin.StatusFail(w, http.StatusBadRequest, "请输入要查询的域名")
		return
	}
	if body.Years <= 0 {
		body.Years = 1
	}
	// 标准化
	for i, d := range body.Domains {
		body.Domains[i] = strings.TrimSpace(strings.ToLower(d))
	}
	// 去重
	seen := map[string]bool{}
	uniq := make([]string, 0, len(body.Domains))
	for _, d := range body.Domains {
		if d == "" || seen[d] || !strings.Contains(d, ".") {
			continue
		}
		seen[d] = true
		uniq = append(uniq, d)
	}
	if len(uniq) == 0 {
		plugin.StatusFail(w, http.StatusBadRequest, "域名格式错误")
		return
	}
	// 批量查（≤20）
	var results map[string]*CheckResult
	if len(uniq) == 1 {
		res, err := c.CheckOne(r.Context(), uniq[0])
		if err != nil {
			plugin.JSONFail(w, "查询失败: "+err.Error())
			return
		}
		results = map[string]*CheckResult{uniq[0]: res}
	} else {
		var err error
		results, err = c.CheckBatch(r.Context(), uniq)
		if err != nil {
			plugin.JSONFail(w, "查询失败: "+err.Error())
			return
		}
	}
	// 组装输出（价格一律来自服务端价目表，客户端传价无效）
	type CheckItem struct {
		Domain         string  `json:"domain"`
		Result         string  `json:"result"`         // available/taken/processing
		Ok             bool    `json:"ok"`             // 快速可用判断
		Premium        bool    `json:"premium"`        // 溢价标记
		Price          float64 `json:"price"`          // 溢价时的价格（溢价提示用）
		ListPrice      string  `json:"listPrice"`      // 服务端注册报价（元，按所选年限）
		ListPriceCents int64   `json:"listPriceCents"` // 服务端注册报价（分）
		Sellable       bool    `json:"sellable"`       // 该后缀已上架且已配置价格
	}
	out := make([]CheckItem, 0, len(uniq))
	for _, d := range uniq {
		res := results[d]
		item := CheckItem{Domain: d}
		if res == nil {
			item.Result = "unknown"
			item.Ok = false
		} else {
			item.Result = res.Result
			item.Ok = res.IsAvailable()
			if res.IsPremium() {
				item.Premium = true
				if res.PremiumPricing != nil {
					item.Price = res.PremiumPricing.PremiumPrice
					if item.Price <= 0 {
						item.Price = res.PremiumPricing.Price
					}
				}
			}
		}
		if cents, _, err := p.priceOf(r.Context(), d, "register", body.Years); err == nil && cents > 0 {
			item.ListPriceCents = cents
			item.ListPrice = money.FormatCents(cents)
			item.Sellable = true
		}
		out = append(out, item)
	}
	writeOK(w, map[string]any{"list": out})
}

// ---- 前台自助注册 ----

func (p *Plugin) clientRegister(w http.ResponseWriter, r *http.Request) {
	userID, ok := plugin.RequireUserID(w, r)
	if !ok {
		return
	}
	c := p.cfgClient(r.Context())
	if c == nil {
		plugin.JSONFail(w, "Spaceship API Key/Secret 未配置")
		return
	}
	var body struct {
		Domain    string `json:"domain"`
		Years     int    `json:"years"`
		ContactID int64  `json:"contactId"` // 本地联系人 id（可选，空则用默认）
		// paidAmount 已废弃：金额一律由后台价目表决定，客户端传价不再生效（P0 资损修复）。
		PaidAmount string `json:"paidAmount"`
	}
	if err := readJSON(r, &body); err != nil {
		plugin.StatusFail(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	res, err := p.registerInternal(r.Context(), c, registerInput{
		Domain:    body.Domain,
		Years:     body.Years,
		UserID:    userID,
		ContactID: body.ContactID,
		// 溢价域名只能由管理员在后台确认价格后代注册（前台自助一律拒绝，避免按普通价卖溢价域名）。
	})
	if err != nil {
		failFromErr(w, err)
		return
	}
	writeRegisterResult(w, res)
}

// ---- 我的域名 ----

func (p *Plugin) clientMyDomains(w http.ResponseWriter, r *http.Request) {
	userID, ok := plugin.RequireUserID(w, r)
	if !ok {
		return
	}
	rows, err := p.repo.ListDomains(r.Context(), sql.NullInt64{Int64: userID, Valid: true})
	if err != nil {
		plugin.StatusFail(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]map[string]any, len(rows))
	for i, d := range rows {
		item := map[string]any{
			"id":              d.ID,
			"domain":          d.Domain,
			"years":           d.Years,
			"status":          d.Status,
			"privacyLevel":    d.PrivacyLevel,
			"autoRenew":       d.AutoRenew,
			"paidAmountCents": d.PaidAmountCents,
			"paidAmountText":  money.FormatCents(d.PaidAmountCents),
		}
		if d.RegisteredAt.Valid {
			item["registeredAt"] = d.RegisteredAt.Time.Format("2006-01-02")
		}
		if d.ExpiresAt.Valid {
			item["expiresAt"] = d.ExpiresAt.Time.Format("2006-01-02")
		}
		out[i] = item
	}
	writeOK(w, map[string]any{"list": out})
}

func (p *Plugin) clientRenew(w http.ResponseWriter, r *http.Request) {
	userID, ok := plugin.RequireUserID(w, r)
	if !ok {
		return
	}
	id, err := readID(r, "id")
	if err != nil {
		plugin.StatusFail(w, http.StatusBadRequest, "参数错误")
		return
	}
	d, err := p.repo.GetDomain(r.Context(), id)
	if err != nil || d.UserID != userID {
		plugin.StatusFail(w, http.StatusNotFound, "域名不存在")
		return
	}
	c := p.cfgClient(r.Context())
	if c == nil {
		plugin.JSONFail(w, "Spaceship API Key/Secret 未配置")
		return
	}
	var body struct {
		Years int `json:"years"`
	}
	_ = readJSON(r, &body)
	res, err := p.renewInternal(r.Context(), c, renewInput{
		DomainID: id,
		Years:    body.Years,
		ActorID:  userID,
	})
	if err != nil {
		failFromErr(w, err)
		return
	}
	writeRenewResult(w, res)
}

func (p *Plugin) clientAutoRenew(w http.ResponseWriter, r *http.Request) {
	userID, ok := plugin.RequireUserID(w, r)
	if !ok {
		return
	}
	id, err := readID(r, "id")
	if err != nil {
		plugin.StatusFail(w, http.StatusBadRequest, "参数错误")
		return
	}
	d, err := p.repo.GetDomain(r.Context(), id)
	if err != nil || d.UserID != userID {
		plugin.StatusFail(w, http.StatusNotFound, "域名不存在")
		return
	}
	var body struct {
		Enable bool `json:"enable"`
	}
	if err := readJSON(r, &body); err != nil {
		plugin.StatusFail(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	if d.Status != "active" {
		plugin.JSONFail(w, "仅 active 状态的域名可切换自动续费")
		return
	}
	if d.SpaceshipDomainID.Valid {
		c := p.cfgClient(r.Context())
		if c != nil {
			if err := c.SetAutoRenew(r.Context(), d.Domain, body.Enable); err != nil {
				plugin.JSONFail(w, "Spaceship 设置失败: "+err.Error())
				return
			}
		}
	}
	if err := p.repo.UpdateDomain(r.Context(), id, map[string]any{"auto_renew": body.Enable}); err != nil {
		plugin.StatusFail(w, http.StatusInternalServerError, "本地保存失败: "+err.Error())
		return
	}
	writeOKMsg(w, "已更新")
}

// ---- 我的联系人 ----

func (p *Plugin) clientMyContacts(w http.ResponseWriter, r *http.Request) {
	userID, ok := plugin.RequireUserID(w, r)
	if !ok {
		return
	}
	rows, err := p.repo.ListContacts(r.Context(), sql.NullInt64{Int64: userID, Valid: true})
	if err != nil {
		plugin.StatusFail(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]map[string]any, len(rows))
	for i, c := range rows {
		name := c.FirstName + " " + c.LastName
		name = strings.TrimSpace(name)
		out[i] = map[string]any{
			"id":        c.ID,
			"contactId": c.ContactID,
			"label":     c.Label,
			"name":      name,
			"email":     c.Email,
			"country":   c.Country,
			"phone":     c.Phone,
			"isDefault": c.IsDefault,
			"shared":    !c.UserID.Valid, // 管理员共享模板：只读，不可删除/设默认/编辑
			// 编辑回填用：共享模板只读，用户自建的才允许编辑
			"firstName":     c.FirstName,
			"lastName":      c.LastName,
			"organization":  c.Organization.String,
			"address1":      c.Address1,
			"address2":      c.Address2.String,
			"city":          c.City,
			"stateProvince": c.StateProvince.String,
			"postalCode":    c.PostalCode,
		}
	}
	writeOK(w, map[string]any{"list": out})
}

func (p *Plugin) clientSaveContact(w http.ResponseWriter, r *http.Request) {
	userID, ok := plugin.RequireUserID(w, r)
	if !ok {
		return
	}
	c := p.cfgClient(r.Context())
	if c == nil {
		plugin.JSONFail(w, "Spaceship API Key/Secret 未配置")
		return
	}
	var body struct {
		FirstName     string `json:"firstName"`
		LastName      string `json:"lastName"`
		Organization  string `json:"organization"`
		Email         string `json:"email"`
		Address1      string `json:"address1"`
		Address2      string `json:"address2"`
		City          string `json:"city"`
		StateProvince string `json:"stateProvince"`
		PostalCode    string `json:"postalCode"`
		Country       string `json:"country"`
		Phone         string `json:"phone"`
		Label         string `json:"label"`
		IsDefault     bool   `json:"isDefault"`
		ExistingID    int64  `json:"existingId"` // 编辑已有联系人时的本地 id（0=新建）
	}
	if err := readJSON(r, &body); err != nil {
		plugin.StatusFail(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	if body.FirstName == "" || body.LastName == "" || body.Email == "" ||
		body.Address1 == "" || body.City == "" || body.Country == "" || body.Phone == "" {
		plugin.JSONFail(w, "姓名/邮箱/地址/城市/国家/电话 必填")
		return
	}
	sc := &Contact{
		FirstName:     body.FirstName,
		LastName:      body.LastName,
		Organization:  body.Organization,
		Email:         body.Email,
		Address1:      body.Address1,
		Address2:      body.Address2,
		City:          body.City,
		StateProvince: body.StateProvince,
		PostalCode:    body.PostalCode,
		Country:       body.Country,
		Phone:         body.Phone,
	}
	// 编辑既有联系人：必须是自己的（共享模板只读），并带上原 contactId 让上游走更新。
	if body.ExistingID > 0 {
		old, err := p.repo.GetContact(r.Context(), body.ExistingID)
		if err != nil {
			if err == ErrContactNotFound {
				plugin.StatusFail(w, http.StatusNotFound, "联系人不存在")
				return
			}
			plugin.StatusFail(w, http.StatusInternalServerError, err.Error())
			return
		}
		if !old.UserID.Valid || old.UserID.Int64 != userID {
			plugin.StatusFail(w, http.StatusForbidden, "无权限")
			return
		}
		sc.ContactID = old.ContactID
	}
	contactID, err := c.SaveContact(r.Context(), sc)
	if err != nil {
		plugin.JSONFail(w, "Spaceship 保存联系人失败: "+err.Error())
		return
	}
	row := &ContactRow{
		ContactID:     contactID,
		UserID:        sql.NullInt64{Int64: userID, Valid: true},
		Label:         body.Label,
		FirstName:     body.FirstName,
		LastName:      body.LastName,
		Organization:  sql.NullString{String: body.Organization, Valid: body.Organization != ""},
		Email:         body.Email,
		Address1:      body.Address1,
		Address2:      sql.NullString{String: body.Address2, Valid: body.Address2 != ""},
		City:          body.City,
		StateProvince: sql.NullString{String: body.StateProvince, Valid: body.StateProvince != ""},
		PostalCode:    sql.NullString{String: body.PostalCode, Valid: body.PostalCode != ""},
		Country:       body.Country,
		Phone:         body.Phone,
		IsDefault:     body.IsDefault,
	}
	saved, err := p.repo.SaveContact(r.Context(), row)
	if err != nil {
		plugin.StatusFail(w, http.StatusInternalServerError, "本地保存失败: "+err.Error())
		return
	}
	if body.IsDefault {
		_ = p.repo.SetDefaultContact(r.Context(), sql.NullInt64{Int64: userID, Valid: true}, saved.ID)
	}
	writeOK(w, map[string]any{"id": saved.ID, "contactId": contactID})
}

func (p *Plugin) clientDeleteContact(w http.ResponseWriter, r *http.Request) {
	userID, ok := plugin.RequireUserID(w, r)
	if !ok {
		return
	}
	id, err := readID(r, "id")
	if err != nil {
		plugin.StatusFail(w, http.StatusBadRequest, "参数错误")
		return
	}
	cnt, err := p.repo.GetContact(r.Context(), id)
	if err != nil {
		if err == ErrContactNotFound {
			plugin.StatusFail(w, http.StatusNotFound, "联系人不存在")
			return
		}
		plugin.StatusFail(w, http.StatusInternalServerError, err.Error())
		return
	}
	// 共享模板（user_id IS NULL）对所有用户只读：既不能删，也不能占为己有。
	if !cnt.UserID.Valid || cnt.UserID.Int64 != userID {
		plugin.StatusFail(w, http.StatusForbidden, "无权限")
		return
	}
	if err := p.repo.DeleteContact(r.Context(), id); err != nil {
		plugin.StatusFail(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeOKMsg(w, "已删除")
}

func (p *Plugin) clientSetDefaultContact(w http.ResponseWriter, r *http.Request) {
	userID, ok := plugin.RequireUserID(w, r)
	if !ok {
		return
	}
	id, err := readID(r, "id")
	if err != nil {
		plugin.StatusFail(w, http.StatusBadRequest, "参数错误")
		return
	}
	cnt, err := p.repo.GetContact(r.Context(), id)
	if err != nil {
		if err == ErrContactNotFound {
			plugin.StatusFail(w, http.StatusNotFound, "联系人不存在")
			return
		}
		plugin.StatusFail(w, http.StatusInternalServerError, err.Error())
		return
	}
	// 共享模板只读：不能被普通用户设为自己的默认联系人。
	if !cnt.UserID.Valid || cnt.UserID.Int64 != userID {
		plugin.StatusFail(w, http.StatusForbidden, "无权限")
		return
	}
	if err := p.repo.SetDefaultContact(r.Context(), sql.NullInt64{Int64: userID, Valid: true}, id); err != nil {
		plugin.StatusFail(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeOKMsg(w, "已设为默认")
}

// ---- 价目与订单（前台只读：价格一律来自服务端价目表） ----

// clientPrices 公开价目表（仅上架的后缀），前台据此展示"注册价/续费价"。
func (p *Plugin) clientPrices(w http.ResponseWriter, r *http.Request) {
	userID, ok := plugin.RequireUserID(w, r)
	if !ok {
		return
	}
	_ = userID
	rows, err := p.repo.ListPrices(r.Context())
	if err != nil {
		plugin.StatusFail(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, q := range rows {
		if !q.Enabled {
			continue
		}
		out = append(out, map[string]any{
			"tld":           q.TLD,
			"registerCents": q.RegisterCents,
			"renewCents":    q.RenewCents,
			"currency":      q.Currency,
			"minYears":      q.MinYears,
			"maxYears":      q.MaxYears,
			"registerPrice": money.FormatCents(q.RegisterCents),
			"renewPrice":    money.FormatCents(q.RenewCents),
		})
	}
	writeOK(w, map[string]any{"list": out})
}

// clientMyOrders 我的订单（注册/续费流水，含是否已退款）。
func (p *Plugin) clientMyOrders(w http.ResponseWriter, r *http.Request) {
	userID, ok := plugin.RequireUserID(w, r)
	if !ok {
		return
	}
	limit := 50
	if l, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && l > 0 && l <= 200 {
		limit = l
	}
	rows, err := p.repo.ListOrders(r.Context(), sql.NullInt64{Int64: userID, Valid: true}, limit, 0)
	if err != nil {
		plugin.StatusFail(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]map[string]any, len(rows))
	for i, o := range rows {
		out[i] = map[string]any{
			"id":          o.ID,
			"domain":      o.Domain,
			"kind":        o.Kind,
			"years":       o.Years,
			"amountCents": o.AmountCents,
			"amount":      money.FormatCents(o.AmountCents),
			"status":      o.Status,
			"createdAt":   o.CreatedAt.Format(time.RFC3339),
		}
	}
	writeOK(w, map[string]any{"list": out})
}

// 避免 fmt/strconv 未使用
var _ = fmt.Sprintf
var _ = strconv.Itoa
