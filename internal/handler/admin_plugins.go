package handler

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"

	"lumeidc/internal/middleware"
	"lumeidc/internal/plugin"
	"lumeidc/internal/repo"
	"lumeidc/internal/service"
)

// AdminPlugins 插件管理：清单 / 启停切换 / 后台首页挂件收集。
type AdminPlugins struct {
	AdminLog *repo.AdminLog
}

// auditPluginOp 记录插件相关后台操作审计（失败仅记日志，不阻断主流程）。
// 插件无数值主键，target_id 统一记 0，名称放 detail。
func auditPluginOp(l *repo.AdminLog, r *http.Request, action, detail string) {
	if l == nil {
		return
	}
	var aid int64
	if s := middleware.FromSession(r.Context()); s != nil {
		aid = s.UserID
	}
	l.Record(aid, action, "plugin", 0, detail, r.RemoteAddr)
}

type pluginListItem struct {
	Name         string `json:"name"`
	Title        string `json:"title"`
	Version      string `json:"version"`
	Description  string `json:"description"`
	Enabled      bool   `json:"enabled"`
	HasAdminPage bool   `json:"hasAdminPage"`
	HasConfig    bool   `json:"hasConfig"`
	MenuTitle    string `json:"menuTitle,omitempty"`
	MenuIcon     string `json:"menuIcon,omitempty"`
	MenuParent   string `json:"menuParent,omitempty"`
}

func (h *AdminPlugins) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin/plugins", h.List)
	mux.HandleFunc("POST /admin/plugins/{name}/toggle", h.Toggle)
	mux.HandleFunc("GET /admin/widgets", h.Widgets)
	// 邮件出站渠道清单（内置 smtp + 插件注册的 MailSender）。
	mux.HandleFunc("GET /admin/mail-senders", h.MailSenders)
	// 前台用户侧：启用且有前台页的插件菜单（登录用户可见）。
	mux.HandleFunc("GET /plugins/client", h.ClientList)
	// 前台内容注入（公开：统计代码等在登录页也需生效）。
	mux.HandleFunc("GET /plugins/injections", h.Injections)
}

// List 全量插件清单（含禁用——管理页需要看到全部；菜单合并由前端按 enabled 过滤）。
func (h *AdminPlugins) List(w http.ResponseWriter, r *http.Request) {
	if !adminRequire(w, r) {
		return
	}
	list := []pluginListItem{}
	for _, p := range plugin.All() {
		info := p.Info()
		item := pluginListItem{
			Name:        info.Name,
			Title:       info.Title,
			Version:     info.Version,
			Description: info.Description,
			Enabled:     plugin.Enabled(info.Name),
		}
		_, item.HasConfig = p.(plugin.ConfigSchemaProvider)
		if mp, ok := p.(plugin.AdminMenuProvider); ok {
			m := mp.AdminMenu()
			item.HasAdminPage = true
			item.MenuTitle = m.Title
			item.MenuIcon = m.Icon
			item.MenuParent = m.Parent
		}
		list = append(list, item)
	}
	writeJSON(w, map[string]any{"ok": 1, "plugins": list})
}

// Toggle 启用/禁用插件（软禁用：即时生效，无需重启；迁移与数据不受影响）。
func (h *AdminPlugins) Toggle(w http.ResponseWriter, r *http.Request) {
	if !adminRequire(w, r) {
		return
	}
	name := r.PathValue("name")
	found := false
	for _, p := range plugin.All() {
		if p.Info().Name == name {
			found = true
			break
		}
	}
	if !found {
		writeJSON(w, map[string]any{"ok": 0, "msg": "插件不存在"})
		return
	}
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, map[string]any{"ok": 0, "msg": "请求格式错误"})
		return
	}
	if err := plugin.SetEnabled(r.Context(), name, req.Enabled); err != nil {
		writeJSON(w, map[string]any{"ok": 0, "msg": "保存失败，请稍后重试"})
		return
	}
	state := "禁用"
	if req.Enabled {
		state = "启用"
	}
	auditPluginOp(h.AdminLog, r, "plugin_toggle", fmt.Sprintf("%s插件 %s", state, name))
	writeJSON(w, map[string]any{"ok": 1})
}

// Widgets 收集启用插件贡献的后台首页挂件（单个失败跳过记日志，不影响其他）。
func (h *AdminPlugins) Widgets(w http.ResponseWriter, r *http.Request) {
	if !adminRequire(w, r) {
		return
	}
	out := []map[string]any{}
	for _, p := range plugin.All() {
		name := p.Info().Name
		if !plugin.Enabled(name) {
			continue
		}
		wp, ok := p.(plugin.AdminWidgetProvider)
		if !ok {
			continue
		}
		data, err := wp.AdminWidget(r.Context())
		if err != nil || data == nil {
			if err != nil {
				log.Printf("[plugin] 插件 %s 挂件取数失败: %v", name, err)
			}
			continue
		}
		out = append(out, map[string]any{"plugin": name, "title": data.Title, "icon": data.Icon, "rows": data.Rows})
	}
	writeJSON(w, map[string]any{"ok": 1, "widgets": out})
}

// MailSenders 邮件出站渠道清单（内置 SMTP + 插件注册渠道；后台邮件设置页下拉）。
func (h *AdminPlugins) MailSenders(w http.ResponseWriter, r *http.Request) {
	if !adminRequire(w, r) {
		return
	}
	out := []map[string]string{{"name": "smtp", "label": "内置 SMTP（多账号轮换）"}}
	for _, s := range service.MailSenders() {
		out = append(out, map[string]string{"name": s.Name(), "label": s.Label()})
	}
	writeJSON(w, map[string]any{"ok": 1, "senders": out})
}

// Injections 前台内容注入片段汇总（公开端点）：收集启用插件的 ClientInjection。
// 单个插件 panic 隔离跳过；内容为站点主自装插件代码，不过滤（见 capability 注释）。
func (h *AdminPlugins) Injections(w http.ResponseWriter, r *http.Request) {
	out := []plugin.ClientInjection{}
	for _, p := range plugin.All() {
		name := p.Info().Name
		if !plugin.Enabled(name) {
			continue
		}
		cp, ok := p.(plugin.ClientInjectionProvider)
		if !ok {
			continue
		}
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					log.Printf("[plugin] 插件 %s 注入片段取数崩溃: %v", name, rec)
				}
			}()
			for _, inj := range cp.ClientInjections() {
				if inj.HTML == "" || (inj.Position != "head" && inj.Position != "body_bottom") {
					continue
				}
				out = append(out, inj)
			}
		}()
	}
	writeJSON(w, map[string]any{"ok": 1, "injections": out})
}

// ClientList 前台插件菜单：启用且声明了前台页的插件（登录用户）。
func (h *AdminPlugins) ClientList(w http.ResponseWriter, r *http.Request) {
	sess := middleware.FromSession(r.Context())
	if sess == nil || sess.UserID <= 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"ok":0,"msg":"请先登录"}`))
		return
	}
	out := []map[string]string{}
	for _, p := range plugin.All() {
		name := p.Info().Name
		if !plugin.Enabled(name) {
			continue
		}
		cp, ok := p.(plugin.ClientPageProvider)
		if !ok {
			continue
		}
		m := cp.ClientPage()
		to := m.To
		if to == "" {
			to = "/plugin/" + name
		}
		out = append(out, map[string]string{"name": name, "title": m.Title, "icon": m.Icon, "to": to})
	}
	writeJSON(w, map[string]any{"ok": 1, "plugins": out})
}
