// Package spaceship Spaceship（Namecheap 旗下）域名注册插件。
// 提供域名可用性查询、联系人管理、注册（含异步轮询）、自动续费开关等能力。
// 走纯业务插件路径（plugin.Plugin），不改动 Provider 接口。
package spaceship

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"strconv"
	"strings"
	"time"

	"lumeidc/internal/plugin"
)

const Name = "spaceship"

// 价目表单条价格上限（分）：与余额 NUMERIC(12,2) 的取值空间保持一致，
// 防止后台误填天文数字导致扣款溢出。
const maxPriceCents = 999999999999

// 插件配置默认值（框架配置页只在保存后写 settings，未保存时 Config 返回空串，
// 故读取处按此兜底，保证与配置页展示的默认值一致）。
const (
	defPollIntervalSec = "30"
	defDefaultYears    = "1"
	defPrivacyLevel    = "high"
	defEnableNotify    = "1"
	defAllowPremium    = "0"
	defRefundOnFailure = "1"
)

// defStuckAfter 异步操作"卡死"阈值：started_at 距今超过此值的 pending 操作，
// ListPending 会主动丢弃，pollOne 也会把它标 failed 并退款（防止资金长期悬挂）。
// 同时 adminRetryOperation 用同一阈值判断"是否需要 resurrect"（started_at 复位）。
// 该常量与 SQL 里的 `interval '15 minutes'` 同源；改这里请同步 repo.go:ListPending。
const defStuckAfter = 15 * time.Minute

// alertCategorySpaceshipPoll 管理员告警分类（NotifyAdminOnce 的 category 字段），
// 同类告警会在告警聚合面板归到一起。
const alertCategorySpaceshipPoll = "spaceship-poll"

func init() {
	plugin.Register(&Plugin{})
}

// Plugin Spaceship 域名注册插件。
type Plugin struct {
	host *plugin.Host
	repo repoStore
}

// repoStore Spaceship 数据访问接口。*Repo 为唯一生产实现（Init 注入）；
// 轮询等逻辑经此接口可在单测中注入内存假实现（无 DB 依赖）。
type repoStore interface {
	SaveContact(ctx context.Context, c *ContactRow) (*ContactRow, error)
	GetContact(ctx context.Context, id int64) (*ContactRow, error)
	ListContacts(ctx context.Context, userID sql.NullInt64) ([]*ContactRow, error)
	DeleteContact(ctx context.Context, id int64) error
	SetDefaultContact(ctx context.Context, userID sql.NullInt64, id int64) error
	GetDefaultContact(ctx context.Context, userID sql.NullInt64) (*ContactRow, error)
	CreateDomain(ctx context.Context, d *DomainRow) (int64, error)
	GetDomain(ctx context.Context, id int64) (*DomainRow, error)
	GetDomainByName(ctx context.Context, domain string) (*DomainRow, error)
	ListDomains(ctx context.Context, userID sql.NullInt64) ([]*DomainRow, error)
	UpdateDomain(ctx context.Context, id int64, patches map[string]any) error
	SetRegistered(ctx context.Context, id int64, spaceshipDomainID string, registeredAt, expiresAt time.Time) error
	CreateOperation(ctx context.Context, op *OperationRow) (int64, error)
	GetOperation(ctx context.Context, id int64) (*OperationRow, error)
	ListPending(ctx context.Context, limit int) ([]*OperationRow, error)
	MarkSuccess(ctx context.Context, id int64, result any) error
	MarkFailed(ctx context.Context, id int64, errMsg string, result any) error
	MarkPendingRetried(ctx context.Context, id int64) (bool, error)
	UpdateOpDomain(ctx context.Context, opID, domainID int64) error
	ListOperations(ctx context.Context, limit, offset int) ([]*OperationRow, error)
	CountOperations(ctx context.Context) (int, error)
	GetPrice(ctx context.Context, tld string) (*PriceRow, error)
	ListPrices(ctx context.Context) ([]*PriceRow, error)
	UpsertPrice(ctx context.Context, row *PriceRow) (*PriceRow, error)
	DeletePrice(ctx context.Context, tld string) error
	CreateOrder(ctx context.Context, o *OrderRow) (int64, error)
	MarkOrderRefunded(ctx context.Context, id int64) error
	LatestPaidOrder(ctx context.Context, domainID int64, kind string) (*OrderRow, error)
	ListOrders(ctx context.Context, userID sql.NullInt64, limit, offset int) ([]*OrderRow, error)
	WithTx(tx *sql.Tx) repoStore
}

func (p *Plugin) Info() plugin.Info {
	return plugin.Info{
		Name:        Name,
		Title:       "域名注册",
		Version:     "1.0.0",
		Description: "Spaceship 域名注册插件：可用性查询、联系人管理、域名注册（异步轮询）、自动续费开关。",
	}
}

func (p *Plugin) Init(h *plugin.Host) error {
	p.host = h
	p.repo = NewRepo(h.DB)
	return nil
}

//go:embed migrations/*.sql
var migrationsFS embed.FS

func (p *Plugin) Migrations() fs.FS { return migrationsFS }

func (p *Plugin) AdminMenu() plugin.MenuItem {
	return plugin.MenuItem{Title: "域名注册", Icon: "ri:global-line", Parent: plugin.MenuGroupBusiness}
}

func (p *Plugin) ClientPage() plugin.MenuItem {
	return plugin.MenuItem{Title: "我的域名", Icon: "ri:global-line", To: "/plugin/spaceship"}
}

// ConfigSchema 插件配置字段（框架自动渲染后台配置页，读写走 settings 表 plugin.spaceship.*）。
func (p *Plugin) ConfigSchema() []plugin.ConfigField {
	return []plugin.ConfigField{
		{Key: "apiKey", Title: "Spaceship API Key", Type: "text",
			Tip: "在 https://www.spaceship.com/application/api-manager/ 创建 API Key（权限范围至少勾 domains:read + domains:billing + contacts:write + contacts:read + asyncoperations:read）"},
		{Key: "apiSecret", Title: "Spaceship API Secret", Type: "password",
			Tip: "API Manager 生成时只显示一次，务必保存"},
		{Key: "defaultYears", Title: "默认注册年限", Type: "number", Default: defDefaultYears,
			Tip: "用户前台注册时的默认年限（1-10 年）"},
		// 官方文档（docs.spaceship.dev）隐私保护仅 high / public 两档，
		// 传入 medium/low 会被上游判 400，故不再提供。
		{Key: "privacyLevel", Title: "默认隐私保护等级", Type: "select", Default: defPrivacyLevel,
			Tip: "注册域名时的 WHOIS 隐私保护等级，官方仅支持 high 与 public",
			Options: []plugin.ConfigOption{
				{Value: "high", Label: "高（推荐，WHOIS 全隐藏）"},
				{Value: "public", Label: "公开（WHOIS 可见注册人信息）"},
			}},
		{Key: "pollIntervalSec", Title: "异步轮询间隔（秒）", Type: "number", Default: defPollIntervalSec,
			Tip: "Cron 定时任务查询 Spaceship 异步操作状态的间隔"},
		{Key: "enableNotify", Title: "注册/续费/失败通知管理员", Type: "switch", Default: defEnableNotify},
		// 溢价域名默认不允许售卖：溢价价格由上游按注册局实时给出，
		// 未确认即成交容易卖穿成本，故默认关闭，开启后仍需管理员逐单确认报价。
		{Key: "allowPremium", Title: "允许售卖溢价域名", Type: "switch", Default: defAllowPremium,
			Tip: "关闭时溢价域名一律拒绝注册；开启后仍需在注册前确认上游溢价报价"},
		{Key: "refundOnFailure", Title: "注册失败自动退款", Type: "switch", Default: defRefundOnFailure,
			Tip: "上游异步注册失败时自动把已扣金额退回用户余额；关闭则保留扣款由人工处理"},
	}
}

// RegisterAdminRoutes 后台管理路由。
func (p *Plugin) RegisterAdminRoutes(mux *http.ServeMux) {
	// 配置页自带 GET/POST /config（框架自动）
	// 联系人
	mux.HandleFunc("GET /contacts", p.adminListContacts)
	mux.HandleFunc("POST /contacts", p.adminSaveContact)
	mux.HandleFunc("DELETE /contacts/{id}", p.adminDeleteContact)
	mux.HandleFunc("POST /contacts/{id}/default", p.adminSetDefaultContact)
	// 价目表（商业运营定价）
	mux.HandleFunc("GET /prices", p.adminListPrices)
	mux.HandleFunc("POST /prices", p.adminSavePrice)
	mux.HandleFunc("DELETE /prices/{tld}", p.adminDeletePrice)
	// 域名
	mux.HandleFunc("GET /domains", p.adminListDomains)
	mux.HandleFunc("POST /domains/register", p.adminRegisterDomain)
	mux.HandleFunc("GET /domains/{id}", p.adminGetDomain)
	mux.HandleFunc("POST /domains/{id}/renew", p.adminRenewDomain)
	mux.HandleFunc("POST /domains/{id}/autorenew", p.adminAutoRenew)
	mux.HandleFunc("POST /domains/{id}/privacy", p.adminPrivacy)
	mux.HandleFunc("POST /domains/{id}/delete", p.adminDeleteDomain)
	// 操作
	mux.HandleFunc("GET /operations", p.adminListOperations)
	mux.HandleFunc("POST /operations/{id}/retry", p.adminRetryOperation)
	// 订单/对账
	mux.HandleFunc("GET /orders", p.adminListOrders)
	// 溢价域名报价确认（管理员确认价格后才能代注册）
	mux.HandleFunc("POST /check", p.adminCheck)
	// 连通性测试
	mux.HandleFunc("POST /client/test", p.adminTestConnection)
}

// RegisterClientRoutes 前台用户路由。
func (p *Plugin) RegisterClientRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /check", p.clientCheck)
	mux.HandleFunc("POST /register", p.clientRegister)
	mux.HandleFunc("GET /my", p.clientMyDomains)
	mux.HandleFunc("POST /my/{id}/renew", p.clientRenew)
	mux.HandleFunc("POST /my/{id}/autorenew", p.clientAutoRenew)
	mux.HandleFunc("GET /my/orders", p.clientMyOrders)
	mux.HandleFunc("GET /prices", p.clientPrices)
	mux.HandleFunc("GET /my/contacts", p.clientMyContacts)
	mux.HandleFunc("POST /my/contacts", p.clientSaveContact)
	mux.HandleFunc("POST /my/contacts/{id}/delete", p.clientDeleteContact)
	mux.HandleFunc("POST /my/contacts/{id}/default", p.clientSetDefaultContact)
}

// ---- 配置读取（空值兜底默认） ----

func (p *Plugin) cfg(ctx context.Context, key, def string) string {
	if v := strings.TrimSpace(p.host.Config(ctx, key)); v != "" {
		return v
	}
	return def
}

func (p *Plugin) cfgInt(ctx context.Context, key string, def int) int {
	n, err := strconv.Atoi(p.cfg(ctx, key, strconv.Itoa(def)))
	if err != nil || n < 0 {
		return def
	}
	return n
}

func (p *Plugin) cfgBool(ctx context.Context, key string, def bool) bool {
	v := strings.TrimSpace(p.host.Config(ctx, key))
	if v == "" {
		return def
	}
	return v == "1" || strings.EqualFold(v, "true")
}

// cfgClient 惰性获取 Spaceship API 客户端（从 Host.Config 读 Key/Secret）。
// 未配置或 Key/Secret 为空返回 nil，调用方应跳过操作或返回"未配置"提示。
func (p *Plugin) cfgClient(ctx context.Context) *Client {
	k := strings.TrimSpace(p.host.Config(ctx, "apiKey"))
	s := strings.TrimSpace(p.host.Config(ctx, "apiSecret"))
	if k == "" || s == "" {
		return nil
	}
	return newClient(k, s)
}

// ---- Cron 轮询（CronContributor 可选接口） ----

func (p *Plugin) CronJobs() []plugin.CronJob {
	interval := p.cfgInt(context.Background(), "pollIntervalSec", 30)
	if interval < 5 {
		interval = 5
	}
	spec := fmt.Sprintf("@every %ds", interval)
	return []plugin.CronJob{{
		Name: "spaceship-poll-operations",
		Spec: spec,
		What: "Spaceship 异步操作轮询（每 " + strconv.Itoa(interval) + " 秒）",
		Run:  p.pollOperations,
	}}
}

// pollOperations 每轮最多处理 10 条 pending 操作；超过 15 分钟仍 pending 标 stuck。
func (p *Plugin) pollOperations(ctx context.Context) error {
	c := p.cfgClient(ctx)
	if c == nil {
		return nil // 未配置，跳过
	}
	return p.pollBatch(ctx, c)
}

// pollBatch 取一批 pending 逐条轮询；单条失败不阻断其他。
// 客户端由参数注入（pollOperations 传 cfgClient；单测可传指向假服务的客户端）。
func (p *Plugin) pollBatch(ctx context.Context, c *Client) error {
	ops, err := p.repo.ListPending(ctx, 10)
	if err != nil {
		return fmt.Errorf("ListPending: %w", err)
	}
	for _, op := range ops {
		if err := p.pollOne(ctx, c, op); err != nil {
			// 单条失败不阻断其他
			continue
		}
	}
	return nil
}

func (p *Plugin) pollOne(ctx context.Context, c *Client, op *OperationRow) error {
	o, err := c.GetOperation(ctx, op.OperationID)
	if err != nil {
		if IsRateLimited(err) {
			return nil // 限流跳过，等下一轮
		}
		return err
	}
	switch o.Status {
	case "success":
		_ = p.repo.MarkSuccess(ctx, op.ID, o.Details)
		// 注册成功：补 spaceship_domain_id / registered_at / expires_at
		if op.OpType == "domain_create" && op.DomainID.Valid {
			patch := map[string]any{"status": "active", "updated_at": time.Now()}
			if d, ok := o.Details.(map[string]any); ok {
				if did, ok := d["domainId"]; ok {
					patch["spaceship_domain_id"] = fmt.Sprint(did)
				}
				if ra, ok := d["createdAt"].(string); ok {
					if t, e := time.Parse(time.RFC3339, ra); e == nil {
						patch["registered_at"] = t
					}
				}
				if ea, ok := d["expiresAt"].(string); ok {
					if t, e := time.Parse(time.RFC3339, ea); e == nil {
						patch["expires_at"] = t
					}
				}
			}
			_ = p.repo.UpdateDomain(ctx, op.DomainID.Int64, patch)
		}
		// 终态告警走 NotifyAdminOnce：alertKey 含 operationID，同一笔操作多次轮询
		// 只发一次；不同操作各自一次；管理员聚合面板按 alertCategorySpaceshipPoll 归类。
		if p.cfgBool(ctx, "enableNotify", true) && p.host.Notify != nil {
			alertKey := fmt.Sprintf("spaceship.poll.success:%s:%s", op.OpType, op.OperationID)
			body := fmt.Sprintf("域名 %s 已成功注册（操作 %s）", op.Domain, op.OperationID)
			_ = p.host.Notify.NotifyAdminOnce(ctx, alertKey, alertCategorySpaceshipPoll, "域名注册成功", body)
		}
	case "failed":
		errMsg := o.Error
		if errMsg == "" {
			if d, ok := o.Details.(map[string]any); ok {
				errMsg, _ = d["error"].(string)
				if errMsg == "" {
					errMsg, _ = d["message"].(string)
				}
			}
		}
		if errMsg == "" {
			errMsg = "Spaceship 返回失败"
		}
		_ = p.repo.MarkFailed(ctx, op.ID, errMsg, o.Details)
		// 注册/续费失败：已扣的钱必须自动退回（幂等，靠订单 status 判定）。
		// refundOnFailure 关闭时保留扣款，由管理员人工对账后再处理（默认开启）。
		refunded := false
		if p.cfgBool(ctx, "refundOnFailure", true) {
			p.refundByDomainOperation(ctx, op)
			refunded = true
		}
		// 失败告警必须能让管理员当天就看到（避免"钱已退用户不知道"），按 operationID 去重。
		if p.cfgBool(ctx, "enableNotify", true) && p.host.Notify != nil {
			what := "域名注册失败"
			if op.OpType == "domain_renew" {
				what = "域名续费失败"
			}
			tail := "已自动退款"
			if !refunded {
				tail = "未自动退款（refundOnFailure 已关闭，需人工处理）"
			}
			alertKey := fmt.Sprintf("spaceship.poll.failed:%s:%s", op.OpType, op.OperationID)
			_ = p.host.Notify.NotifyAdminOnce(ctx, alertKey, alertCategorySpaceshipPoll, what,
				fmt.Sprintf("域名 %s %s：%s（操作 %s，%s）", op.Domain, what, errMsg, op.OperationID, tail))
		}
	case "pending":
		// 超过 defStuckAfter 仍 pending → 标记 failed 并退款（防止资金长期悬挂）。
		// 与 failed 分支同规则：钱先退回用户，管理员再对账上游是否真的注册成功。
		if time.Since(op.StartedAt) > defStuckAfter {
			_ = p.repo.MarkFailed(ctx, op.ID, "轮询超时（>15min），需管理员对账", nil)
			if p.cfgBool(ctx, "refundOnFailure", true) {
				p.refundByDomainOperation(ctx, op)
			}
			if p.cfgBool(ctx, "enableNotify", true) && p.host.Notify != nil {
				alertKey := fmt.Sprintf("spaceship.poll.timeout:%s:%s", op.OpType, op.OperationID)
				body := fmt.Sprintf("域名 %s 异步操作超过 %v 仍 pending，已按失败处理（操作 %s）",
					op.Domain, defStuckAfter, op.OperationID)
				_ = p.host.Notify.NotifyAdminOnce(ctx, alertKey, alertCategorySpaceshipPoll, "异步操作超时", body)
			}
		}
	}
	return nil
}

// ---- 纯函数（供 handler + 单测复用） ----

// yearsOK 注册年限范围校验（全局兜底，具体后缀以价目表 min_years/max_years 为准）。
func yearsOK(n int) bool { return n >= 1 && n <= 10 }

// tldOf 取域名后缀（小写、不含点）。多级后缀（co.uk/com.cn）取最后一段，
// 与价目表按后缀定价的口径一致；无点返回空串。
func tldOf(domain string) string {
	d := strings.ToLower(strings.TrimSpace(domain))
	if i := strings.LastIndex(d, "."); i >= 0 && i < len(d)-1 {
		return d[i+1:]
	}
	return ""
}

// priceOf 服务端定价：按后缀 + 年限算注册/续费金额（分）。
// 金额只由后台价目表决定，客户端传来的任何价格字段一律忽略。
// kind 取 "register" 或 "renew"。
func (p *Plugin) priceOf(ctx context.Context, domain, kind string, years int) (int64, *PriceRow, error) {
	tld := tldOf(domain)
	if tld == "" {
		return 0, nil, errors.New("域名格式不正确，无法识别后缀")
	}
	row, err := p.repo.GetPrice(ctx, tld)
	if err != nil {
		return 0, nil, err
	}
	if !row.Enabled {
		return 0, nil, ErrTLDDisabled
	}
	if years < 1 {
		years = 1
	}
	if row.MinYears > 0 && years < row.MinYears {
		return 0, nil, ErrYearsOutOfRange
	}
	if row.MaxYears > 0 && years > row.MaxYears {
		return 0, nil, ErrYearsOutOfRange
	}
	unit := row.RegisterCents
	if kind == "renew" {
		unit = row.RenewCents
	}
	if unit < 0 {
		unit = 0
	}
	return int64(years) * unit, row, nil
}
