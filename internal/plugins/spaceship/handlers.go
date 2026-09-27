package spaceship

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"lumeidc/internal/money"
	"lumeidc/internal/plugin"
)

// ---- 通用 ----

// writeOK 输出扁平成功响应 {ok:1, ...m}（与核心插件前端读取约定一致）。
func writeOK(w http.ResponseWriter, m map[string]any) {
	m["ok"] = 1
	plugin.WriteJSON(w, m)
}

func writeOKMsg(w http.ResponseWriter, msg string) {
	plugin.WriteJSON(w, map[string]any{"ok": 1, "msg": msg})
}

// readJSON 读取请求体 JSON 到 v。
func readJSON(r *http.Request, v any) error {
	defer r.Body.Close()
	return json.NewDecoder(r.Body).Decode(v)
}

// readID 从 URL 路径解析 id 参数（http.ServeMux "GET /domains/{id}" 模式）。
func readID(r *http.Request, name string) (int64, error) {
	raw := r.PathValue(name)
	return strconv.ParseInt(raw, 10, 64)
}

// ---- 联系人管理 ----

func (p *Plugin) adminListContacts(w http.ResponseWriter, r *http.Request) {
	if !plugin.AdminOK(w, r) {
		return
	}
	rows, err := p.repo.ListContacts(r.Context(), sql.NullInt64{})
	if err != nil {
		plugin.StatusFail(w, http.StatusInternalServerError, err.Error())
		return
	}
	type ContactItem struct {
		ID            int64  `json:"id"`
		ContactID     string `json:"contactId"`
		Label         string `json:"label"`
		Name          string `json:"name"`
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
		IsDefault     bool   `json:"isDefault"`
		Shared        bool   `json:"shared"` // user_id IS NULL：管理员共享模板
	}
	out := make([]ContactItem, len(rows))
	for i, c := range rows {
		out[i] = ContactItem{ID: c.ID, ContactID: c.ContactID, Label: c.Label,
			Name:  strings.TrimSpace(c.FirstName + " " + c.LastName),
			Email: c.Email, Country: c.Country, Phone: c.Phone, IsDefault: c.IsDefault,
			FirstName: c.FirstName, LastName: c.LastName, Organization: c.Organization.String,
			Address1: c.Address1, Address2: c.Address2.String, City: c.City,
			StateProvince: c.StateProvince.String, PostalCode: c.PostalCode.String,
			Shared: !c.UserID.Valid}
	}
	writeOK(w, map[string]any{"list": out})
}

func (p *Plugin) adminSaveContact(w http.ResponseWriter, r *http.Request) {
	if !plugin.AdminOK(w, r) {
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
		ExistingID    int64  `json:"existingId"` // 编辑已有联系人时的本地 id
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
	// 先调 Spaceship 创建/更新联系人
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
	// 编辑既有联系人（existingId）时带上原 contactId，让上游走更新而不是再建一条。
	// 归属必须沿用原记录：否则后台编辑用户自建联系人会把它变成共享模板（越权扩大可见范围）。
	var oldUserID sql.NullInt64
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
		sc.ContactID = old.ContactID
		oldUserID = old.UserID
	}
	contactID, err := c.SaveContact(r.Context(), sc)
	if err != nil {
		plugin.JSONFail(w, "Spaceship 保存联系人失败: "+err.Error())
		return
	}
	// 再落本地库（编辑时保留原归属：用户自建的仍是他的，共享模板仍是共享的）
	row := &ContactRow{
		ContactID:     contactID,
		UserID:        oldUserID,
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
		// 作用域跟随联系人归属：用户自建联系人只影响该用户，共享模板只影响共享池。
		_ = p.repo.SetDefaultContact(r.Context(), saved.UserID, saved.ID)
	}
	writeOK(w, map[string]any{"id": saved.ID, "contactId": contactID})
}

func (p *Plugin) adminDeleteContact(w http.ResponseWriter, r *http.Request) {
	if !plugin.AdminOK(w, r) {
		return
	}
	id, err := readID(r, "id")
	if err != nil {
		plugin.StatusFail(w, http.StatusBadRequest, "参数错误")
		return
	}
	if err := p.repo.DeleteContact(r.Context(), id); err != nil {
		if err == ErrContactNotFound {
			plugin.StatusFail(w, http.StatusNotFound, "联系人不存在")
			return
		}
		plugin.StatusFail(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeOKMsg(w, "已删除")
}

func (p *Plugin) adminSetDefaultContact(w http.ResponseWriter, r *http.Request) {
	if !plugin.AdminOK(w, r) {
		return
	}
	id, err := readID(r, "id")
	if err != nil {
		plugin.StatusFail(w, http.StatusBadRequest, "参数错误")
		return
	}
	// 作用域跟随联系人自身归属：共享模板只在共享池内生效，用户自建的只影响该用户，
	// 避免后台一次操作把其他人的默认联系人全部清空。
	cnt, err := p.repo.GetContact(r.Context(), id)
	if err != nil {
		if err == ErrContactNotFound {
			plugin.StatusFail(w, http.StatusNotFound, "联系人不存在")
			return
		}
		plugin.StatusFail(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := p.repo.SetDefaultContact(r.Context(), cnt.UserID, id); err != nil {
		plugin.StatusFail(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeOKMsg(w, "已设为默认")
}

// ---- 域名管理 ----

func (p *Plugin) adminListDomains(w http.ResponseWriter, r *http.Request) {
	if !plugin.AdminOK(w, r) {
		return
	}
	rows, err := p.repo.ListDomains(r.Context(), sql.NullInt64{})
	if err != nil {
		plugin.StatusFail(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]map[string]any, len(rows))
	for i, d := range rows {
		item := map[string]any{
			"id":              d.ID,
			"userId":          d.UserID,
			"domain":          d.Domain,
			"years":           d.Years,
			"paidAmountCents": d.PaidAmountCents,
			"status":          d.Status,
			"privacyLevel":    d.PrivacyLevel,
			"autoRenew":       d.AutoRenew,
		}
		if d.SpaceshipDomainID.Valid {
			item["spaceshipDomainId"] = d.SpaceshipDomainID.String
		}
		if d.RegisteredAt.Valid {
			item["registeredAt"] = d.RegisteredAt.Time.Format(time.RFC3339)
		}
		if d.ExpiresAt.Valid {
			item["expiresAt"] = d.ExpiresAt.Time.Format(time.RFC3339)
		}
		out[i] = item
	}
	writeOK(w, map[string]any{"list": out})
}

func (p *Plugin) adminRegisterDomain(w http.ResponseWriter, r *http.Request) {
	if !plugin.AdminOK(w, r) {
		return
	}
	c := p.cfgClient(r.Context())
	if c == nil {
		plugin.JSONFail(w, "Spaceship API Key/Secret 未配置")
		return
	}
	var body struct {
		Domain       string `json:"domain"`
		Years        int    `json:"years"`
		ContactID    int64  `json:"contactId"`    // 本地 contacts 表 id
		UserId       int64  `json:"userId"`       // 归属用户（谁付钱）
		AllowPremium bool   `json:"allowPremium"` // 管理员已确认溢价价格
		// paidAmount 已废弃：后台代注册同样按服务端价目表扣款（此前完全不扣款＝白送域名）。
		PaidAmount string `json:"paidAmount"`
	}
	if err := readJSON(r, &body); err != nil {
		plugin.StatusFail(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	if body.UserId <= 0 {
		plugin.JSONFail(w, "请指定归属用户")
		return
	}
	adminID := int64(0)
	if sess, ok := plugin.AdminSession(w, r); ok && sess != nil {
		adminID = sess.UserID
	}
	res, err := p.registerInternal(r.Context(), c, registerInput{
		Domain:       body.Domain,
		Years:        body.Years,
		UserID:       body.UserId,
		ContactID:    body.ContactID,
		AdminID:      adminID,
		AllowPremium: body.AllowPremium,
	})
	if err != nil {
		failFromErr(w, err)
		return
	}
	writeRegisterResult(w, res)
}

func (p *Plugin) adminGetDomain(w http.ResponseWriter, r *http.Request) {
	if !plugin.AdminOK(w, r) {
		return
	}
	id, err := readID(r, "id")
	if err != nil {
		plugin.StatusFail(w, http.StatusBadRequest, "参数错误")
		return
	}
	d, err := p.repo.GetDomain(r.Context(), id)
	if err != nil {
		if err == ErrDomainNotFound {
			plugin.StatusFail(w, http.StatusNotFound, "域名不存在")
			return
		}
		plugin.StatusFail(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeOK(w, map[string]any{"item": d})
}

func (p *Plugin) adminRenewDomain(w http.ResponseWriter, r *http.Request) {
	if !plugin.AdminOK(w, r) {
		return
	}
	id, err := readID(r, "id")
	if err != nil {
		plugin.StatusFail(w, http.StatusBadRequest, "参数错误")
		return
	}
	adminID := int64(0)
	if sess, ok := plugin.AdminSession(w, r); ok && sess != nil {
		adminID = sess.UserID
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
		ActorID:  adminID,
		IsAdmin:  true,
	})
	if err != nil {
		failFromErr(w, err)
		return
	}
	writeRenewResult(w, res)
}

func (p *Plugin) adminAutoRenew(w http.ResponseWriter, r *http.Request) {
	if !plugin.AdminOK(w, r) {
		return
	}
	id, err := readID(r, "id")
	if err != nil {
		plugin.StatusFail(w, http.StatusBadRequest, "参数错误")
		return
	}
	var body struct {
		Enable bool `json:"enable"`
	}
	if err := readJSON(r, &body); err != nil {
		plugin.StatusFail(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	d, err := p.repo.GetDomain(r.Context(), id)
	if err != nil {
		if err == ErrDomainNotFound {
			plugin.StatusFail(w, http.StatusNotFound, "域名不存在")
			return
		}
		plugin.StatusFail(w, http.StatusInternalServerError, err.Error())
		return
	}
	// 只有已生效的域名才能开自动续费：pending/deleted 状态下打开会给出错误的续费预期。
	if body.Enable && d.Status != "active" {
		plugin.JSONFail(w, "仅已生效的域名可开启自动续费（当前状态："+d.Status+"）")
		return
	}
	if d.SpaceshipDomainID.Valid {
		c := p.cfgClient(r.Context())
		if c != nil {
			if err := c.SetAutoRenew(r.Context(), d.Domain, body.Enable); err != nil {
				plugin.JSONFail(w, "Spaceship 设置自动续费失败: "+err.Error())
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

func (p *Plugin) adminPrivacy(w http.ResponseWriter, r *http.Request) {
	if !plugin.AdminOK(w, r) {
		return
	}
	id, err := readID(r, "id")
	if err != nil {
		plugin.StatusFail(w, http.StatusBadRequest, "参数错误")
		return
	}
	var body struct {
		// Level 官方仅支持 high / public 两档，其他值一律在上游判 400。
		Level string `json:"level"`
		// Consent 注册人对 WHOIS 隐私条款的同意（官方 userConsent 字段，必填）。
		// 后台代客操作默认视为已取得同意，除非显式传 false。
		Consent *bool `json:"consent"`
	}
	if err := readJSON(r, &body); err != nil {
		plugin.StatusFail(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	level := strings.TrimSpace(body.Level)
	if level != "high" && level != "public" {
		plugin.StatusFail(w, http.StatusBadRequest, "隐私保护等级仅支持 high / public")
		return
	}
	consent := true
	if body.Consent != nil {
		consent = *body.Consent
	}
	d, err := p.repo.GetDomain(r.Context(), id)
	if err != nil {
		if err == ErrDomainNotFound {
			plugin.StatusFail(w, http.StatusNotFound, "域名不存在")
			return
		}
		plugin.StatusFail(w, http.StatusInternalServerError, err.Error())
		return
	}
	// 未生效/已删除的域名在上游不存在，调用必然 404，直接拦截避免脏操作记录。
	if d.Status != "active" {
		plugin.JSONFail(w, "仅已生效的域名可修改隐私保护（当前状态："+d.Status+"）")
		return
	}
	c := p.cfgClient(r.Context())
	if c == nil {
		plugin.JSONFail(w, "未配置 Spaceship API Key/Secret，无法同步上游")
		return
	}
	if err := c.SetPrivacy(r.Context(), d.Domain, level, consent); err != nil {
		plugin.JSONFail(w, "Spaceship 设置隐私保护失败: "+err.Error())
		return
	}
	// 上游成功后才落库，避免本地与上游隐私等级分叉（上游 400 时本地不得变更）。
	if err := p.repo.UpdateDomain(r.Context(), id, map[string]any{"privacy_level": level}); err != nil {
		plugin.StatusFail(w, http.StatusInternalServerError, "本地保存失败: "+err.Error())
		return
	}
	writeOKMsg(w, "已更新")
}

func (p *Plugin) adminDeleteDomain(w http.ResponseWriter, r *http.Request) {
	sess, ok := plugin.AdminSession(w, r)
	if !ok {
		return
	}
	if allowed, retry := p.checkAdminRate(r, sess.UserID, "delete"); !allowed {
		w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())+1))
		plugin.StatusFail(w, http.StatusTooManyRequests, "操作过于频繁，请稍后再试")
		return
	}
	id, err := readID(r, "id")
	if err != nil {
		plugin.StatusFail(w, http.StatusBadRequest, "参数错误")
		return
	}
	// MVP 只软删除本地记录（Spaceship API 没确认，暂不调用）
	_ = p.repo.UpdateDomain(r.Context(), id, map[string]any{
		"status":     "deleted",
		"deleted_at": time.Now(),
	})
	writeOKMsg(w, "已标记删除")
}

// ---- 服务端价目表（商业运营：价格只能由管理员在后台维护） ----

// adminListPrices 价目表（后台定价页 / 前台展示均可读）。
func (p *Plugin) adminListPrices(w http.ResponseWriter, r *http.Request) {
	if !plugin.AdminOK(w, r) {
		return
	}
	rows, err := p.repo.ListPrices(r.Context())
	if err != nil {
		plugin.StatusFail(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]map[string]any, len(rows))
	for i, q := range rows {
		out[i] = map[string]any{
			"id":            q.ID,
			"tld":           q.TLD,
			"registerCents": q.RegisterCents,
			"renewCents":    q.RenewCents,
			"currency":      q.Currency,
			"enabled":       q.Enabled,
			"minYears":      q.MinYears,
			"maxYears":      q.MaxYears,
			"registerPrice": money.FormatCents(q.RegisterCents),
			"renewPrice":    money.FormatCents(q.RenewCents),
		}
	}
	writeOK(w, map[string]any{"list": out})
}

// adminCheck 管理员查询域名可用性与溢价报价。
// 溢价域名前台不可自助注册，必须由管理员在此确认真实价格后，
// 用 allowPremium=true 代注册（此时仍按价目表扣款，溢价差价由人工线下结算）。
func (p *Plugin) adminCheck(w http.ResponseWriter, r *http.Request) {
	if !plugin.AdminOK(w, r) {
		return
	}
	c := p.cfgClient(r.Context())
	if c == nil {
		plugin.JSONFail(w, "Spaceship API Key/Secret 未配置")
		return
	}
	var body struct {
		Domain string `json:"domain"`
	}
	if err := readJSON(r, &body); err != nil {
		plugin.StatusFail(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	domain := strings.TrimSpace(strings.ToLower(body.Domain))
	if domain == "" || !strings.Contains(domain, ".") {
		plugin.StatusFail(w, http.StatusBadRequest, "域名格式错误")
		return
	}
	res, err := c.CheckOne(r.Context(), domain)
	if err != nil {
		plugin.JSONFail(w, "查询失败: "+err.Error())
		return
	}
	item := map[string]any{
		"domain":      domain,
		"result":      res.Result,
		"available":   res.IsAvailable(),
		"isPremium":   res.IsPremium(),
		"needConfirm": res.IsPremium(),
	}
	if res.PremiumPricing != nil {
		price := res.PremiumPricing.PremiumPrice
		if price <= 0 {
			price = res.PremiumPricing.Price
		}
		item["premiumPrice"] = price
		item["currency"] = res.PremiumPricing.Currency
	}
	// 同时给出本地价目表价格，便于管理员对比溢价差额。
	if cents, row, err := p.priceOf(r.Context(), domain, "register", 1); err == nil {
		item["listPriceCents"] = cents
		item["listPrice"] = money.FormatCents(cents)
		item["tld"] = row.TLD
	}
	writeOK(w, map[string]any{"item": item})
}

// adminSavePrice 新增/修改一条价目。金额以「元」字符串入参（防浮点误差）。
func (p *Plugin) adminSavePrice(w http.ResponseWriter, r *http.Request) {
	if !plugin.AdminOK(w, r) {
		return
	}
	var body struct {
		TLD           string `json:"tld"`
		RegisterPrice string `json:"registerPrice"`
		RenewPrice    string `json:"renewPrice"`
		Currency      string `json:"currency"`
		Enabled       *bool  `json:"enabled"`
		MinYears      int    `json:"minYears"`
		MaxYears      int    `json:"maxYears"`
	}
	if err := readJSON(r, &body); err != nil {
		plugin.StatusFail(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	tld := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(body.TLD, ".")))
	if tld == "" {
		plugin.StatusFail(w, http.StatusBadRequest, "后缀不能为空")
		return
	}
	reg, regCents, err := money.ParseNonNegative(body.RegisterPrice, maxPriceCents)
	if err != nil || regCents <= 0 {
		plugin.StatusFail(w, http.StatusBadRequest, "注册价格不合法")
		return
	}
	_, renCents, err := money.ParseNonNegative(body.RenewPrice, maxPriceCents)
	if err != nil || renCents <= 0 {
		plugin.StatusFail(w, http.StatusBadRequest, "续费价格不合法")
		return
	}
	cur := strings.ToUpper(strings.TrimSpace(body.Currency))
	if cur == "" {
		cur = "CNY"
	}
	enabled := true
	if body.Enabled != nil {
		enabled = *body.Enabled
	}
	minYears, maxYears := body.MinYears, body.MaxYears
	if minYears < 1 {
		minYears = 1
	}
	if maxYears < minYears || maxYears > 10 {
		maxYears = 10
	}
	row, err := p.repo.UpsertPrice(r.Context(), &PriceRow{
		TLD:           tld,
		RegisterCents: regCents,
		RenewCents:    renCents,
		Currency:      cur,
		Enabled:       enabled,
		MinYears:      minYears,
		MaxYears:      maxYears,
	})
	if err != nil {
		plugin.StatusFail(w, http.StatusInternalServerError, "保存失败: "+err.Error())
		return
	}
	_ = reg
	writeOK(w, map[string]any{"id": row.ID, "tld": row.TLD})
}

// adminDeletePrice 删除价目（删除后该后缀不可自助注册）。
func (p *Plugin) adminDeletePrice(w http.ResponseWriter, r *http.Request) {
	if !plugin.AdminOK(w, r) {
		return
	}
	tld := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(r.PathValue("tld"), ".")))
	if tld == "" {
		plugin.StatusFail(w, http.StatusBadRequest, "参数错误")
		return
	}
	if err := p.repo.DeletePrice(r.Context(), tld); err != nil {
		plugin.StatusFail(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeOKMsg(w, "已删除")
}

// ---- 订单/对账 ----

// adminListOrders 订单流水（对账：谁买了什么、扣了多少、是否已退）。
func (p *Plugin) adminListOrders(w http.ResponseWriter, r *http.Request) {
	if !plugin.AdminOK(w, r) {
		return
	}
	limit := 50
	if l, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && l > 0 && l <= 500 {
		limit = l
	}
	offset := 0
	if o, err := strconv.Atoi(r.URL.Query().Get("offset")); err == nil && o >= 0 {
		offset = o
	}
	rows, err := p.repo.ListOrders(r.Context(), sql.NullInt64{}, limit, offset)
	if err != nil {
		plugin.StatusFail(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]map[string]any, len(rows))
	for i, o := range rows {
		out[i] = map[string]any{
			"id":          o.ID,
			"domain":      o.Domain,
			"userId":      o.UserID,
			"kind":        o.Kind,
			"years":       o.Years,
			"amountCents": o.AmountCents,
			"amount":      money.FormatCents(o.AmountCents),
			"status":      o.Status,
			"note":        o.Note.String,
			"createdAt":   o.CreatedAt.Format(time.RFC3339),
		}
	}
	writeOK(w, map[string]any{"list": out})
}

// ---- 操作日志 ----

func (p *Plugin) adminListOperations(w http.ResponseWriter, r *http.Request) {
	if !plugin.AdminOK(w, r) {
		return
	}
	limit := 50
	if l, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && l > 0 && l <= 500 {
		limit = l
	}
	offset := 0
	if o, err := strconv.Atoi(r.URL.Query().Get("offset")); err == nil && o >= 0 {
		offset = o
	}
	rows, err := p.repo.ListOperations(r.Context(), limit, offset)
	if err != nil {
		plugin.StatusFail(w, http.StatusInternalServerError, err.Error())
		return
	}
	total, _ := p.repo.CountOperations(r.Context())
	writeOK(w, map[string]any{"items": rows, "total": total})
}

// adminRetryOperation 管理员手动重试一次轮询：
//   - 终态（success / failed）拒绝重试；重复触发既浪费上游额度又给用户"还在处理中"的错觉。
//   - pending 但已超过 15 分钟窗口（ListPending 已主动丢弃），先把 started_at 重新置为 now()
//     让下一次 cron 还能拣到，然后 pollOne 立刻拉一次 —— 这是"卡死复活"语义。
//   - pending 且仍在窗口内，只走一次 pollOne 即可，不动时间戳。
//   - 入口按 <adminID>|<ip>|retry 限流（默认 30 次/分钟），防连点打爆上游。
func (p *Plugin) adminRetryOperation(w http.ResponseWriter, r *http.Request) {
	sess, ok := plugin.AdminSession(w, r)
	if !ok {
		return
	}
	if allowed, retry := p.checkAdminRate(r, sess.UserID, "retry"); !allowed {
		w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())+1))
		plugin.StatusFail(w, http.StatusTooManyRequests, "操作过于频繁，请稍后再试")
		return
	}
	id, err := readID(r, "id")
	if err != nil {
		plugin.StatusFail(w, http.StatusBadRequest, "参数错误")
		return
	}
	op, err := p.repo.GetOperation(r.Context(), id)
	if err != nil {
		if err == ErrOpNotFound {
			plugin.StatusFail(w, http.StatusNotFound, "操作不存在")
			return
		}
		plugin.StatusFail(w, http.StatusInternalServerError, err.Error())
		return
	}
	if op.Status == "success" || op.Status == "failed" {
		plugin.StatusFail(w, http.StatusBadRequest, "该操作已结束（"+op.Status+"），无需重试")
		return
	}
	// pending：先把 started_at 复位（no-op 若非 pending，亦不会有副作用），再手动 poll 一次。
	resurrected := false
	if op.Status == "pending" {
		if ok, err := p.repo.MarkPendingRetried(r.Context(), op.ID); err == nil && ok &&
			time.Since(op.StartedAt) > defStuckAfter {
			resurrected = true
		}
	}
	c := p.cfgClient(r.Context())
	if c == nil {
		plugin.JSONFail(w, "Spaceship API Key/Secret 未配置")
		return
	}
	_ = p.pollOne(r.Context(), c, op)
	if resurrected {
		writeOKMsg(w, "操作已从卡死状态恢复，并触发了一次轮询")
		return
	}
	writeOKMsg(w, "已触发轮询")
}

// ---- 连通性测试 ----

func (p *Plugin) adminTestConnection(w http.ResponseWriter, r *http.Request) {
	if !plugin.AdminOK(w, r) {
		return
	}
	c := p.cfgClient(r.Context())
	if c == nil {
		plugin.JSONFail(w, "Spaceship API Key/Secret 未配置")
		return
	}
	if err := c.TestConnection(r.Context()); err != nil {
		plugin.JSONFail(w, "连接失败: "+err.Error())
		return
	}
	writeOKMsg(w, "连接正常")
}

// 避免 fmt 未使用
var _ = fmt.Sprintf
