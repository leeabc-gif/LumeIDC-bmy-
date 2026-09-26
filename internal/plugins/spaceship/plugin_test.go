package spaceship

import (
	"context"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"lumeidc/internal/plugin"
)

// fakeSettings 内存 settings（测试配置读取兜底；Host.Config 依赖 pluginName）。
type fakeSettings struct{ vals map[string]string }

func (f *fakeSettings) Get(ctx context.Context, key string) (string, error) { return f.vals[key], nil }
func (f *fakeSettings) Set(ctx context.Context, key, value string) error {
	f.vals[key] = value
	return nil
}

func testPlugin(vals map[string]string) *Plugin {
	p := &Plugin{}
	p.host = (&plugin.Host{Settings: &fakeSettings{vals: vals}}).ForPlugin(Name)
	return p
}

// 注册年限范围校验：1-10 年（Spaceship 上限 10 年）。
func TestYearsOK(t *testing.T) {
	cases := []struct {
		n    int
		want bool
	}{
		{-1, false},
		{0, false},
		{1, true},
		{5, true},
		{10, true},
		{11, false},
	}
	for _, c := range cases {
		if got := yearsOK(c.n); got != c.want {
			t.Errorf("yearsOK(%d) = %v want %v", c.n, got, c.want)
		}
	}
}

// 配置未存 settings 时 Host.Config 返回空串，插件必须兜底为 schema 默认值。
func TestConfigFallbackDefaults(t *testing.T) {
	p := testPlugin(nil)
	ctx := context.Background()
	if got := p.cfg(ctx, "apiKey", ""); got != "" {
		t.Fatalf("无默认值应返回空串: %q", got)
	}
	if got := p.cfg(ctx, "defaultYears", defDefaultYears); got != "1" {
		t.Fatalf("cfg 兜底失败: %q", got)
	}
	if got := p.cfg(ctx, "privacyLevel", defPrivacyLevel); got != "high" {
		t.Fatalf("privacyLevel 兜底失败: %q", got)
	}
	if got := p.cfgInt(ctx, "pollIntervalSec", 30); got != 30 {
		t.Fatalf("cfgInt 兜底失败: %d", got)
	}
	if got := p.cfgInt(ctx, "defaultYears", 1); got != 1 {
		t.Fatalf("cfgInt 兜底失败: %d", got)
	}
	if !p.cfgBool(ctx, "enableNotify", true) {
		t.Fatal("cfgBool 兜底失败")
	}
	if p.cfgBool(ctx, "enableNotify", false) {
		t.Fatal("无值时 cfgBool 应取传入默认 false")
	}
	// 未配置 Key/Secret → 客户端为 nil，轮询/查询等调用方据此跳过。
	if c := p.cfgClient(ctx); c != nil {
		t.Fatal("未配置凭据应返回 nil 客户端")
	}
}

// 存了 settings 后按存储值读取；非法数字回退默认；cfgBool 只认 "1"/"true"。
func TestConfigStoredValues(t *testing.T) {
	p := testPlugin(map[string]string{
		"plugin.spaceship.apiKey":          "key-123",
		"plugin.spaceship.apiSecret":       "secret-456",
		"plugin.spaceship.defaultYears":    "3",
		"plugin.spaceship.privacyLevel":    "low",
		"plugin.spaceship.pollIntervalSec": "abc",
		"plugin.spaceship.enableNotify":    "0",
	})
	ctx := context.Background()
	if got := p.cfg(ctx, "apiKey", ""); got != "key-123" {
		t.Fatalf("cfg 读取错误: %q", got)
	}
	if got := p.cfgInt(ctx, "defaultYears", 1); got != 3 {
		t.Fatalf("cfgInt 读取错误: %d", got)
	}
	if got := p.cfgInt(ctx, "pollIntervalSec", 30); got != 30 {
		t.Fatalf("非法数字应回退默认: %d", got)
	}
	if got := p.cfgInt(ctx, "pollIntervalSec", 30); got < 0 {
		t.Fatalf("不允许负数: %d", got)
	}
	if p.cfgBool(ctx, "enableNotify", true) {
		t.Fatal(`存了 "0" 应为 false`)
	}
	if c := p.cfgClient(ctx); c == nil {
		t.Fatal("配置了 Key/Secret 应返回客户端")
	}
	// 纯空白凭据视为未配置。
	blank := testPlugin(map[string]string{"plugin.spaceship.apiKey": "  ", "plugin.spaceship.apiSecret": "  "})
	if c := blank.cfgClient(ctx); c != nil {
		t.Fatal("空白凭据应视为未配置")
	}
}

// CronJobs：轮询间隔最小 5 秒（防止配置过小打爆 API），spec 格式 @every Ns。
func TestCronJobsSpec(t *testing.T) {
	cases := []struct {
		stored string
		want   string
	}{
		{"", "@every 30s"},   // 未配置 → 默认 30
		{"1", "@every 5s"},   // 过小 → 钳到 5
		{"0", "@every 5s"},   // 0 → 钳到 5
		{"60", "@every 60s"}, // 正常值
	}
	for _, c := range cases {
		vals := map[string]string{}
		if c.stored != "" {
			vals["plugin.spaceship.pollIntervalSec"] = c.stored
		}
		jobs := testPlugin(vals).CronJobs()
		if len(jobs) != 1 {
			t.Fatalf("CronJobs 应返回 1 个任务，实际 %d", len(jobs))
		}
		job := jobs[0]
		if job.Name != "spaceship-poll-operations" {
			t.Fatalf("任务名错误: %q", job.Name)
		}
		if job.Spec != c.want {
			t.Fatalf("stored=%q: spec = %q want %q", c.stored, job.Spec, c.want)
		}
		if job.Run == nil {
			t.Fatal("Run 不应为 nil")
		}
		if job.What == "" {
			t.Fatal("What 描述不应为空")
		}
	}
}

// 负数场景修正：cfgInt 对 "-3" 会因 n<0 回退 def(30)，CronJobs 再钳最小 5。
func TestCronJobsSpecNegative(t *testing.T) {
	jobs := testPlugin(map[string]string{"plugin.spaceship.pollIntervalSec": "-3"}).CronJobs()
	if jobs[0].Spec != "@every 30s" {
		t.Fatalf("负数应回退默认 30 秒: %q", jobs[0].Spec)
	}
}

// 插件元信息与菜单：名称固定、菜单挂业务组、前台页指向 /plugin/spaceship。
func TestPluginMeta(t *testing.T) {
	p := &Plugin{}
	info := p.Info()
	if info.Name != Name {
		t.Fatalf("插件名与常量不一致: %q", info.Name)
	}
	if Name != "spaceship" {
		t.Fatalf("插件名应为 spaceship: %q", Name)
	}
	if info.Title == "" || info.Version == "" || info.Description == "" {
		t.Fatalf("元信息不完整: %+v", info)
	}
	menu := p.AdminMenu()
	if menu.Title == "" || menu.Parent != plugin.MenuGroupBusiness {
		t.Fatalf("后台菜单错误: %+v", menu)
	}
	page := p.ClientPage()
	if page.To != "/plugin/spaceship" {
		t.Fatalf("前台页路径错误: %q", page.To)
	}
}

// 配置结构：8 个键且类型合法（框架自动渲染配置页依赖）。
func TestConfigSchema(t *testing.T) {
	fields := (&Plugin{}).ConfigSchema()
	keys := map[string]string{}
	for _, f := range fields {
		keys[f.Key] = f.Type
	}
	want := map[string]string{
		"apiKey":          "text",
		"apiSecret":       "password",
		"defaultYears":    "number",
		"privacyLevel":    "select",
		"pollIntervalSec": "number",
		"enableNotify":    "switch",
		"allowPremium":    "switch",
		"refundOnFailure": "switch",
	}
	for k, typ := range want {
		if keys[k] != typ {
			t.Fatalf("配置 %q 类型错误: got %q want %q", k, keys[k], typ)
		}
	}
	if len(fields) != len(want) {
		t.Fatalf("配置项数量错误: %d", len(fields))
	}
	// 默认值与兜底常量一致（配置页展示与读取处不得分叉）。
	defaults := map[string]string{}
	for _, f := range fields {
		if f.Default != "" {
			defaults[f.Key] = f.Default
		}
	}
	if defaults["defaultYears"] != defDefaultYears || defaults["pollIntervalSec"] != defPollIntervalSec {
		t.Fatalf("数字项默认值错误: %v", defaults)
	}
	// select 选项须严格等于官方支持的两档：high（高隐私，推荐）与 public。
	// 早期提供的 medium/low 已被官方移除，传入会被判 400（P1-7 回归）。
	for _, f := range fields {
		if f.Key == "privacyLevel" {
			if len(f.Options) != 2 || f.Options[0].Value != "high" || f.Options[1].Value != "public" {
				t.Fatalf("privacyLevel 选项错误: %+v", f.Options)
			}
		}
	}
	// 溢价默认关闭、失败退款默认开启（商业运营安全默认值）。
	if defaults["allowPremium"] != defAllowPremium || defaults["refundOnFailure"] != defRefundOnFailure {
		t.Fatalf("开关项默认值错误: %v", defaults)
	}
}

// IsRateLimited 仅 429 为真；其他错误/nil 为假。
func TestIsRateLimited(t *testing.T) {
	if IsRateLimited(nil) {
		t.Fatal("nil 不应判定为限流")
	}
	if !IsRateLimited(&APIError{HTTPStatus: 429}) {
		t.Fatal("429 应判定为限流")
	}
	if IsRateLimited(&APIError{HTTPStatus: 500}) {
		t.Fatal("500 不应判定为限流")
	}
	if IsRateLimited(context.DeadlineExceeded) {
		t.Fatal("非 APIError 不应判定为限流")
	}
}

// APIError.Error：errors 数组优先，其次 message，最后仅状态码。
func TestAPIErrorMessage(t *testing.T) {
	if got := (&APIError{HTTPStatus: 400, Errors: []string{"bad domain", "oops"}}).Error(); got != "Spaceship API 错误(400): bad domain; oops" {
		t.Fatalf("errors 数组格式错误: %q", got)
	}
	if got := (&APIError{HTTPStatus: 401, ErrorMessage: "unauthorized"}).Error(); got != "Spaceship API 错误(401): unauthorized" {
		t.Fatalf("message 格式错误: %q", got)
	}
	if got := (&APIError{HTTPStatus: 500}).Error(); got != "Spaceship API 错误(500)" {
		t.Fatalf("空错误体格式错误: %q", got)
	}
}

// CheckResult：available 可注册；premiumPricing/premium 标记溢价（前台需二次确认，后端禁自注册）。
func TestCheckResultFlags(t *testing.T) {
	var nilResult *CheckResult
	if nilResult.IsAvailable() || nilResult.IsPremium() {
		t.Fatal("nil 应为 false（不得 panic）")
	}
	free := &CheckResult{Domain: "a.com", Result: "available"}
	if !free.IsAvailable() || free.IsPremium() {
		t.Fatalf("普通可注册域名判定错误: %+v", free)
	}
	taken := &CheckResult{Domain: "b.com", Result: "taken"}
	if taken.IsAvailable() {
		t.Fatal("taken 不可注册")
	}
	premium := &CheckResult{Domain: "c.com", Result: "available", PremiumPricing: &Pricing{Price: 99}}
	if !premium.IsAvailable() || !premium.IsPremium() {
		t.Fatal("溢价域名应为 available+premium")
	}
	flagOnly := &CheckResult{Domain: "d.com", Result: "available", Premium: true}
	if !flagOnly.IsPremium() {
		t.Fatal("Premium 标记应生效")
	}
	proc := &CheckResult{Domain: "e.com", Result: "processing"}
	if proc.IsAvailable() {
		t.Fatal("processing 不可注册")
	}
}

// NewClient 去除首尾空白（配置里手滑多打空格不应导致鉴权失败）。
func TestNewClientTrim(t *testing.T) {
	c := NewClient("  key  ", "\tsecret\n")
	if c.apiKey != "key" || c.apiSecret != "secret" {
		t.Fatalf("凭据未去空白: %q %q", c.apiKey, c.apiSecret)
	}
	if !c.configured() {
		t.Fatal("非空凭据应 configured")
	}
	empty := NewClient("", "")
	if empty.configured() {
		t.Fatal("空凭据不应 configured")
	}
}

// writeOK/writeOKMsg：扁平响应 {ok:1,...}（与前端读取约定一致）。
func TestWriteOK(t *testing.T) {
	rec := httptest.NewRecorder()
	writeOK(rec, map[string]any{"list": []int{1, 2}, "total": 2})
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	if m["ok"] != float64(1) {
		t.Fatalf("ok 应为 1: %v", m["ok"])
	}
	if m["total"] != float64(2) {
		t.Fatalf("透传字段错误: %v", m["total"])
	}
	list, ok := m["list"].([]any)
	if !ok || len(list) != 2 {
		t.Fatalf("list 透传错误: %v", m["list"])
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("Content-Type 错误: %q", ct)
	}

	rec2 := httptest.NewRecorder()
	writeOKMsg(rec2, "保存成功")
	var m2 map[string]any
	if err := json.Unmarshal(rec2.Body.Bytes(), &m2); err != nil {
		t.Fatal(err)
	}
	if m2["ok"] != float64(1) || m2["msg"] != "保存成功" {
		t.Fatalf("writeOKMsg 输出错误: %v", m2)
	}
}

// readJSON：解析 JSON body；非法 JSON 返回错误。
func TestReadJSON(t *testing.T) {
	req := httptest.NewRequest("POST", "/save", strings.NewReader(`{"domain":"a.com","years":2}`))
	var m map[string]any
	if err := readJSON(req, &m); err != nil {
		t.Fatal(err)
	}
	if m["domain"] != "a.com" || m["years"] != float64(2) {
		t.Fatalf("JSON 解析错误: %v", m)
	}
	bad := httptest.NewRequest("POST", "/save", strings.NewReader(`{oops`))
	var v any
	if err := readJSON(bad, &v); err == nil {
		t.Fatal("非法 JSON 应报错")
	}
}

// readID：从 ServeMux 路径模式 {id} 取参；非数字报错。
func TestReadID(t *testing.T) {
	var gotID int64
	var gotErr error
	mux := http.NewServeMux()
	mux.HandleFunc("GET /domains/{id}", func(w http.ResponseWriter, r *http.Request) {
		gotID, gotErr = readID(r, "id")
	})
	mux.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/domains/42", nil))
	if gotErr != nil || gotID != 42 {
		t.Fatalf("readID 解析错误: id=%d err=%v", gotID, gotErr)
	}
	mux.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/domains/abc", nil))
	if gotErr == nil {
		t.Fatal("非数字 id 应报错")
	}
}

// 路由注册冒烟：admin/client 全部路由一次性注册不得 panic（核心在 Init 时调用）。
func TestRoutesRegistered(t *testing.T) {
	p := &Plugin{}
	p.RegisterAdminRoutes(http.NewServeMux())
	p.RegisterClientRoutes(http.NewServeMux())
}

// 迁移文件随二进制嵌入（Migrations() 可读 001_init.sql）。
func TestMigrationsEmbedded(t *testing.T) {
	data, err := fs.ReadFile((&Plugin{}).Migrations(), "migrations/001_init.sql")
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 {
		t.Fatal("迁移文件为空")
	}
}
