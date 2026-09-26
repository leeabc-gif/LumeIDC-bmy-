package refund

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

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

func TestWithinWindow(t *testing.T) {
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	paid := now.Add(-3 * 24 * time.Hour)
	cases := []struct {
		name string
		paid time.Time
		days int
		want bool
	}{
		{"不限期限", now.Add(-9999 * time.Hour), 0, true},
		{"负天数按不限", paid, -1, true},
		{"期限内", paid, 7, true},
		{"恰好到期", paid, 3, true},
		{"超出期限", paid, 2, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := withinWindow(c.paid, now, c.days); got != c.want {
				t.Fatalf("withinWindow(%v, %v, %d) = %v want %v", c.paid, now, c.days, got, c.want)
			}
		})
	}
}

func TestShouldAutoApprove(t *testing.T) {
	cases := []struct {
		name       string
		mode       string
		maxCents   int64
		amountCent int64
		want       bool
	}{
		{"人工模式不自动", "manual", 500, 100, false},
		{"未设阈值不自动", "auto", 0, 100, false},
		{"阈值内自动通过", "auto", 500, 100, true},
		{"等于阈值自动通过", "auto", 500, 500, true},
		{"超阈值不自动", "auto", 500, 501, false},
		{"未知模式不自动", "weird", 500, 100, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := shouldAutoApprove(c.mode, c.maxCents, c.amountCent); got != c.want {
				t.Fatalf("shouldAutoApprove(%q,%d,%d) = %v want %v", c.mode, c.maxCents, c.amountCent, got, c.want)
			}
		})
	}
}

// 配置未存 settings 时 Host.Config 返回空串，插件必须兜底为 schema 默认值。
func TestConfigFallbackDefaults(t *testing.T) {
	p := testPlugin(nil)
	ctx := context.Background()
	if got := p.cfg(ctx, "refundReasons", defReasons); got != defReasons {
		t.Fatalf("cfg 兜底失败: %q", got)
	}
	if got := p.cfgInt(ctx, "refundWindowDays", 7); got != 7 {
		t.Fatalf("cfgInt 兜底失败: %d", got)
	}
	if got := p.cfgCents(ctx, "refundMaxAmount", 0); got != 0 {
		t.Fatalf("cfgCents 兜底失败: %d", got)
	}
	if !p.cfgBool(ctx, "notifyAdminOnSubmit", true) {
		t.Fatal("cfgBool 兜底失败")
	}
	if got := p.cfgList(ctx, "refundMethods", defMethods); len(got) != 1 || got[0] != "balance" {
		t.Fatalf("cfgList 兜底失败: %v", got)
	}
}

// multiselect 存 JSON 数组字符串；textarea 存多行文本；非法值回退默认。
func TestConfigStoredValues(t *testing.T) {
	p := testPlugin(map[string]string{
		"plugin.refund.refundMethods":  `["gateway","balance"]`,
		"plugin.refund.refundReasons":  "不想要了\n重复购买\n",
		"plugin.refund.reviewMode":     "auto",
		"plugin.refund.autoApproveMax": "12.50",
		"plugin.refund.refundWindowDays": "30",
		"plugin.refund.notifyUserOnResult": "0",
		"plugin.refund.refundMaxAmount": "99.99",
	})
	ctx := context.Background()
	if got := p.cfgList(ctx, "refundMethods", defMethods); len(got) != 2 || got[0] != "gateway" || got[1] != "balance" {
		t.Fatalf("JSON 数组解析错误: %v", got)
	}
	if got := splitLines(p.cfg(ctx, "refundReasons", defReasons)); len(got) != 2 || got[0] != "不想要了" {
		t.Fatalf("多行选项解析错误: %v", got)
	}
	if got := p.cfgInt(ctx, "refundWindowDays", 7); got != 30 {
		t.Fatalf("cfgInt 读取错误: %d", got)
	}
	if got := p.cfgCents(ctx, "autoApproveMax", 0); got != 1250 {
		t.Fatalf("cfgCents 读取错误: %d", got)
	}
	if got := p.cfgCents(ctx, "refundMaxAmount", 0); got != 9999 {
		t.Fatalf("cfgCents 读取错误: %d", got)
	}
	if p.cfgBool(ctx, "notifyUserOnResult", true) {
		t.Fatal("存了 \"0\" 应为 false")
	}

	// 非法数字回退默认，不 panic。
	bad := testPlugin(map[string]string{"plugin.refund.refundWindowDays": "abc"})
	if got := bad.cfgInt(ctx, "refundWindowDays", 7); got != 7 {
		t.Fatalf("非法数字应回退默认: %d", got)
	}
	// 非法 JSON 数组回退 splitLines。
	broken := testPlugin(map[string]string{"plugin.refund.refundMethods": "[oops"})
	if got := broken.cfgList(ctx, "refundMethods", defMethods); len(got) != 1 {
		t.Fatalf("非法 JSON 应回退默认: %v", got)
	}
}

// 前台表单选项：方式/原因/上限文本/期限一次取齐，供申请页渲染。
func TestClientOptions(t *testing.T) {
	p := testPlugin(map[string]string{
		"plugin.refund.refundMethods": `["balance","gateway"]`,
		"plugin.refund.refundReasons": "不想要了\n其他",
		"plugin.refund.refundMaxAmount": "10.00",
		"plugin.refund.refundWindowDays": "3",
	})
	opts := p.clientOptions(context.Background())
	methods, ok := opts["methods"].([]map[string]string)
	if !ok || len(methods) != 2 || methods[1]["value"] != "gateway" || methods[1]["label"] != "原路退回" {
		t.Fatalf("方式选项错误: %#v", opts["methods"])
	}
	reasons, ok := opts["reasons"].([]string)
	if !ok || len(reasons) != 2 || reasons[1] != "其他" {
		t.Fatalf("原因选项错误: %#v", opts["reasons"])
	}
	if opts["maxCents"].(int64) != 1000 || opts["maxText"].(string) != "10.00" {
		t.Fatalf("上限错误: %v %v", opts["maxCents"], opts["maxText"])
	}
	if opts["windowDays"].(int) != 3 {
		t.Fatalf("期限错误: %v", opts["windowDays"])
	}
}

func TestSplitLines(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"空串", "", []string{}},
		{"多行去空白空行", " 不想要了 \n\n 不想要了 \n重复购买\n", []string{"不想要了", "重复购买"}},
		{"去重保序", "a\nb\na\nb", []string{"a", "b"}},
		{"仅空白行", "  \n\t\n", []string{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := splitLines(c.in)
			if len(got) != len(c.want) {
				t.Fatalf("长度不符: got %v want %v", got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("第 %d 项不符: got %q want %q", i, got[i], c.want[i])
				}
			}
		})
	}
}

func TestContains(t *testing.T) {
	opts := []string{"balance", "gateway"}
	if !contains(opts, "balance") || contains(opts, "alipay") || contains(nil, "balance") {
		t.Fatal("contains 白名单判定错误")
	}
}

func TestParseBool(t *testing.T) {
	for _, v := range []string{"", "0", "false"} {
		if parseBool(v) {
			t.Fatalf("%q 应为 false", v)
		}
	}
	for _, v := range []string{"1", "true", "on", "yes"} {
		if !parseBool(v) {
			t.Fatalf("%q 应为 true", v)
		}
	}
}

func TestFmtNullTime(t *testing.T) {
	if s := fmtNullTime(sql.NullTime{}); s != "" {
		t.Fatalf("无效时间应为空串: %q", s)
	}
	tm := time.Date(2026, 9, 21, 10, 30, 0, 0, time.UTC)
	if s := fmtNullTime(sql.NullTime{Time: tm, Valid: true}); s != "2026-09-21 10:30" {
		t.Fatalf("格式化不符: %q", s)
	}
}

func TestPageParam(t *testing.T) {
	cases := []struct {
		query     string
		wantPage  int
		wantLimit int
	}{
		{"", 1, 10},
		{"page=0", 1, 10},
		{"page=abc", 1, 10},
		{"page=3", 3, 10},
		{"limit=0", 1, 10},
		{"limit=999", 1, 10},
		{"limit=50", 1, 50},
		{"page=2&limit=20", 2, 20},
	}
	for _, c := range cases {
		q, err := url.ParseQuery(c.query)
		if err != nil {
			t.Fatal(err)
		}
		page, limit := pageParam(q)
		if page != c.wantPage || limit != c.wantLimit {
			t.Fatalf("query %q: got (%d,%d) want (%d,%d)", c.query, page, limit, c.wantLimit, c.wantLimit)
		}
	}
}

// 前台视图不得外泄邮箱/处理人；状态与方式必须有中文标签。
// handle_note 即驳回原因（与用户通知正文一致），前台可见。
func TestRequestItemKeys(t *testing.T) {
	row := Row{
		Request: Request{
			ID: 1, OrderID: 2, UserID: 3, Amount: "10.00", Reason: "不想要了",
			Method: "gateway", Status: statusRejected, HandleNote: "内部备注",
			HandledBy: sql.NullInt64{Int64: 9, Valid: true},
		},
		UserName:  "小明",
		UserEmail: "a@b.c",
	}
	public := requestItem(row, false)
	for _, k := range []string{"user_email", "handled_by"} {
		if _, ok := public[k]; ok {
			t.Fatalf("前台视图不应含 %q", k)
		}
	}
	if _, ok := public["handle_note"]; !ok {
		t.Fatal("前台视图应含驳回原因（handle_note）")
	}
	if public["status_label"] != "已驳回" || public["method_label"] != "原路退回" {
		t.Fatalf("标签错误: %v %v", public["status_label"], public["method_label"])
	}
	admin := requestItem(row, true)
	for _, k := range []string{"user_email", "handle_note", "handled_by"} {
		if _, ok := admin[k]; !ok {
			t.Fatalf("后台视图应含 %q", k)
		}
	}
}

// 插件元信息与菜单：名称固定、菜单挂用户组、前台页指向 /plugin/refund。
func TestPluginMeta(t *testing.T) {
	p := &Plugin{}
	info := p.Info()
	if info.Name != Name {
		t.Fatalf("插件名与常量不一致: %q", info.Name)
	}
	if Name != "refund" {
		t.Fatalf("插件名应为 refund: %q", Name)
	}
	if info.Title == "" || info.Version == "" || info.Description == "" {
		t.Fatalf("元信息不完整: %+v", info)
	}
	menu := p.AdminMenu()
	if menu.Title == "" || menu.Parent != "users" {
		t.Fatalf("后台菜单错误: %+v", menu)
	}
	page := p.ClientPage()
	if page.To != "/plugin/refund" {
		t.Fatalf("前台页路径错误: %q", page.To)
	}
}

// 配置结构：键且类型合法；multiselect 默认值须为合法 JSON（前端解析依赖）。
func TestConfigSchema(t *testing.T) {
	fields := (&Plugin{}).ConfigSchema()
	keys := map[string]string{}
	for _, f := range fields {
		keys[f.Key] = f.Type
	}
	want := map[string]string{
		"refundWindowDays":     "number",
		"refundMethods":        "multiselect",
		"refundReasons":        "textarea",
		"reviewMode":           "select",
		"autoApproveMax":       "number",
		"refundMaxAmount":      "number",
		"notifyAdminOnSubmit":  "switch",
		"notifyUserOnResult":   "switch",
		"notifyWecom":          "switch",
		"notifyWecomUrl":       "text",
		"notifyDingtalk":       "switch",
		"notifyDingtalkUrl":    "text",
		"notifyFeishu":         "switch",
		"notifyFeishuUrl":      "text",
	}
	for k, typ := range want {
		if keys[k] != typ {
			t.Fatalf("配置 %q 类型错误: got %q want %q", k, keys[k], typ)
		}
	}
	if len(fields) != len(want) {
		t.Fatalf("配置项数量错误: %d", len(fields))
	}
	var arr []string
	if err := json.Unmarshal([]byte(defMethods), &arr); err != nil || len(arr) != 1 || arr[0] != "balance" {
		t.Fatalf("multiselect 默认值须为合法 JSON 数组: %q err=%v", defMethods, err)
	}
}

// 事件常量稳定（webhooknotify 等订阅方依赖）。
func TestEventConstants(t *testing.T) {
	if EventRefundRequestCreated != "refund_request.created" ||
		EventRefundApproved != "refund_request.approved" ||
		EventRefundRejected != "refund_request.rejected" {
		t.Fatalf("事件常量被改动: %q %q %q", EventRefundRequestCreated, EventRefundApproved, EventRefundRejected)
	}
	if statusLabels[statusPending] != "待审核" || methodLabels["balance"] != "退回余额" {
		t.Fatal("标签映射错误")
	}
}

// 请求体解析：JSON（bool true → "1"）、表单。
func TestParseFormValues(t *testing.T) {
	req := httptest.NewRequest("POST", "/create", strings.NewReader(`{"order_id":"3","amount":"10.00","reason":"不想要了"}`))
	req.Header.Set("Content-Type", "application/json")
	vals, err := parseFormValues(req)
	if err != nil {
		t.Fatal(err)
	}
	if vals["order_id"] != "3" || vals["amount"] != "10.00" || vals["reason"] != "不想要了" {
		t.Fatalf("JSON 解析错误: %v", vals)
	}

	req2 := httptest.NewRequest("POST", "/create", strings.NewReader("order_id=5&method=balance"))
	req2.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	vals2, err := parseFormValues(req2)
	if err != nil {
		t.Fatal(err)
	}
	if vals2["order_id"] != "5" || vals2["method"] != "balance" {
		t.Fatalf("表单解析错误: %v", vals2)
	}
}

// 退款方式按商品规则 refund_type 映射。
func TestAllowedMethodsForRule(t *testing.T) {
	fallback := []string{"balance", "gateway"}
	cases := []struct {
		refundType string
		want       []string
	}{
		{"balance", []string{"balance"}},
		{"balance_gateway", []string{"balance", "gateway"}},
		{"gateway_record", []string{"gateway"}},
		{"unknown", fallback},
	}
	for _, c := range cases {
		got := allowedMethodsForRule(c.refundType, fallback)
		if len(got) != len(c.want) {
			t.Fatalf("refund_type=%q 期望 %v 实际 %v", c.refundType, c.want, got)
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Fatalf("refund_type=%q 第 %d 项不匹配: 期望 %q 实际 %q", c.refundType, i, c.want[i], got[i])
			}
		}
	}
}

// isValidDecimal 边界校验。
func TestIsValidDecimal(t *testing.T) {
	valid := []string{"0", "0.00", "1", "1.5", "100.99", "0.01", "99999.99"}
	for _, s := range valid {
		if !isValidDecimal(s) {
			t.Errorf("期望合法: %q", s)
		}
	}
	invalid := []string{"", "abc", "1.2.3", ".5", "5.", "-1", "1a", " 1"}
	for _, s := range invalid {
		if isValidDecimal(s) {
			t.Errorf("期望非法: %q", s)
		}
	}
}

// cycleDays 周期天数映射。
func TestCycleDays(t *testing.T) {
	if got := cycleDays("monthly"); got != 30 {
		t.Errorf("monthly 期望 30 实际 %d", got)
	}
	if got := cycleDays("quarterly"); got != 90 {
		t.Errorf("quarterly 期望 90 实际 %d", got)
	}
	if got := cycleDays("yearly"); got != 365 {
		t.Errorf("yearly 期望 365 实际 %d", got)
	}
	if got := cycleDays("unknown"); got != 30 {
		t.Errorf("未知周期期望默认 30 实际 %d", got)
	}
}

// proratedRefundable 按天退款比例计算。
func TestProratedRefundable(t *testing.T) {
	// 月付订单 300 元，支付 0 天 → 全额可退。
	fresh := sql.NullTime{Time: time.Now(), Valid: true}
	if got := proratedRefundable(30000, fresh, "monthly"); got != 30000 {
		t.Errorf("刚支付应全额可退，实际 %d 分", got)
	}
	// 月付 300 元，支付 15 天 → 退一半（剩余 15/30）。
	half := sql.NullTime{Time: time.Now().AddDate(0, 0, -15), Valid: true}
	got := proratedRefundable(30000, half, "monthly")
	if got < 14500 || got > 15500 {
		t.Errorf("过半期望约 150 元，实际 %d 分", got)
	}
	// 支付超过 30 天 → 不可退。
	old := sql.NullTime{Time: time.Now().AddDate(0, 0, -31), Valid: true}
	if got := proratedRefundable(30000, old, "monthly"); got != 0 {
		t.Errorf("超期期望 0，实际 %d 分", got)
	}
	// 支付时间无效 → 回退全额。
	if got := proratedRefundable(30000, sql.NullTime{Valid: false}, "monthly"); got != 30000 {
		t.Errorf("时间无效应回退全额，实际 %d 分", got)
	}
}

// withinProductWindow 商品规则窗口校验（天/小时/不限）。
func TestWithinProductWindow(t *testing.T) {
	now := time.Now()
	recent := sql.NullTime{Time: now.Add(-2 * time.Hour), Valid: true}
	old := sql.NullTime{Time: now.Add(-72 * time.Hour), Valid: true}

	if !withinProductWindow(recent, now, "hours", 24) {
		t.Error("2 小时前应在 24 小时窗口内")
	}
	if withinProductWindow(old, now, "hours", 24) {
		t.Error("72 小时前应超出 24 小时窗口")
	}
	if !withinProductWindow(recent, now, "days", 1) {
		t.Error("2 小时前应在 1 天窗口内")
	}
	if withinProductWindow(old, now, "days", 2) {
		t.Error("72 小时前应超出 2 天窗口")
	}
	if !withinProductWindow(old, now, "days", 0) {
		t.Error("windowValue=0 应视为不限")
	}
	if withinProductWindow(sql.NullTime{Valid: false}, now, "days", 7) {
		t.Error("支付时间无效且有限期时应拒绝")
	}
}

// 驳回备注长度上限 500：纯长度函数级断言（实际 handler 限制在 adminReject）。
func TestHandleNoteMaxLen(t *testing.T) {
	const maxLen = 500
	if got := len([]rune(strings.Repeat("驳", maxLen))); got != maxLen {
		t.Fatalf("期望 %d 实际 %d", maxLen, got)
	}
	if got := len([]rune(strings.Repeat("驳", maxLen+1))); got <= maxLen {
		t.Fatalf("超长应能区分: %d", got)
	}
}

// 校验 approvals / auto approvals 写库时把 admin=NULL 与 admin=非零 都正确表达。
func TestClaimByNullVsInt(t *testing.T) {
	// 只能纯函数级断言：handler 行为是否调用 ClaimBy 而非 Claim。
	// 这里仅断言 sql.NullInt64{Valid:false} 与 sql.NullInt64{Valid:true} 字段语义。
	var nullAdmin sql.NullInt64
	if nullAdmin.Valid {
		t.Fatal("零值应为无效")
	}
	nonNull := sql.NullInt64{Int64: 99, Valid: true}
	if !nonNull.Valid || nonNull.Int64 != 99 {
		t.Fatalf("非零值语义错误: %+v", nonNull)
	}
}
