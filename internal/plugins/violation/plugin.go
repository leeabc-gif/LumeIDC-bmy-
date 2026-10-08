// Package violation 用户违规管理插件：后台违规记录管理（列表/添加）+ 违规公示公告，
// 前台用户中心「违规公示」页。违规处置仅记录与公示，不联动账户或服务状态。
package violation

import (
	"context"
	"embed"
	"io/fs"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"lumeidc/internal/plugin"
)

const Name = "violation"

// 插件事件（供 webhooknotify 等订阅）。
const (
	EventViolationCreated = "violation.created"
	EventViolationRemoved = "violation.removed"
)

// ViolationPayload 违规事件负载。
type ViolationPayload struct {
	RecordID int64  `json:"recordId"`
	UserID   int64  `json:"userId"`
	Type     string `json:"type"`
	Level    string `json:"level"`
}

// 违规等级固定枚举（存储值 → 中文标签）。
var levelLabels = map[string]string{
	"light":  "轻微",
	"medium": "中度",
	"severe": "严重",
}

func init() {
	plugin.RegisterEvent(EventViolationCreated, "新增违规记录")
	plugin.RegisterEvent(EventViolationRemoved, "删除违规记录")
	plugin.Register(&Plugin{})
}

// Plugin 用户违规管理插件。
type Plugin struct {
	host    *plugin.Host
	records *Records
	anns    *Announcements
	users   *Users
	// adminLimiter 管理端危险操作限流器（按 adminID|IP|op 三重键；懒加载）。
	// 防脚本批量点击 / 误操作刷量；纯内存无外部依赖，语义见 plugin.RateLimiter。
	adminLimiterOnce sync.Once
	adminLimiter     *plugin.RateLimiter
}

func (p *Plugin) limiter() *plugin.RateLimiter {
	p.adminLimiterOnce.Do(func() {
		p.adminLimiter = plugin.NewRateLimiter()
	})
	return p.adminLimiter
}

// checkAdminRate 管理端危险操作统一节流闸门（保存/删除等共用）。
// 限流强度取插件配置 adminOpRatePerMin（<=0 表示关闭，本地开发/单管理员机房）；
// 键为 <adminID>|<ip>|<op>，限额语义详见 plugin.RateLimiter.AllowAdmin。
func (p *Plugin) checkAdminRate(r *http.Request, adminID int64, op string) (bool, time.Duration) {
	rate := 30
	if v := strings.TrimSpace(p.host.Config(r.Context(), "adminOpRatePerMin")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			rate = n
		}
	}
	return p.limiter().AllowAdmin(r, rate, adminID, op)
}

func (p *Plugin) Info() plugin.Info {
	return plugin.Info{
		Name:        Name,
		Title:       "用户违规管理",
		Version:     "1.0.0",
		Description: "记录用户违规（类型/等级/处置措施/有效期）并选择性前台公示；附违规公示公告管理。仅记录与公示，不联动账户或服务状态。",
	}
}

func (p *Plugin) Init(h *plugin.Host) error {
	p.host = h
	p.records = NewRecords(h.DB)
	p.anns = NewAnnouncements(h.DB)
	p.users = NewUsers(h.DB)
	return nil
}

//go:embed migrations/*.sql
var migrationsFS embed.FS

func (p *Plugin) Migrations() fs.FS { return migrationsFS }

func (p *Plugin) AdminMenu() plugin.MenuItem {
	return plugin.MenuItem{Title: "违规管理", Icon: "ri:alert-line", Parent: plugin.MenuGroupUsers}
}

func (p *Plugin) ClientPage() plugin.MenuItem {
	return plugin.MenuItem{Title: "违规公示", Icon: "ri:alert-line", To: "/plugin/violation"}
}

func (p *Plugin) ConfigSchema() []plugin.ConfigField {
	return []plugin.ConfigField{
		{Key: "typeOptions", Title: "违规类型选项", Type: "textarea",
			Default: "滥用资源\n垃圾邮件\n欺诈行为\n侵犯版权\n其他", Tip: "每行一个类型"},
		{Key: "actionOptions", Title: "处置措施选项", Type: "textarea",
			Default: "警告\n限制功能\n暂停服务\n停用账户", Tip: "每行一个措施；仅记录展示，不联动账户状态"},
		{Key: "defaultPublic", Title: "新增时默认公示", Type: "switch", Default: "0",
			Tip: "添加违规表单中「是否公示」的默认值"},
		{Key: "notifyUser", Title: "通知用户", Type: "switch", Default: "1",
			Tip: "新增/实质变更违规记录时向对应用户发送站内信；开启邮件转发的站点将同步补发"},
		{Key: "adminOpRatePerMin", Title: "管理端操作限流（次/分钟）", Type: "number", Default: "30",
			Tip: "保存/删除等危险操作按 <管理员>|<IP>|<操作> 限流；0 表示关闭（本地开发/单管理员环境）"},
	}
}

// cfgBool 开关配置解析：空串（未配置）回退默认值，""/"0"/"false" 为 false，其余 true。
func (p *Plugin) cfgBool(ctx context.Context, key string, def bool) bool {
	v := strings.TrimSpace(p.host.Config(ctx, key))
	if v == "" {
		return def
	}
	return parseBool(v)
}

func (p *Plugin) RegisterAdminRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /list", p.adminList)
	mux.HandleFunc("GET /stats", p.adminStats)
	mux.HandleFunc("GET /users/search", p.adminUserSearch)
	mux.HandleFunc("GET /form", p.adminForm)
	mux.HandleFunc("POST /save", p.adminSave)
	mux.HandleFunc("POST /{id}/delete", p.adminDelete)
	mux.HandleFunc("GET /announcements", p.adminAnnouncements)
	mux.HandleFunc("POST /announcements/save", p.adminAnnouncementSave)
	mux.HandleFunc("POST /announcements/{id}/delete", p.adminAnnouncementDelete)
}

func (p *Plugin) RegisterClientRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /feed", p.clientFeed)
	mux.HandleFunc("GET /announcement/{id}", p.clientAnnouncement)
	mux.HandleFunc("GET /record/{id}", p.clientRecord)
}

// splitLines 解析多行文本配置为选项（去首尾空白与空行，保持顺序去重）。
func splitLines(s string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, 8)
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || seen[line] {
			continue
		}
		seen[line] = true
		out = append(out, line)
	}
	return out
}

// maskUser 前台脱敏展示：有昵称显示昵称，否则「用户#id」；不外泄 email。
func maskUser(name string, id int64) string {
	if n := strings.TrimSpace(name); n != "" {
		return n
	}
	return "用户#" + strconv.FormatInt(id, 10)
}
