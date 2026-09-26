package violation

import (
	"database/sql"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

func TestSplitLines(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"空串", "", []string{}},
		{"多行去空白空行", " 警告 \n\n 警告 \n停用账户\n", []string{"警告", "停用账户"}},
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

func TestMaskUser(t *testing.T) {
	if got := maskUser("小明", 7); got != "小明" {
		t.Fatalf("有昵称应显示昵称: %q", got)
	}
	if got := maskUser("   ", 7); got != "用户#7" {
		t.Fatalf("无昵称应脱敏: %q", got)
	}
	if got := maskUser("", 0); got != "用户#0" {
		t.Fatalf("空昵称应脱敏: %q", got)
	}
}

func TestParseBool(t *testing.T) {
	falses := []string{"", "0", "false"}
	for _, v := range falses {
		if parseBool(v) {
			t.Fatalf("%q 应为 false", v)
		}
	}
	trues := []string{"1", "true", "on", "yes"}
	for _, v := range trues {
		if !parseBool(v) {
			t.Fatalf("%q 应为 true", v)
		}
	}
}

func TestParseTime(t *testing.T) {
	zero, err := parseTime("")
	if err != nil || zero.Valid {
		t.Fatalf("空串应返回无效 NullTime: %+v err=%v", zero, err)
	}
	for _, v := range []string{"2026-09-21", "2026-09-21 10:30", "2026-09-21 10:30:00", "2026-09-21T10:30:00+08:00"} {
		got, err := parseTime(v)
		if err != nil || !got.Valid {
			t.Fatalf("%q 应解析成功: %+v err=%v", v, got, err)
		}
	}
	if _, err := parseTime("not-a-time"); err == nil {
		t.Fatal("非法时间应报错")
	}
}

func TestFmtTime(t *testing.T) {
	got, err := parseTime("2026-09-21 10:30")
	if err != nil {
		t.Fatal(err)
	}
	if s := fmtTime(got); s != "2026-09-21 10:30" {
		t.Fatalf("格式化不符: %q", s)
	}
	if s := fmtTime(sql.NullTime{}); s != "" {
		t.Fatalf("无效时间应为空串: %q", s)
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
			t.Fatalf("query %q: got (%d,%d) want (%d,%d)", c.query, page, limit, c.wantPage, c.wantLimit)
		}
	}
}

func TestContains(t *testing.T) {
	opts := []string{"警告", "停用账户"}
	if !contains(opts, "警告") || contains(opts, "其他") || contains(nil, "警告") {
		t.Fatal("contains 白名单判定错误")
	}
}

// 前台视图不得外泄邮箱/备注/处理人/凭据 URL；等级必须有中文标签；
// 长 description 必须截断以保护展示页；admin 视图必须看到全部字段。
func TestRecordItemAdminKeys(t *testing.T) {
	longDesc := strings.Repeat("啊", 500)
	row := RecordAdminRow{
		Record:     Record{ID: 1, UserID: 2, Level: "severe", Type: "垃圾邮件", HandledBy: 9, Note: "内部备注", Description: longDesc, EvidenceURL: "https://internal.example/secret"},
		UserName:   "小明",
		UserEmail:  "a@b.c",
	}
	public := recordItem(row, false)
	for _, k := range []string{"user_email", "note", "handled_by", "evidence_url"} {
		if _, ok := public[k]; ok {
			t.Fatalf("前台视图不应含 %q", k)
		}
	}
	if public["level_label"] != "严重" {
		t.Fatalf("等级标签错误: %v", public["level_label"])
	}
	pd, _ := public["description"].(string)
	if len([]rune(pd)) > 201 { // 200 字 + "…" 省略号
		t.Fatalf("前台 description 未截断：len=%d", len([]rune(pd)))
	}
	if !strings.HasSuffix(pd, "…") {
		t.Fatalf("前台 description 应以省略号结尾: %q", pd)
	}
	admin := recordItem(row, true)
	for _, k := range []string{"user_email", "note", "handled_by", "evidence_url"} {
		if _, ok := admin[k]; !ok {
			t.Fatalf("后台视图应含 %q", k)
		}
	}
	if admin["description"] != longDesc {
		t.Fatalf("后台 description 应保留原文")
	}
}

// truncateText 边界：空串、超限、不超限、max<=0。
func TestTruncateText(t *testing.T) {
	cases := []struct {
		in   string
		max  int
		want string
	}{
		{"", 5, ""},
		{"abc", 5, "abc"},
		{"abc", 3, "abc"},
		{"abcdef", 3, "abc…"},
		{"中文汉字超限", 2, "中文…"},
		{"hi", 0, ""},
	}
	for _, c := range cases {
		t.Run(c.in+"|"+strconv.Itoa(c.max), func(t *testing.T) {
			if got := truncateText(c.in, c.max); got != c.want {
				t.Fatalf("truncateText(%q,%d) = %q want %q", c.in, c.max, got, c.want)
			}
		})
	}
}

// 插件元信息与菜单：名称固定、菜单挂用户组、前台页指向 /plugin/violation。
func TestPluginMeta(t *testing.T) {
	p := &Plugin{}
	info := p.Info()
	if info.Name != Name {
		t.Fatalf("插件名与常量不一致: %q", info.Name)
	}
	if Name != "violation" {
		t.Fatalf("插件名应为 violation: %q", Name)
	}
	if info.Title == "" || info.Version == "" || info.Description == "" {
		t.Fatalf("元信息不完整: %+v", info)
	}
	menu := p.AdminMenu()
	if menu.Title == "" || menu.Parent != "users" {
		t.Fatalf("后台菜单错误: %+v", menu)
	}
	page := p.ClientPage()
	if page.To != "/plugin/violation" {
		t.Fatalf("前台页路径错误: %q", page.To)
	}
}

// 配置结构：三个键且类型合法（自动表单渲染依赖）。
func TestConfigSchema(t *testing.T) {
	fields := (&Plugin{}).ConfigSchema()
	keys := map[string]string{}
	for _, f := range fields {
		keys[f.Key] = f.Type
	}
	want := map[string]string{"typeOptions": "textarea", "actionOptions": "textarea", "defaultPublic": "switch"}
	for k, typ := range want {
		if keys[k] != typ {
			t.Fatalf("配置 %q 类型错误: got %q want %q", k, keys[k], typ)
		}
	}
	if len(fields) != len(want) {
		t.Fatalf("配置项数量错误: %d", len(fields))
	}
}

// 事件常量稳定（webhooknotify 等订阅方依赖）。
func TestEventConstants(t *testing.T) {
	if EventViolationCreated != "violation.created" || EventViolationRemoved != "violation.removed" {
		t.Fatalf("事件常量被改动: %q %q", EventViolationCreated, EventViolationRemoved)
	}
	if levelLabels["light"] != "轻微" || levelLabels["medium"] != "中度" || levelLabels["severe"] != "严重" {
		t.Fatal("等级标签映射错误")
	}
}

// 请求体解析：JSON（bool true → "1"、数字 → 字符串）、表单、空 JSON 键值。
func TestParseFormValues(t *testing.T) {
	req := httptest.NewRequest("POST", "/save", strings.NewReader(`{"id":"3","public":true,"hidden":false,"note":""}`))
	req.Header.Set("Content-Type", "application/json")
	vals, err := parseFormValues(req)
	if err != nil {
		t.Fatal(err)
	}
	if vals["id"] != "3" {
		t.Fatalf("id 解析错误: %q", vals["id"])
	}
	if vals["public"] != "1" {
		t.Fatalf("bool true 应为 \"1\": %q", vals["public"])
	}
	if _, ok := vals["hidden"]; ok {
		t.Fatal("bool false 不应写入键")
	}
	if _, ok := vals["note"]; !ok || vals["note"] != "" {
		t.Fatalf("空字符串应保留: %q", vals["note"])
	}

	req2 := httptest.NewRequest("POST", "/save", strings.NewReader("id=5&public=1"))
	req2.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	vals2, err := parseFormValues(req2)
	if err != nil {
		t.Fatal(err)
	}
	if vals2["id"] != "5" || vals2["public"] != "1" {
		t.Fatalf("表单解析错误: %v", vals2)
	}
}

// 前端 http 封装以 JSON 发送数字（user_id/id），后端必须能解析，否则保存必然失败。
func TestParseFormValuesJSONNumbers(t *testing.T) {
	req := httptest.NewRequest("POST", "/save", strings.NewReader(`{"id":7,"user_id":42,"public":false,"starts_at":""}`))
	req.Header.Set("Content-Type", "application/json")
	vals, err := parseFormValues(req)
	if err != nil {
		t.Fatal(err)
	}
	if vals["id"] != "7" {
		t.Fatalf("数字 id 解析错误: %q", vals["id"])
	}
	if vals["user_id"] != "42" {
		t.Fatalf("数字 user_id 解析错误: %q", vals["user_id"])
	}
	if _, ok := vals["public"]; ok {
		t.Fatal("bool false 不应写入键")
	}
	if _, ok := vals["starts_at"]; !ok || vals["starts_at"] != "" {
		t.Fatalf("空字符串应保留: %q", vals["starts_at"])
	}
}
