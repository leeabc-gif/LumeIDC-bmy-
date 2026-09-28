// Package announcement 系统公告插件：公告管理（后台 CRUD）+ 前台公告中心 API。
// 表与核心共享（repo.Announcements 留核心：首页启动数据/用户中心/导航 megamenu 等
// 聚合点只读使用；写路径由本插件独占）。禁用后：管理与公告中心 API 404、
// 核心聚合点返回空（前台新闻区/用户中心公告自动隐藏）。
package announcement

import (
	"embed"
	"io/fs"
	"net/http"

	"lumeidc/internal/plugin"
	"lumeidc/internal/repo"
)

// Name 插件名常量。注意：核心聚合点（首页新闻区/铃铛）用字符串字面量按此名查启用态。
const Name = "announcement"

type Plugin struct {
	host  *plugin.Host
	store *repo.Announcements
}

func (p *Plugin) Info() plugin.Info {
	return plugin.Info{
		Name:        Name,
		Title:       "系统公告",
		Version:     "1.0.0",
		Description: "站点公告管理：分类、摘要、封面、置顶、阅读量；前台公告中心与首页新闻区数据源。",
	}
}

func (p *Plugin) Init(h *plugin.Host) error {
	p.host = h
	p.store = repo.NewAnnouncements(h.DB)
	return nil
}

//go:embed migrations/*.sql
var migrationsFS embed.FS

func (p *Plugin) Migrations() fs.FS { return migrationsFS }

func (p *Plugin) AdminMenu() plugin.MenuItem {
	return plugin.MenuItem{Title: "系统公告", Icon: "ri:notification-3-line", Parent: plugin.MenuGroupUsers}
}

// 公告前台页是公开页 /notices（非用户中心页），故不实现 ClientPageProvider；
// 前台导航「公告」为静态入口，插件禁用时空列表由页面空态呈现。

// RegisterAdminRoutes 管理 API（/admin/plugin/announcement/ 下）。
func (p *Plugin) RegisterAdminRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /list", p.adminList)
	mux.HandleFunc("GET /form", p.adminForm)
	mux.HandleFunc("POST /save", p.adminSave)
	mux.HandleFunc("POST /{id}/delete", p.adminDelete)
}

// RegisterClientRoutes 前台公告 API（/plugin/announcement/ 下，无需登录——公告对外公开）。
func (p *Plugin) RegisterClientRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /list", p.clientList)
	mux.HandleFunc("GET /detail/{id}", p.clientDetail)
}

func init() { plugin.Register(&Plugin{}) }
