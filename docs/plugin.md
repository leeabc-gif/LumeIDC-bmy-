# LumeIDC 业务插件开发指南

面向「带事件订阅/建表/路由/页面的完整功能」插件。如果只做支付网关、上游供应商、短信/邮件渠道这类
**单能力接口**，走各自的注册表即可，见 [provider.md](provider.md)（供应商）与本文「接口类扩展」一节。

## 一、最小插件（5 分钟跑通）

```go
// internal/plugins/hello/plugin.go
package hello

import (
    "context"

    "lumeidc/internal/plugin"
)

type Plugin struct{ host *plugin.Host }

func (p *Plugin) Info() plugin.Info {
    return plugin.Info{
        Name:        "hello",        // 唯一标识，与目录同名，小写蛇形
        Title:       "你好插件",
        Version:     "1.0.0",
        Description: "最小示例。",
    }
}

func (p *Plugin) Init(h *plugin.Host) error {
    p.host = h
    // 订阅事件：订单支付成功后干点什么
    h.Subscribe(plugin.EventOrderPaid, func(ctx context.Context, payload any) error {
        // payload 按事件约定断言，如 plugin.OrderPaidPayload
        return nil
    })
    return nil
}

func init() { plugin.Register(&Plugin{}) }
```

接线最多两处（组合根零改动）：

```go
// internal/plugins/all/all.go —— 加一行（必有）
_ "lumeidc/internal/plugins/hello"
```

若插件带后台/前台页面，再到 `web/src/plugins/registry.ts` 的
`adminRegistry`/`clientRegistry` 加一行组件映射（纯 API/事件类插件不需要）。

构建启动后，后台「系统设置 → 插件管理」即可看到并可启停。

## 二、能力总览（类型断言探测，实现即生效，无需全实现）

| 接口 | 获得 |
|---|---|
| `Migrator` | 启动时执行 `migrations/*.sql`（version 前缀 `plugin/{name}/` 隔离） |
| `AdminRouteRegistrar` | 管理 API 子 mux（挂 `/admin/plugin/{name}/` 前缀，写相对路径） |
| `ClientRouteRegistrar` | 用户侧 API 子 mux（挂 `/plugin/{name}/` 前缀） |
| `ConfigSchemaProvider` | 后台自动配置表单页 + 统一 config API（零前端零 handler） |
| `AdminMenuProvider` | 后台侧栏菜单项（可选挂业务分组） |
| `ClientPageProvider` | 前台用户中心菜单项 |
| `AdminWidgetProvider` | 后台首页挂件卡片（JSON，前端统一渲染） |
| `CronContributor` | 定时任务（失败走管理员告警，与核心任务同待遇） |
| `ClientInjectionProvider` | 前台页面注入片段（统计代码/客服悬浮窗） |

## 三、配置系统（推荐首选）

声明 schema 即获得配置页，免写 Vue/handler：

```go
func (p *Plugin) ConfigSchema() []plugin.ConfigField {
    return []plugin.ConfigField{
        {Key: "enabled", Title: "启用", Type: "switch"},
        {Key: "url", Title: "Webhook URL", Type: "text", Tip: "事件发生时 POST"},
        {Key: "secret", Title: "签名密钥", Type: "password"},            // 不回显，has_ 掩码
        {Key: "hour", Title: "发送时刻", Type: "select", Options: hours}, // Options 保序
        {Key: "events", Title: "订阅事件", Type: "multiselect", OptionsRef: "plugin.events"},
        {Key: "note", Title: "备注", Type: "textarea"},
    }
}
```

- 类型：`text | number | password | switch | select | multiselect | textarea`
- 存储：settings 表，键 `plugin.{name}.{key}`（multiselect 存 JSON 数组字符串）
- 读取：`p.host.Config(ctx, "url")`（缺失返回空串）
- `OptionsRef: "plugin.events"`：选项动态取事件目录（含其他插件注册的事件）

## 四、事件与过滤器

### 订阅核心事件

```go
h.Subscribe(plugin.EventServiceSuspended, p.onSuspend)  // 指定事件
h.Subscribe("*", p.onAny)                                // 通配全部（含未来插件注册的）
// handler 内取事件名：plugin.EventName(ctx)
```

事件目录（`plugin.EventCatalog()` 含中文标签，后台配置页自动列出）：
`order.created / order.paid / invoice.expired / refund.created /
service.created / service.suspended / service.unsuspended / service.renewed /
service.terminated / service.expiring / promotion.ending /
user.registered / user.login / identity.submitted / identity.reviewed`，
以及插件注册的 `ticket.opened / ticket.replied`（工单）、
`refund_request.created / refund_request.approved / refund_request.rejected`（退款）、
`violation.created / violation.removed`（违规）。
payload 类型见 [events.go](../internal/plugin/events.go)。

**语义**：Emit 同步广播；单个订阅者 panic/出错仅记日志，不阻断主流程；插件禁用后不投递。
handler 里别做慢事——网络投递请 `go func()`（参考 webhooknotify 的 deliver）。

### 过滤器（可改数据）

```go
h.SubscribeFilter(plugin.FilterNotifyMessage, func(ctx context.Context, v any) (any, error) {
    msg := v.(*plugin.NotificationMessage)
    msg.Body += "\n——本消息由 XX 系统发出"
    return msg, nil
})
```

链式执行；某环出错/禁用则保留当前值继续。

### 插件注册自定义事件

```go
plugin.RegisterEvent("myplugin.something", "某事发生")  // init 或 Init 中
plugin.Emit(ctx, "myplugin.something", payload)          // 触发
```

webhooknotify 等订阅方的配置页会自动出现该事件。

## 五、数据表（Migrator）

```go
//go:embed migrations/*.sql
var migrationsFS embed.FS
func (p *Plugin) Migrations() fs.FS { return migrationsFS }
```

约定：表名带插件前缀（`plugin_myplugin_xxx`），全部语句幂等
（`CREATE TABLE IF NOT EXISTS` / `ADD COLUMN IF NOT EXISTS` / `ON CONFLICT DO NOTHING`），
新装与老库升级都安全。插件迁移在核心迁移后、Init 前执行，与核心 version 序列隔离。
**插件禁用不影响表与数据。**

## 六、路由与鉴权

```go
func (p *Plugin) RegisterAdminRoutes(mux *http.ServeMux) {
    mux.HandleFunc("GET /logs", p.logs)   // → /admin/plugin/{name}/logs
}
func (p *Plugin) RegisterClientRoutes(mux *http.ServeMux) {
    mux.HandleFunc("GET /list", p.list)   // → /plugin/{name}/list
}
```

- 子 mux 写**相对路径**；框架统一挂前缀并包启用态闸门（禁用即 404）
- 方法限制：管理侧只挂 GET/POST/DELETE，用户侧只挂 GET/POST——PUT/PATCH 会 405，
  写 handler 时就用这三种方法表达动作（如 `POST /{id}/close`）
- 鉴权 helper：`plugin.AdminOK(w, r)` / `plugin.AdminSession(w, r)`（要管理员 ID 时）/
  `plugin.RequireUserID(w, r)`
- 响应：`plugin.WriteJSON(w, {...})` 成功；`plugin.JSONFail(w, "中文原因")`（HTTP 200 + ok:0）；
  `plugin.StatusFail(w, 400, "...")`（参数错误等需状态码场景）
- 配置 API（`GET/POST /admin/plugin/{name}/config`）由框架统一提供，勿重复注册 `/config`

## 七、页面与菜单

**后台页**：`web/src/plugins/{name}/Admin.vue` + `web/src/plugins/registry.ts` 的
`adminRegistry` 加一行映射。页面路径 `/admin#/plugin/{name}`。
若同时实现了 ConfigSchema，配置表单自动渲染在自定义组件上方（参考 webhooknotify：
配置自动表单 + 自定义投递日志区）。

**后台菜单归属**：`AdminMenu()` 返回 `MenuItem{Title, Icon, Parent}`。
`Parent` 取 `plugin.MenuGroupOps / MenuGroupBusiness / MenuGroupUsers / MenuGroupSettings /
MenuGroupSystem`，空则收拢进「插件」分组。

**前台页**：实现 `ClientPageProvider`（`MenuItem.To` 可指存量路径如 `/tickets`，
空则 `/plugin/{name}` 槽位，组件进 `clientRegistry`）。

**后台挂件**：`AdminWidget(ctx) (*plugin.WidgetData, error)`——返回标题/图标/行列表，
前端统一渲染卡片（不注入 HTML）。取数失败返回 error，该挂件自动跳过。

**前台注入**：`ClientInjections() []plugin.ClientInjection`（`Position: "head" | "body_bottom"`）。
内容为站点主自装代码，不过滤不转义——勿拼接用户输入。

## 八、定时任务（CronContributor）

```go
func (p *Plugin) CronJobs() []plugin.CronJob {
    return []plugin.CronJob{{
        Name: "my_task",            // 告警去重键
        Spec: "@every 1h",          // robfig 表达式
        What: "做某事",             // 告警文案
        Run:  p.doSomething,        // func(ctx) error
    }}
}
```

插件禁用时不触发；失败经核心 failTask 给管理员告警（按 Name+日期去重）。
**配置里读出的 Spec 在启动时注册，改配置需重启生效**（参考 dailyreport）。

## 九、启停语义（软禁用，即时生效，无需重启）

| 面 | 禁用后 |
|---|---|
| 事件/过滤器 | 不再投递/执行 |
| 路由 | 404 |
| 后台/前台菜单、挂件、注入 | 不显示（后端过滤；已打开页面需刷新才消失） |
| cron | 不触发 |
| 数据表/配置 | 保留（重启用即恢复） |

注：页面壳（`/admin#/plugin/{name}`、`/plugin/{name}`）本身不感知禁用态——
菜单隐藏后手动输地址仍能打开壳，由 API 404 兜底报错，属可接受的固有局限。

## 十、故障策略与测试

- **启动期严格**：迁移失败/Init 失败 → 启动失败（编译期问题尽早暴露）
- **运行期宽容**：事件 handler/挂件/注入的 panic 隔离记日志（插件崩不影响核心）
- 单测参考：`internal/plugins/dailyreport/report_test.go`（配置分支）、
  `webhooknotify/plugin_test.go`（投递签名）、`internal/plugin/events_test.go`（总线语义）
- 提交前：`go build ./... && go vet ./... && go test ./...`，有前端改动加
  `cd web && npx vitest run && npm run build`

## 十一、现有插件参考索引

| 插件 | 示范能力 |
|---|---|
| `webhooknotify` | 事件通配订阅、配置 schema、管理路由、后台挂件、签名投递 |
| `announcement` | 迁移建表、前后台 API、核心聚合点联动（启用态控制首页新闻区显隐） |
| `tickets` | 复杂业务搬迁：前后台全套页面、cron 任务、通知模板触发、附件存储、自定义事件 |
| `refund` | 复用核心能力（Host.Refunder）、商品级规则表、自定义事件、多渠道通知 |
| `violation` | 公示页（前台公开路由）、跨插件复用公告仓储、记录生命周期事件 |
| `dailyreport` | 最薄插件：纯配置 + cron + 管理员通知，零建表零前端 |
| `spaceship` | 外部 API 客户端 + 异步注册轮询（cron）+ 前后台 CRUD + 迁移建表 + 余额扣减 |

注：dailyreport 目前直接查 tickets 插件的表统计工单数（跨插件表耦合），
tickets 禁用后日报仍会统计其历史工单——如需严格隔离请自行加启用态判断。

## 十二、接口类扩展：新增短信渠道

短信渠道与上游供应商同属**接口类扩展**（非业务插件：不出现在插件管理页、无启停态），
走 `internal/smsdk` 注册表自注册。新增一家短信商只需三步，核心零改动：

1. 在 `internal/plugins/sms/` 新建 `sms_adapter_xxx.go`（package `sms`），实现
   `smsdk.Provider` 接口（`SendSMS`，按需实现 `TemplateCreate/Query/Delete` 等）
2. 同文件内联描述符并在 `init()` 注册（重复 key 会 panic，启动期暴露冲突）：

```go
var descriptorXxx = smsdk.ProviderDescriptor{
	Key:          "xxx",
	Name:         "某某短信",
	ConfigFields: []string{"sms_access_key", "sms_secret_key"},
	Capabilities: smsdk.ProviderCapabilities{ /* 范围/审核能力声明 */ },
}

func init() { smsdk.RegisterSMSProvider(descriptorXxx, newXxxAdapter) }
```

3. 完成。`internal/plugins/all/all.go` 已聚合本包，后台「短信服务商」下拉、
   配置表单、可用性校验自动出现新渠道（均由注册表驱动，无需改 handler/前端）。

常用工具（smsdk 包）：`DoRequest`（HTTP 传输层，含 nil client 兜底/禁重定向/1MB 限制）、
`DecodeResponse`（JSON 解析保精度）、`Rejected`（供应商明确拒绝统一结果）、
`CanonicalQuery/TC3Signature/MD5Hex` 等签名函数、`PhoneForRange`（国内/国际号码规范化）。

参考实现：`sms_adapter_smsbao.go`（最简 GET 协议）、`sms_adapter_qcloudsms.go`（TC3 签名完整示例）。

## 十三、接口类扩展：新增实名渠道

实名渠道同属**接口类扩展**，走 `internal/vsdk` 注册表自注册。新增一家实名商同样三步，
核心与前端零改动：

1. 在 `internal/plugins/verify/` 新建 `verification_adapter_xxx.go`（package `verify`），实现
   `vsdk.Provider` 接口（`Key/Start/Poll`）
2. 同文件内联描述符并在 `init()` 注册。与短信不同：实名每家配置键完全不同，
   描述符需带字段 label 与 Secret 标注（Secret 字段设置页不回传、保存时留空保留旧值）：

```go
var descriptorXxx = vsdk.Descriptor{
	Key:  "xxx",
	Name: "某某实名",
	Fields: []vsdk.ConfigField{
		{Key: "verification_xxx_api_key", Label: "API Key"},
		{Key: "verification_xxx_secret_key", Label: "Secret Key", Secret: true},
	},
}

func init() { vsdk.Register(descriptorXxx, newXxxAdapter) }
```

3. 完成。后台「实名认证」设置页的服务商下拉、字段表单、保存校验全部经
   `GET /admin/verification-providers` + 注册表驱动自动生效。

适配器宿主 `vsdk.Host` 提供：`Get`（读设置）、`Endpoint`（地址 https + host 白名单校验）、
`RequestJSON/RequestForm`（HTTP 传输，nil client 兜底）；工具 `StatusFromMap`（各家状态归一化）、
`ResponseOK`、`StringValue/FirstString`、`SHA256Hex/RandomHex`。

参考实现：`verification_adapter_smapi.go`（最简）、`verification_adapter_leaf_face.go`（HMAC 签名完整示例）。

## 十四、接口类扩展：新增验证码供应商

外部人机验证同属**接口类扩展**，走 `internal/csdk` 注册表自注册。新增一家验证码商三步，
核心与前端零改动：

1. 在 `internal/plugins/captcha/` 新建 `captcha_adapter_xxx.go`（package `captcha`），实现
   `csdk.Provider` 接口（`PublicConfig` 返回浏览器侧安全配置、`Verify` 服务器端校验）
2. 同文件内联描述符并在 `init()` 注册。描述符除配置字段外还需声明前端 SDK 元数据
   （SDKURL/SDKVersion，inline 渲染的填 RenderMode/APIBaseURL）：

```go
var descriptorXxx = csdk.Descriptor{
	Key:        "xxx",
	Name:       "某某验证",
	SDKURL:     "https://cdn.example.com/sdk.js",
	SDKVersion: "v1",
	Fields: []csdk.ConfigField{
		{Key: "captcha_xxx_id", Label: "XXX ID"},
		{Key: "captcha_xxx_key", Label: "XXX Key", Secret: true},
	},
}

func init() { csdk.Register(descriptorXxx, func(h *csdk.Host) (csdk.Provider, error) { return &xxxAdapter{host: h}, nil }) }
```

3. 完成。后台「安全设置」的服务商下拉、字段表单、保存校验全部经
   `GET /admin/captcha-providers` + 注册表驱动自动生效。

`PublicConfig` 用 `描述符.BaseConfig(publicID, scene)` 构造（配置完整时置 `Enabled=true`）；
宿主 `csdk.Host` 提供 `Get`（读设置）与 `HTTPClient()`（nil client 兜底 10s），
校验响应用 `csdk.ReadResponse`（64KB 上限），签名用 `csdk.HexString`。

场景开关（哪个场景启用外部验证）是系统层逻辑（`external_captcha_*_enabled` 设置项），
不属于适配器；适配器只需关心「配置完整时给出公开配置」与「服务器端校验」两件事。

参考实现：`captcha_adapter_geetest.go`（HMAC 签名 + 表单校验）、`captcha_adapter_corptcha.go`（Bearer JSON 最简）。
