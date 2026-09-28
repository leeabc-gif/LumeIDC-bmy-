// Package tickets 工单插件：用户工单（提交/回复/关闭/附件）+ 后台工单管理
// （列表/详情/回复/状态/分配/内部备注/附件）+ 超时提醒定时任务。
// 表与核心共享（通知铃铛读 tickets 做待办；附件存核心私有根目录，存量附件无缝）。
package tickets

import (
	"embed"
	"io/fs"
	"net/http"

	"lumeidc/internal/plugin"
	"lumeidc/internal/storage"
)

// Name 插件名常量。注意：核心聚合点（铃铛工单入口）用字符串字面量按此名查启用态。
const Name = "tickets"

// 插件事件（供 webhooknotify 等订阅）。
const (
	EventTicketOpened  = "ticket.opened"
	EventTicketReplied = "ticket.replied"
)

// TicketPayload 工单事件载荷。
type TicketPayload struct {
	TicketID int64  `json:"ticketId"`
	UserID   int64  `json:"userId"`
	Subject  string `json:"subject"`
	ByAdmin  bool   `json:"byAdmin"` // ticket.replied：是否客服回复
}

func init() {
	plugin.RegisterEvent(EventTicketOpened, "工单新建")
	plugin.RegisterEvent(EventTicketReplied, "工单新回复")
	plugin.Register(&Plugin{})
}

type Plugin struct {
	host  *plugin.Host
	files *storage.PrivateFiles
}

func (p *Plugin) Info() plugin.Info {
	return plugin.Info{
		Name:        Name,
		Title:       "工单支持",
		Version:     "1.0.0",
		Description: "用户工单：提交/回复/状态流转/附件/客服分配/超时提醒。",
	}
}

func (p *Plugin) Init(h *plugin.Host) error {
	p.host = h
	// 附件与核心同根目录（PrivateDataDir）：存量附件的 storage_ref 无需迁移。
	p.files = &storage.PrivateFiles{Root: h.PrivateRoot}
	return nil
}

//go:embed migrations/*.sql
var migrationsFS embed.FS

func (p *Plugin) Migrations() fs.FS { return migrationsFS }

func (p *Plugin) AdminMenu() plugin.MenuItem {
	return plugin.MenuItem{Title: "工单管理", Icon: "ri:customer-service-2-line", Parent: plugin.MenuGroupUsers}
}

// ClientPage 前台用户中心菜单；工单页路径保留 /tickets（存量路径不变）。
func (p *Plugin) ClientPage() plugin.MenuItem {
	return plugin.MenuItem{Title: "工单支持", Icon: "ri:customer-service-2-line", To: "/tickets"}
}

// RegisterClientRoutes 用户侧 API（/plugin/tickets/ 下，登录用户）。
func (p *Plugin) RegisterClientRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /list", p.clientList)
	mux.HandleFunc("POST /create", p.clientCreate)
	mux.HandleFunc("GET /{ticketID}", p.clientDetail)
	mux.HandleFunc("POST /{ticketID}/reply", p.clientReply)
	mux.HandleFunc("POST /{ticketID}/close", p.clientClose)
	mux.HandleFunc("POST /{ticketID}/reopen", p.clientReopen)
	mux.HandleFunc("POST /{ticketID}/attachments", p.clientAttachment)
	mux.HandleFunc("GET /{ticketID}/attachments/{attachmentID}", p.clientAttachmentDownload)
}

// RegisterAdminRoutes 管理 API（/admin/plugin/tickets/ 下）。
func (p *Plugin) RegisterAdminRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /list", p.adminList)
	mux.HandleFunc("GET /stats", p.adminStats)
	mux.HandleFunc("GET /assignees", p.adminAssignees)
	mux.HandleFunc("GET /{ticketID}", p.adminDetail)
	mux.HandleFunc("POST /{ticketID}/reply", p.adminReply)
	mux.HandleFunc("POST /{ticketID}/status", p.adminStatus)
	mux.HandleFunc("POST /{ticketID}/assign", p.adminAssign)
	mux.HandleFunc("POST /{ticketID}/internal-note", p.adminInternalNote)
	mux.HandleFunc("POST /{ticketID}/attachments", p.adminAttachment)
	mux.HandleFunc("GET /{ticketID}/attachments/{attachmentID}", p.adminAttachmentDownload)
}
