package violation

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"lumeidc/internal/plugin"
)

// parseBool 表单布尔解析约定：空/"0"/"false" 为 false，其余为 true。
func parseBool(v string) bool {
	return v != "" && v != "0" && v != "false"
}

// parseTime 解析前端日期选择器值（兼容多种格式）；空串返回零 NullTime。
func parseTime(v string) (sql.NullTime, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return sql.NullTime{}, nil
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05", "2006-01-02 15:04", "2006-01-02"} {
		if ts, err := time.Parse(layout, v); err == nil {
			return sql.NullTime{Time: ts, Valid: true}, nil
		}
	}
	return sql.NullTime{}, errors.New("格式不正确")
}

// fmtTime NullTime 格式化（无效返回空串）。
func fmtTime(t sql.NullTime) string {
	if !t.Valid {
		return ""
	}
	return t.Time.Format("2006-01-02 15:04")
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

// recordItem 记录 JSON 视图（admin=true 附带邮箱/处理人/备注）。
// 非 admin 视图：description 截断到 200 字避免长文展示；evidence_url 不外泄（多为后台内部凭据）。
func recordItem(r RecordAdminRow, admin bool) map[string]any {
	m := map[string]any{
		"id":          r.ID,
		"user_id":     r.UserID,
		"user_name":   r.UserName,
		"type":        r.Type,
		"level":       r.Level,
		"level_label": levelLabels[r.Level],
		"public":      r.Public,
		"starts_at":   fmtTime(r.StartsAt),
		"expires_at":  fmtTime(r.ExpiresAt),
		"created_at":  r.CreatedAt.Format("2006-01-02 15:04"),
	}
	if admin {
		m["user_email"] = r.UserEmail
		m["handled_by"] = r.HandledBy
		m["note"] = r.Note
		m["description"] = r.Description
		m["evidence_url"] = r.EvidenceURL
		m["action"] = r.Action
	} else {
		m["description"] = truncateText(r.Description, 200)
		m["action"] = r.Action
	}
	return m
}

// truncateText 截断正文（按 rune），超出加省略号；空串原样返回。
func truncateText(s string, max int) string {
	if max <= 0 {
		return ""
	}
	rs := []rune(s)
	if len(rs) <= max {
		return s
	}
	return string(rs[:max]) + "…"
}

// announcementItem 公告 JSON 视图。
func announcementItem(a ViolationAnnouncement) map[string]any {
	return map[string]any{
		"id":         a.ID,
		"title":      a.Title,
		"content":    a.Content,
		"pinned":     a.Pinned,
		"hidden":     a.Hidden,
		"reads":      a.Reads,
		"created_at": a.CreatedAt.Format("2006-01-02 15:04"),
		"updated_at": a.UpdatedAt.Format("2006-01-02 15:04"),
	}
}

// formOptions 表单选项（配置的多行选项 + 等级枚举 + 默认公示）。
func (p *Plugin) formOptions(ctx context.Context) map[string]any {
	return map[string]any{
		"types":         splitLines(p.host.Config(ctx, "typeOptions")),
		"actions":       splitLines(p.host.Config(ctx, "actionOptions")),
		"levels":        levelLabels,
		"defaultPublic": parseBool(p.host.Config(ctx, "defaultPublic")),
	}
}

func (p *Plugin) adminList(w http.ResponseWriter, r *http.Request) {
	if !plugin.AdminOK(w, r) {
		return
	}
	q := r.URL.Query()
	page, limit := pageParam(q)
	rows, total, err := p.records.AdminList(r.Context(), RecordFilter{
		Keyword: strings.TrimSpace(q.Get("keyword")),
		Level:   strings.TrimSpace(q.Get("level")),
		Type:    strings.TrimSpace(q.Get("type")),
		Page:    page,
		Limit:   limit,
	})
	if err != nil {
		plugin.JSONFail(w, "查询失败")
		return
	}
	list := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		list = append(list, recordItem(row, true))
	}
	plugin.WriteJSON(w, map[string]any{"ok": 1, "list": list, "total": total, "page": page, "limit": limit})
}

func (p *Plugin) adminStats(w http.ResponseWriter, r *http.Request) {
	if !plugin.AdminOK(w, r) {
		return
	}
	total, active, public, err := p.records.Stats(r.Context())
	if err != nil {
		plugin.JSONFail(w, "查询失败")
		return
	}
	plugin.WriteJSON(w, map[string]any{"ok": 1, "stats": map[string]int{
		"total": total, "active": active, "public": public,
	}})
}

func (p *Plugin) adminUserSearch(w http.ResponseWriter, r *http.Request) {
	if !plugin.AdminOK(w, r) {
		return
	}
	users, err := p.users.Search(r.Context(), strings.TrimSpace(r.URL.Query().Get("keyword")), 20)
	if err != nil {
		plugin.JSONFail(w, "查询失败")
		return
	}
	list := make([]map[string]any, 0, len(users))
	for _, u := range users {
		list = append(list, map[string]any{"id": u.ID, "email": u.Email, "name": u.Name})
	}
	plugin.WriteJSON(w, map[string]any{"ok": 1, "list": list})
}

// adminForm 表单数据：id 空/0=新增（仅选项），非空=回填记录。
func (p *Plugin) adminForm(w http.ResponseWriter, r *http.Request) {
	if !plugin.AdminOK(w, r) {
		return
	}
	out := map[string]any{"ok": 1, "item": map[string]any{}, "options": p.formOptions(r.Context())}
	raw := strings.TrimSpace(r.URL.Query().Get("id"))
	if raw == "" {
		plugin.WriteJSON(w, out)
		return
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		plugin.StatusFail(w, 400, "参数错误")
		return
	}
	rec, err := p.records.Get(r.Context(), id)
	if err != nil {
		plugin.JSONFail(w, "查询失败")
		return
	}
	if rec == nil {
		plugin.JSONFail(w, "记录不存在")
		return
	}
	usr, err := p.users.Get(r.Context(), rec.UserID)
	if err != nil {
		plugin.JSONFail(w, "查询失败")
		return
	}
	if usr == nil {
		plugin.JSONFail(w, "用户不存在")
		return
	}
	out["item"] = recordItem(RecordAdminRow{Record: *rec, UserName: usr.Name, UserEmail: usr.Email}, true)
	plugin.WriteJSON(w, out)
}

func (p *Plugin) adminSave(w http.ResponseWriter, r *http.Request) {
	sess, ok := plugin.AdminSession(w, r)
	if !ok {
		return
	}
	if allowed, retry := p.checkAdminRate(r, sess.UserID, "save"); !allowed {
		w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())+1))
		plugin.StatusFail(w, http.StatusTooManyRequests, "操作过于频繁，请稍后再试")
		return
	}
	vals, err := parseFormValues(r)
	if err != nil {
		plugin.JSONFail(w, "请求格式错误")
		return
	}
	id := int64(0)
	if s := strings.TrimSpace(vals["id"]); s != "" {
		id, err = strconv.ParseInt(s, 10, 64)
		if err != nil || id < 0 {
			plugin.StatusFail(w, 400, "记录参数错误")
			return
		}
	}
	var old *Record
	if id > 0 {
		// 编辑模式：先确认记录存在再 Update，避免对不存在行返回 ok=1 的静默成功。
		got, gerr := p.records.Get(r.Context(), id)
		if gerr != nil {
			plugin.JSONFail(w, "查询失败")
			return
		}
		if got == nil {
			plugin.JSONFail(w, "记录不存在")
			return
		}
		old = got
	}
	userID, err := strconv.ParseInt(strings.TrimSpace(vals["user_id"]), 10, 64)
	if err != nil || userID <= 0 {
		plugin.StatusFail(w, 400, "请选择用户")
		return
	}
	usr, err := p.users.Get(r.Context(), userID)
	if err != nil {
		plugin.JSONFail(w, "查询用户失败")
		return
	}
	if usr == nil {
		plugin.JSONFail(w, "用户不存在")
		return
	}
	level := strings.TrimSpace(vals["level"])
	if _, ok := levelLabels[level]; !ok {
		plugin.StatusFail(w, 400, "请选择违规等级")
		return
	}
	typ := strings.TrimSpace(vals["type"])
	if opts := splitLines(p.host.Config(r.Context(), "typeOptions")); len(opts) > 0 && !contains(opts, typ) {
		plugin.StatusFail(w, 400, "违规类型不在允许的选项中")
		return
	}
	// 处置措施在前端为可选字段（可清空），仅在有值时校验选项归属
	action := strings.TrimSpace(vals["action"])
	if action != "" {
		if opts := splitLines(p.host.Config(r.Context(), "actionOptions")); len(opts) > 0 && !contains(opts, action) {
			plugin.StatusFail(w, 400, "处置措施不在允许的选项中")
			return
		}
	}
	description := strings.TrimSpace(vals["description"])
	if len([]rune(description)) > 2000 {
		plugin.StatusFail(w, 400, "违规描述不能超过 2000 字")
		return
	}
	startsAt, err := parseTime(vals["starts_at"])
	if err != nil {
		plugin.StatusFail(w, 400, "生效时间"+err.Error())
		return
	}
	expiresAt, err := parseTime(vals["expires_at"])
	if err != nil {
		plugin.StatusFail(w, 400, "过期时间"+err.Error())
		return
	}
	if startsAt.Valid && expiresAt.Valid && !expiresAt.Time.After(startsAt.Time) {
		plugin.StatusFail(w, 400, "过期时间必须晚于生效时间")
		return
	}
	rec := &Record{
		ID:          id,
		UserID:      userID,
		Type:        typ,
		Level:       level,
		Description: description,
		EvidenceURL: strings.TrimSpace(vals["evidence_url"]),
		Action:      action,
		StartsAt:    startsAt,
		ExpiresAt:   expiresAt,
		Public:      parseBool(vals["public"]),
		HandledBy:   sess.UserID,
		Note:        strings.TrimSpace(vals["note"]),
	}
	if id > 0 {
		if err := p.records.Update(r.Context(), rec); err != nil {
			plugin.JSONFail(w, "保存失败")
			return
		}
		// 仅实质变化（等级/措施/有效期等）才打扰用户，改错别字不发通知。
		if changes := materialChange(old, rec); len(changes) > 0 {
			p.notifyUser(r.Context(), rec.UserID, notifyTitleUpdated, violationUpdatedBody(rec.ID, changes))
		}
	} else {
		newID, err := p.records.Create(r.Context(), rec)
		if err != nil {
			plugin.JSONFail(w, "保存失败")
			return
		}
		plugin.Emit(r.Context(), EventViolationCreated, ViolationPayload{
			RecordID: newID, UserID: rec.UserID, Type: rec.Type, Level: rec.Level,
		})
		p.notifyUser(r.Context(), rec.UserID, notifyTitleCreated, violationCreatedBody(rec))
	}
	plugin.WriteJSON(w, map[string]any{"ok": 1})
}

func (p *Plugin) adminDelete(w http.ResponseWriter, r *http.Request) {
	sess, ok := plugin.AdminSession(w, r)
	if !ok {
		return
	}
	if allowed, retry := p.checkAdminRate(r, sess.UserID, "delete"); !allowed {
		w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())+1))
		plugin.StatusFail(w, http.StatusTooManyRequests, "操作过于频繁，请稍后再试")
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		plugin.StatusFail(w, 400, "参数错误")
		return
	}
	// 先确认存在再 Delete，避免对不存在行返回 ok=1 的静默成功。
	rec, gerr := p.records.Get(r.Context(), id)
	if gerr != nil {
		plugin.JSONFail(w, "查询失败")
		return
	}
	if rec == nil {
		plugin.JSONFail(w, "记录不存在")
		return
	}
	if err := p.records.Delete(r.Context(), id); err != nil {
		plugin.JSONFail(w, "删除失败")
		return
	}
	plugin.Emit(r.Context(), EventViolationRemoved, ViolationPayload{RecordID: id})
	// 撤销处罚同样要告知用户：处置不再生效，避免用户反复申诉。
	p.notifyUser(r.Context(), rec.UserID, notifyTitleRemoved, violationRemovedBody(id))
	plugin.WriteJSON(w, map[string]any{"ok": 1})
}

func (p *Plugin) adminAnnouncements(w http.ResponseWriter, r *http.Request) {
	if !plugin.AdminOK(w, r) {
		return
	}
	anns, err := p.anns.AdminList(r.Context())
	if err != nil {
		plugin.JSONFail(w, "查询失败")
		return
	}
	list := make([]map[string]any, 0, len(anns))
	for _, a := range anns {
		list = append(list, announcementItem(a))
	}
	plugin.WriteJSON(w, map[string]any{"ok": 1, "list": list})
}

func (p *Plugin) adminAnnouncementSave(w http.ResponseWriter, r *http.Request) {
	if !plugin.AdminOK(w, r) {
		return
	}
	vals, err := parseFormValues(r)
	if err != nil {
		plugin.JSONFail(w, "请求格式错误")
		return
	}
	id, _ := strconv.ParseInt(strings.TrimSpace(vals["id"]), 10, 64)
	title := strings.TrimSpace(vals["title"])
	if title == "" {
		plugin.StatusFail(w, 400, "标题不能为空")
		return
	}
	sess, ok := plugin.AdminSession(w, r)
	if !ok {
		return
	}
	ann := &ViolationAnnouncement{
		ID:        id,
		Title:     title,
		Content:   strings.TrimSpace(vals["content"]),
		Pinned:    parseBool(vals["pinned"]),
		Hidden:    parseBool(vals["hidden"]),
		CreatedBy: sess.UserID,
	}
	if _, err := p.anns.Save(r.Context(), ann); err != nil {
		plugin.JSONFail(w, "保存失败")
		return
	}
	plugin.WriteJSON(w, map[string]any{"ok": 1})
}

func (p *Plugin) adminAnnouncementDelete(w http.ResponseWriter, r *http.Request) {
	if !plugin.AdminOK(w, r) {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		plugin.StatusFail(w, 400, "参数错误")
		return
	}
	if err := p.anns.Delete(r.Context(), id); err != nil {
		plugin.JSONFail(w, "删除失败")
		return
	}
	plugin.WriteJSON(w, map[string]any{"ok": 1})
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
