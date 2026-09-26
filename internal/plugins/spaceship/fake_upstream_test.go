package spaceship

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"lumeidc/internal/db"
	"lumeidc/internal/middleware"
	"lumeidc/internal/money"
	"lumeidc/internal/plugin"
)

// ---- 假 Spaceship 上游（httptest） ----

// fakeUpstream 模拟 docs.spaceship.dev 的行为契约，用于在无真实 API Key 的情况下
// 覆盖注册资金链路。关键点全部按官方文档复刻：
//   - 注册 POST /v1/domains/{domain} → 202 + 响应头 spaceship-async-operationid
//   - 异步结果 GET /v1/async-operations/{id} → HTTP 恒 200，靠 body 的 status 判定
//   - 续费 POST /v1/domains/{domain}/renew → 202 + 异步头（官方真实存在）
//   - 列表 GET /v1/domains → take/skip 为 required，缺失必须 400
//   - 限流 429
type fakeUpstream struct {
	server *httptest.Server

	mu sync.Mutex
	// 可用性查询结果；key 为小写域名。nil 时默认 available。
	availability map[string]*CheckResult
	// 注册结果：域名 → 期望的异步终态（pending/success/failed）
	registerOutcome map[string]string
	// 操作状态：operationId → status（可被 SetOpStatus 改写）
	opStatus map[string]string

	// 调用留痕（断言"是否调用了上游"用）
	registerCalls []string // 被提交注册的域名（按调用顺序）
	renewCalls    []string
	lastRegister  map[string]any // 最后一次注册请求体（校验 years/privacy/contacts 必填）
	lastRenew     map[string]any
	// 自动续费开关：调用留痕 + 上游侧状态（官方字段名 isEnabled）
	autoRenewCalls []string
	lastAutoRenew  map[string]any
	autoRenewState map[string]bool
	contactCalls   int
	savedContacts  []map[string]any // 每次 PUT /v1/contacts 的请求体

	// 故障注入
	rateLimited bool   // 强制 429
	failMessage string // 非空：注册立即失败（同步 4xx）
	nextOpID    int
}

func newFakeUpstream() *fakeUpstream {
	f := &fakeUpstream{
		availability:    map[string]*CheckResult{},
		registerOutcome: map[string]string{},
		opStatus:        map[string]string{},
		autoRenewState:  map[string]bool{},
		nextOpID:        1,
	}
	f.server = httptest.NewServer(http.HandlerFunc(f.handle))
	return f
}

func (f *fakeUpstream) Close() { f.server.Close() }

// setAvailability 设置域名可用性。
func (f *fakeUpstream) setAvailability(domain string, r *CheckResult) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.availability[strings.ToLower(domain)] = r
}

// setOutcome 设置注册的异步终态（success/failed/pending）。
func (f *fakeUpstream) setOutcome(domain, status string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.registerOutcome[strings.ToLower(domain)] = status
}

func (f *fakeUpstream) setRateLimited(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rateLimited = v
}

func (f *fakeUpstream) setFailMessage(msg string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failMessage = msg
}

// registerCallCount 上游被请求注册的域名次数。
func (f *fakeUpstream) registerCallCount(domain string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, d := range f.registerCalls {
		if d == strings.ToLower(domain) {
			n++
		}
	}
	return n
}

func (f *fakeUpstream) registerCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.registerCalls)
}

func (f *fakeUpstream) renewCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.renewCalls)
}

func (f *fakeUpstream) snapshotLastRegister() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastRegister
}

// snapshotLastRenew 返回最后一次续费请求体（校验 years / currentExpirationDate 形态）。
func (f *fakeUpstream) snapshotLastRenew() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastRenew
}

func (f *fakeUpstream) snapshotContacts() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]map[string]any, len(f.savedContacts))
	copy(out, f.savedContacts)
	return out
}

func (f *fakeUpstream) handle(w http.ResponseWriter, r *http.Request) {
	// 鉴权头必须存在（官方文档：X-Api-Key + X-Api-Secret）
	if r.Header.Get("X-Api-Key") == "" || r.Header.Get("X-Api-Secret") == "" {
		writeUpstreamErr(w, http.StatusUnauthorized, "missing credentials")
		return
	}
	f.mu.Lock()
	limited := f.rateLimited
	f.mu.Unlock()
	if limited {
		writeUpstreamErr(w, http.StatusTooManyRequests, "rate limited")
		return
	}

	// 真实 base 为 <host>/api/v1，故先剥掉 /api/v1（兼容仅有 /v1 的情况）
	path := r.URL.Path
	path = strings.TrimPrefix(path, "/api/v1")
	path = strings.TrimPrefix(path, "/v1")
	path = strings.TrimPrefix(path, "/")

	switch {
	// 异步操作查询
	case strings.HasPrefix(path, "async-operations/"):
		id := strings.TrimPrefix(path, "async-operations/")
		f.serveOperation(w, id)
		return
	// 联系人
	case path == "contacts":
		f.serveSaveContact(w, r)
		return
	case strings.HasPrefix(path, "contacts/"):
		f.serveGetContact(w, strings.TrimPrefix(path, "contacts/"))
		return
	// 可用性
	case strings.HasSuffix(path, "/available"):
		f.serveAvailability(w, strings.TrimSuffix(path, "/available"))
		return
	// 续费
	case strings.HasSuffix(path, "/renew"):
		f.serveRenew(w, strings.TrimSuffix(path, "/renew"), r)
		return
	// 自动续费开关 PUT /v1/domains/{domain}/autorenew
	case strings.HasSuffix(path, "/autorenew"):
		f.serveAutoRenew(w, strings.TrimSuffix(path, "/autorenew"), r)
		return
	// 域名列表（take/skip required）
	case path == "domains":
		f.serveListDomains(w, r)
		return
	default:
		// 其余视为域名操作：POST = 注册，GET = 详情，DELETE = 删除
		switch r.Method {
		case http.MethodPost:
			f.serveRegister(w, path, r)
		case http.MethodGet:
			f.serveDomainInfo(w, path)
		case http.MethodDelete:
			// 官方实现为 501 Not Implemented
			writeUpstreamErr(w, http.StatusNotImplemented, "not implemented")
		default:
			writeUpstreamErr(w, http.StatusMethodNotAllowed, "method not allowed")
		}
	}
}

func writeUpstreamErr(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"message": msg,
		"errors":  []string{msg},
	})
}

// domainOfPath 从 "<domain>/xxx" 形式的路径片段中取出域名（剥掉 domains/ 前缀并解码转义）。
func domainOfPath(raw string) string {
	domain := strings.TrimPrefix(raw, "domains/")
	domain, _ = url.PathUnescape(domain)
	return domain
}

// serveAvailability GET /v1/domains/{domain}/available
func (f *fakeUpstream) serveAvailability(w http.ResponseWriter, raw string) {
	domain := domainOfPath(raw)
	f.mu.Lock()
	res, ok := f.availability[strings.ToLower(domain)]
	f.mu.Unlock()
	if !ok {
		res = &CheckResult{Domain: domain, Result: "available"}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(res)
}

// serveRegister POST /v1/domains/{domain} → 202 + 异步头
func (f *fakeUpstream) serveRegister(w http.ResponseWriter, domain string, r *http.Request) {
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)

	f.mu.Lock()
	f.registerCalls = append(f.registerCalls, strings.ToLower(domain))
	f.lastRegister = body
	// 官方必填校验：years / privacyProtection / contacts
	if _, ok := body["years"]; !ok {
		f.mu.Unlock()
		writeUpstreamErr(w, http.StatusBadRequest, "years is required")
		return
	}
	if _, ok := body["privacyProtection"]; !ok {
		f.mu.Unlock()
		writeUpstreamErr(w, http.StatusBadRequest, "privacyProtection is required")
		return
	}
	if _, ok := body["contacts"]; !ok {
		f.mu.Unlock()
		writeUpstreamErr(w, http.StatusBadRequest, "contacts is required")
		return
	}
	if _, ok := body["autoRenew"]; !ok {
		f.mu.Unlock()
		writeUpstreamErr(w, http.StatusBadRequest, "autoRenew is required")
		return
	}
	if fail := f.failMessage; fail != "" {
		f.mu.Unlock()
		writeUpstreamErr(w, http.StatusBadRequest, fail)
		return
	}
	outcome, ok := f.registerOutcome[strings.ToLower(domain)]
	if !ok {
		outcome = "success"
	}
	f.nextOpID++
	opID := fmt.Sprintf("op-%d-%d", f.nextOpID, time.Now().UnixNano())
	f.opStatus[opID] = outcome
	f.mu.Unlock()

	w.Header().Set("spaceship-async-operationid", opID)
	w.WriteHeader(http.StatusAccepted)
}

// serveOperation GET /v1/async-operations/{id} —— HTTP 恒 200，靠 body.status
func (f *fakeUpstream) serveOperation(w http.ResponseWriter, id string) {
	f.mu.Lock()
	status := f.opStatus[id]
	f.mu.Unlock()
	if status == "" {
		status = "pending"
	}
	out := map[string]any{
		"operationId": id,
		"type":        "domains_Create",
		"status":      status,
		"createdAt":   time.Now().Add(-time.Minute).Format(time.RFC3339),
		"modifiedAt":  time.Now().Format(time.RFC3339),
	}
	switch status {
	case "success":
		out["details"] = map[string]any{
			"domainId":  "sp-12345",
			"createdAt": time.Now().Format(time.RFC3339),
			"expiresAt": time.Now().AddDate(1, 0, 0).Format(time.RFC3339),
		}
	case "failed":
		out["error"] = "upstream registration failed"
		out["details"] = map[string]any{"error": "upstream registration failed"}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// serveRenew POST /v1/domains/{domain}/renew —— 官方真实接口，必填 years + currentExpirationDate
// 官方文档：currentExpirationDate 为 string <date-time>（RFC3339），传整数会被判 400。
// 假上游按真实契约强校验，防止客户端再退回毫秒整数写法。
func (f *fakeUpstream) serveRenew(w http.ResponseWriter, domain string, r *http.Request) {
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	if _, ok := body["years"]; !ok {
		writeUpstreamErr(w, http.StatusBadRequest, "years is required")
		return
	}
	rawExp, ok := body["currentExpirationDate"]
	if !ok {
		writeUpstreamErr(w, http.StatusBadRequest, "currentExpirationDate is required")
		return
	}
	expStr, isStr := rawExp.(string)
	if !isStr {
		writeUpstreamErr(w, http.StatusBadRequest, "currentExpirationDate must be string <date-time>")
		return
	}
	if _, err := time.Parse(time.RFC3339, expStr); err != nil {
		writeUpstreamErr(w, http.StatusBadRequest, "currentExpirationDate must be RFC3339 date-time")
		return
	}
	f.mu.Lock()
	f.renewCalls = append(f.renewCalls, strings.ToLower(domain))
	f.lastRenew = body
	f.nextOpID++
	opID := fmt.Sprintf("rop-%d-%d", f.nextOpID, time.Now().UnixNano())
	f.opStatus[opID] = "success"
	f.mu.Unlock()
	w.Header().Set("spaceship-async-operationid", opID)
	w.WriteHeader(http.StatusAccepted)
}

// serveAutoRenew PUT /v1/domains/{domain}/autorenew —— 官方请求体为 {"isEnabled": bool}。
// 早期这里无脑返回 200，导致客户端传错字段名（autoRenew）也能通过测试，
// 真实上游却会 400。此处按文档强校验 isEnabled 必填且为布尔。
func (f *fakeUpstream) serveAutoRenew(w http.ResponseWriter, domain string, r *http.Request) {
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	enabled, ok := body["isEnabled"].(bool)
	if !ok {
		writeUpstreamErr(w, http.StatusBadRequest, "isEnabled is required")
		return
	}
	key := strings.ToLower(domainOfPath(domain))
	f.mu.Lock()
	f.autoRenewCalls = append(f.autoRenewCalls, key)
	f.lastAutoRenew = body
	f.autoRenewState[key] = enabled
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"isEnabled": enabled})
}

// autoRenewStateOf 读取某个域名在上游侧被设置的自动续费状态。
func (f *fakeUpstream) autoRenewStateOf(domain string) (bool, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.autoRenewState[strings.ToLower(domain)]
	return v, ok
}

func (f *fakeUpstream) autoRenewCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.autoRenewCalls)
}

// serveSaveContact PUT /v1/contacts —— 带 contactId 走更新，不带走新建
func (f *fakeUpstream) serveSaveContact(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	f.mu.Lock()
	f.contactCalls++
	f.savedContacts = append(f.savedContacts, body)
	id, _ := body["contactId"].(string)
	if id == "" {
		id = fmt.Sprintf("SP-CONTACT-%d", f.contactCalls)
	}
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"contactId": id,
		"firstName": body["firstName"],
		"lastName":  body["lastName"],
		"email":     body["email"],
	})
}

func (f *fakeUpstream) serveGetContact(w http.ResponseWriter, id string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"contactId": id, "firstName": "Fake", "lastName": "User"})
}

// serveListDomains GET /v1/domains —— take/skip 为 required
func (f *fakeUpstream) serveListDomains(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("take") == "" || r.URL.Query().Get("skip") == "" {
		writeUpstreamErr(w, http.StatusBadRequest, "take and skip are required")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode([]map[string]any{})
}

func (f *fakeUpstream) serveDomainInfo(w http.ResponseWriter, domain string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"domain":    domain,
		"domainId":  "sp-12345",
		"status":    "active",
		"expiresAt": time.Now().AddDate(1, 0, 0).Format(time.RFC3339),
		"autoRenew": false,
	})
}

// ---- 测试用插件实例（真库 + 假上游） ----

// testDBDSN 读取隔离测试数据库 DSN；未设置时跳过（与仓库既有集成测试一致）。
func testDBDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("未设置 TEST_DATABASE_DSN，跳过需要真实数据库的用例")
	}
	return dsn
}

// newTestPlugin 构造绑定真库 + 假上游的插件实例。
// 迁移：核心库迁移 + 本插件迁移（保证 users/balance 与插件表都存在）。
func newTestPlugin(t *testing.T, up *fakeUpstream, vals map[string]string) (*Plugin, *sql.DB) {
	t.Helper()
	dsn := testDBDSN(t)
	d, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })

	ctx := context.Background()
	if err := db.Migrate(ctx, d, db.Migrations()); err != nil {
		t.Fatalf("核心迁移失败: %v", err)
	}
	p := &Plugin{}
	if err := db.MigratePrefixed(ctx, d, p.Migrations(), Name); err != nil {
		t.Fatalf("插件迁移失败: %v", err)
	}

	all := map[string]string{
		"plugin.spaceship.apiKey":    "test-key",
		"plugin.spaceship.apiSecret": "test-secret",
	}
	for k, v := range vals {
		all[k] = v
	}
	p.host = (&plugin.Host{
		DB:       d,
		Settings: &fakeSettings{vals: all},
		Notify:   &fakeNotify{},
	}).ForPlugin(Name)
	p.repo = NewRepo(d)
	return p, d
}

// withFakeUpstream 把插件的客户端工厂临时指向假上游。
func withFakeUpstream(t *testing.T, up *fakeUpstream) {
	t.Helper()
	orig := newClient
	newClient = func(apiKey, apiSecret string) *Client {
		c := NewClient(apiKey, apiSecret)
		c.base = up.server.URL + "/api/v1"
		return c
	}
	t.Cleanup(func() { newClient = orig })
}

// withUser 构造携带普通用户会话的请求上下文。
func withUser(r *http.Request, userID int64) *http.Request {
	sess := &middleware.Session{UserID: userID, ExpiresAt: time.Now().Add(time.Hour)}
	return r.WithContext(middleware.WithSession(r.Context(), sess))
}

// withAdmin 构造携带管理员会话的请求上下文。
func withAdmin(r *http.Request) *http.Request {
	sess := &middleware.Session{UserID: 1, IsAdmin: true, ExpiresAt: time.Now().Add(time.Hour)}
	return r.WithContext(middleware.WithSession(r.Context(), sess))
}

// createTestUser 创建隔离测试用户并充值余额，返回 userID（余额单位：元字符串）。
func createTestUser(t *testing.T, d *sql.DB, tag, balance string) int64 {
	t.Helper()
	ctx := context.Background()
	email := fmt.Sprintf("sp-%s-%d@lumeidc.test", tag, time.Now().UnixNano())
	var id int64
	// users.status 为 SMALLINT（1=正常），balance 为 NUMERIC(12,2)
	err := d.QueryRowContext(ctx,
		`INSERT INTO users(email, password_hash, status, balance) VALUES($1,$2,1,$3::numeric) RETURNING id`,
		email, "x", balance).Scan(&id)
	if err != nil {
		t.Fatalf("创建测试用户失败: %v", err)
	}
	t.Cleanup(func() {
		_, _ = d.ExecContext(context.Background(), `DELETE FROM plugin_spaceship_operations WHERE domain LIKE 'sp-%'`)
		_, _ = d.ExecContext(context.Background(), `DELETE FROM plugin_spaceship_domains WHERE user_id=$1`, id)
		_, _ = d.ExecContext(context.Background(), `DELETE FROM plugin_spaceship_contacts WHERE user_id=$1`, id)
		_, _ = d.ExecContext(context.Background(), `DELETE FROM users WHERE id=$1`, id)
	})
	return id
}

// balanceOf 读取用户余额（分）。NUMERIC 用 ::text 取出后按小数解析，避免驱动差异。
func balanceOf(t *testing.T, d *sql.DB, userID int64) int64 {
	t.Helper()
	var s string
	if err := d.QueryRowContext(context.Background(), `SELECT balance::text FROM users WHERE id=$1`, userID).Scan(&s); err != nil {
		t.Fatalf("读取余额失败: %v", err)
	}
	_, cents, err := money.ParseNonNegative(s, 999999999999)
	if err != nil {
		t.Fatalf("余额格式异常 %q: %v", s, err)
	}
	return cents
}

// pluginMigrationsSnapshot 返回插件迁移文件列表（隔离校验用）。
func pluginMigrationsSnapshot(p *Plugin) []string {
	entries, err := fs.Glob(p.Migrations(), "migrations/*.sql")
	if err != nil {
		return nil
	}
	sort.Strings(entries)
	return entries
}
