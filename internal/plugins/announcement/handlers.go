package announcement

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"lumeidc/internal/plugin"
	"lumeidc/internal/repo"
)

// item 公告 JSON 视图（前台/管理共用键集；管理侧多 hidden/pinned/updated_at）。
func item(an repo.Announcement, admin bool) map[string]any {
	m := map[string]any{
		"id": an.ID, "title": an.Title, "category": an.Category, "summary": an.Summary,
		"content": an.Content, "cover": an.Cover, "pinned": an.Pinned, "reads": an.Reads,
		"created_at": an.CreatedAt.Format("2006-01-02"),
	}
	if admin {
		m["hidden"] = an.Hidden
		m["created_at"] = an.CreatedAt.Format("2006-01-02 15:04")
		m["updated_at"] = an.UpdatedAt.Format("2006-01-02 15:04")
	}
	return m
}

// ---- 前台（公开，无需登录；插件禁用时由框架闸门返回 404） ----

// clientList GET /plugin/announcement/list — 公告列表（分类/关键字/分页，仅显示中）。
func (p *Plugin) clientList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	category := strings.TrimSpace(q.Get("category"))
	keyword := strings.ToLower(strings.TrimSpace(q.Get("keyword")))
	page, err := strconv.Atoi(q.Get("page"))
	if err != nil || page < 1 {
		page = 1
	}
	limit, err := strconv.Atoi(q.Get("limit"))
	if err != nil || limit < 1 || limit > 50 {
		limit = 10
	}

	rows, err := p.store.List(r.Context(), true)
	if err != nil {
		plugin.JSONFail(w, "查询失败")
		return
	}
	catSet := map[string]struct{}{}
	filtered := make([]map[string]any, 0, len(rows))
	for _, an := range rows {
		if an.Category != "" {
			catSet[an.Category] = struct{}{}
		}
		if category != "" && an.Category != category {
			continue
		}
		if keyword != "" && !strings.Contains(strings.ToLower(an.Title), keyword) {
			continue
		}
		filtered = append(filtered, item(an, false))
	}

	categories := make([]string, 0, len(catSet))
	for c := range catSet {
		categories = append(categories, c)
	}
	sort.Strings(categories)

	total := len(filtered)
	start := (page - 1) * limit
	if start > total {
		start = total
	}
	end := start + limit
	if end > total {
		end = total
	}

	plugin.WriteJSON(w, map[string]any{
		"ok": 1, "list": filtered[start:end], "total": total,
		"page": page, "limit": limit, "categories": categories,
	})
}

// clientDetail GET /plugin/announcement/detail/{id} — 公告详情（阅读量 +1）。
func (p *Plugin) clientDetail(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	an, err := p.store.Get(r.Context(), id)
	if err != nil || an.Hidden {
		http.NotFound(w, r)
		return
	}
	if err := p.store.IncRead(r.Context(), id); err == nil {
		an.Reads++
	}
	plugin.WriteJSON(w, map[string]any{"ok": 1, "item": item(*an, false)})
}

// ---- 管理（AdminOK 鉴权） ----

// adminList GET /admin/plugin/announcement/list — 全部公告（含隐藏）。
func (p *Plugin) adminList(w http.ResponseWriter, r *http.Request) {
	if !plugin.AdminOK(w, r) {
		return
	}
	rows, err := p.store.List(r.Context(), false)
	if err != nil {
		plugin.JSONFail(w, "查询失败")
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, an := range rows {
		out = append(out, item(an, true))
	}
	plugin.WriteJSON(w, map[string]any{"ok": 1, "list": out})
}

// adminForm GET /admin/plugin/announcement/form?id= — 新建/编辑回填。
func (p *Plugin) adminForm(w http.ResponseWriter, r *http.Request) {
	if !plugin.AdminOK(w, r) {
		return
	}
	var announcement any
	if id := r.URL.Query().Get("id"); id != "" {
		if pid, err := strconv.ParseInt(id, 10, 64); err == nil {
			if an, gerr := p.store.Get(r.Context(), pid); gerr == nil && an != nil {
				// 走 item 视图序列化，与列表键名一致（结构体直接序列化会输出帕斯卡键）。
				announcement = item(*an, true)
			}
		}
	}
	plugin.WriteJSON(w, map[string]any{"ok": 1, "announcement": announcement})
}

// adminSave POST /admin/plugin/announcement/save — 保存（新增/更新）。
func (p *Plugin) adminSave(w http.ResponseWriter, r *http.Request) {
	if !plugin.AdminOK(w, r) {
		return
	}
	var vals map[string]string
	if strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "application/json") {
		var raw map[string]any
		if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
			plugin.JSONFail(w, "请求格式错误")
			return
		}
		vals = map[string]string{}
		for k, v := range raw {
			if s, ok := v.(string); ok {
				vals[k] = s
			} else if b, ok := v.(bool); ok {
				if b {
					vals[k] = "1"
				}
			}
		}
	} else {
		if err := r.ParseForm(); err != nil {
			plugin.JSONFail(w, "请求格式错误")
			return
		}
		vals = map[string]string{}
		for k := range r.PostForm {
			vals[k] = r.PostFormValue(k)
		}
	}
	id, _ := strconv.ParseInt(vals["id"], 10, 64)
	title := strings.TrimSpace(vals["title"])
	if title == "" {
		plugin.JSONFail(w, "标题不能为空")
		return
	}
	hidden := vals["hidden"] != "" && vals["hidden"] != "0" && vals["hidden"] != "false"
	pinned := vals["pinned"] != "" && vals["pinned"] != "0" && vals["pinned"] != "false"
	if err := p.store.Save(r.Context(), id, title,
		strings.TrimSpace(vals["category"]), strings.TrimSpace(vals["summary"]),
		vals["content"], strings.TrimSpace(vals["cover"]), hidden, pinned); err != nil {
		plugin.JSONFail(w, "保存失败，请稍后重试")
		return
	}
	plugin.WriteJSON(w, map[string]any{"ok": 1, "msg": "已保存"})
}

// adminDelete POST /admin/plugin/announcement/{id}/delete — 删除。
func (p *Plugin) adminDelete(w http.ResponseWriter, r *http.Request) {
	if !plugin.AdminOK(w, r) {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		plugin.StatusFail(w, 400, "参数错误")
		return
	}
	if err := p.store.Delete(r.Context(), id); err != nil {
		plugin.JSONFail(w, "删除失败，请稍后重试")
		return
	}
	plugin.WriteJSON(w, map[string]any{"ok": 1, "msg": "已删除"})
}
