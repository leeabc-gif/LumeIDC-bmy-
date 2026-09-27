package violation

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"lumeidc/internal/plugin"
)

// fakeNotifier 记录站内信/管理员告警调用，验证通知门禁与文案。
type fakeNotifier struct {
	msgs     []fakeMsg
	failWith error
}

type fakeMsg struct {
	userID int64
	title  string
	body   string
}

func (f *fakeNotifier) NotifyTemplate(ctx context.Context, userID int64, code, title, body string, values ...map[string]string) error {
	return nil
}

func (f *fakeNotifier) Notify(ctx context.Context, userID int64, title, body string) error {
	if f.failWith != nil {
		return f.failWith
	}
	f.msgs = append(f.msgs, fakeMsg{userID: userID, title: title, body: body})
	return nil
}

func (f *fakeNotifier) NotifyAdminOnce(ctx context.Context, alertKey, category, subject, body string) error {
	return nil
}

// notifyTestPlugin 带 Notify 注入的插件实例（fakeSettings 见 limiter_test.go）。
func notifyTestPlugin(vals map[string]string) (*Plugin, *fakeNotifier) {
	fr := &fakeNotifier{}
	p := &Plugin{}
	p.host = (&plugin.Host{Settings: &fakeSettings{vals: vals}, Notify: fr}).ForPlugin(Name)
	return p, fr
}

// 默认（未配置）应通知用户，且文案自包含、不含管理员内部备注。
func TestNotifyUser_DefaultOnSendsMessage(t *testing.T) {
	p, fr := notifyTestPlugin(nil)
	rec := &Record{
		ID: 7, UserID: 42, Type: "垃圾邮件", Level: "medium", Action: "暂停服务",
		EvidenceURL: "https://cdn.example.com/e.png",
		StartsAt:    sql.NullTime{Time: time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC), Valid: true},
		ExpiresAt:   sql.NullTime{Time: time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC), Valid: true},
		Note:        "内部备注：已电话联系",
	}
	p.notifyUser(context.Background(), rec.UserID, notifyTitleCreated, violationCreatedBody(rec))
	if len(fr.msgs) != 1 {
		t.Fatalf("应发送 1 条通知，实际 %d 条", len(fr.msgs))
	}
	msg := fr.msgs[0]
	if msg.userID != 42 || msg.title != notifyTitleCreated {
		t.Fatalf("通知目标/标题错误: %+v", msg)
	}
	for _, want := range []string{"垃圾邮件", "中度", "暂停服务", "2026-09-01 08:00 至 2026-10-01 08:00", "https://cdn.example.com/e.png", "申诉"} {
		if !strings.Contains(msg.body, want) {
			t.Fatalf("正文缺少 %q:\n%s", want, msg.body)
		}
	}
	if strings.Contains(msg.body, "内部备注") {
		t.Fatalf("正文泄露管理员内部备注:\n%s", msg.body)
	}
}

// 开关关闭时完全静默。
func TestNotifyUser_DisabledByConfig(t *testing.T) {
	p, fr := notifyTestPlugin(map[string]string{"plugin.violation.notifyUser": "0"})
	p.notifyUser(context.Background(), 42, notifyTitleCreated, violationCreatedBody(&Record{Type: "欺诈行为", Level: "severe"}))
	if len(fr.msgs) != 0 {
		t.Fatalf("关闭后不应发送通知，实际 %d 条", len(fr.msgs))
	}
	// "1"/"true" 之外的合法真值同样视为开启
	p2, fr2 := notifyTestPlugin(map[string]string{"plugin.violation.notifyUser": "true"})
	p2.notifyUser(context.Background(), 42, notifyTitleCreated, "x")
	if len(fr2.msgs) != 1 {
		t.Fatalf("notifyUser=true 应发送通知，实际 %d 条", len(fr2.msgs))
	}
}

// 通知失败与 Notify 未注入都不得影响主流程（不 panic、不返回错误）。
func TestNotifyUser_FailureAndNilNotifyAreSwallowed(t *testing.T) {
	p, fr := notifyTestPlugin(nil)
	fr.failWith = errors.New("站内信服务不可用")
	p.notifyUser(context.Background(), 42, notifyTitleCreated, "x") // 仅记日志

	p2, _ := notifyTestPlugin(nil)
	p2.host.Notify = nil
	p2.notifyUser(context.Background(), 42, notifyTitleCreated, "x")

	p3 := &Plugin{} // host 未初始化（Init 前的极端情况）
	p3.notifyUser(context.Background(), 42, notifyTitleCreated, "x")

	p4, fr4 := notifyTestPlugin(nil)
	p4.notifyUser(context.Background(), 0, notifyTitleCreated, "x") // 无主用户不发送
	if len(fr4.msgs) != 0 {
		t.Fatalf("userID<=0 不应发送，实际 %d 条", len(fr4.msgs))
	}
}

// 文案构建：空值兜底与四种有效期组合。
func TestValidityTextAndCreatedBodyFallbacks(t *testing.T) {
	day := func(s string) sql.NullTime {
		ts, err := time.Parse("2006-01-02 15:04", s)
		if err != nil {
			t.Fatal(err)
		}
		return sql.NullTime{Time: ts, Valid: true}
	}
	cases := []struct {
		name             string
		starts, expires  sql.NullTime
		want             string
	}{
		{"都未设置", sql.NullTime{}, sql.NullTime{}, "未设置"},
		{"仅开始", day("2026-09-01 08:00"), sql.NullTime{}, "自 2026-09-01 08:00 起，无截止日期"},
		{"仅截止", sql.NullTime{}, day("2026-10-01 08:00"), "至 2026-10-01 08:00 截止"},
		{"区间", day("2026-09-01 08:00"), day("2026-10-01 08:00"), "2026-09-01 08:00 至 2026-10-01 08:00"},
	}
	for _, c := range cases {
		if got := validityText(c.starts, c.expires); got != c.want {
			t.Errorf("%s: validityText = %q want %q", c.name, got, c.want)
		}
	}
	body := violationCreatedBody(&Record{Level: "unknown-level"})
	if !strings.Contains(body, "unknown-level") {
		t.Errorf("未知等级应原样返回:\n%s", body)
	}
	if !strings.Contains(body, "违规类型：未填写") || !strings.Contains(body, "处置措施：无") {
		t.Errorf("空值兜底缺失:\n%s", body)
	}
	body2 := violationCreatedBody(&Record{Type: "滥用资源", Level: "light"})
	if strings.Contains(body2, "举证材料") {
		t.Errorf("无举证链接时不应出现举证行:\n%s", body2)
	}
}

// 实质变更判定：等级/措施/有效期/公示变化要通知，改错别字/举证链接/备注不通知。
func TestMaterialChange(t *testing.T) {
	base := func() *Record {
		return &Record{
			ID: 7, UserID: 42, Type: "垃圾邮件", Level: "light", Action: "警告",
			Description: "原始描述", EvidenceURL: "https://a.example.com",
			Public:      false,
			StartsAt:    sql.NullTime{Time: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), Valid: true},
			ExpiresAt:   sql.NullTime{Time: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), Valid: true},
			Note:        "备注",
		}
	}
	cases := []struct {
		name   string
		mutate func(r *Record)
		want   []string
	}{
		{"无实质变化", func(r *Record) { r.Description = "改了错别字"; r.EvidenceURL = "https://b.example.com"; r.Note = "新备注" }, nil},
		{"等级加重", func(r *Record) { r.Level = "severe" }, []string{"违规等级"}},
		{"措施变更", func(r *Record) { r.Action = "停用账户" }, []string{"处置措施"}},
		{"公示状态", func(r *Record) { r.Public = true }, []string{"公示状态"}},
		{"有效期延长", func(r *Record) { r.ExpiresAt = sql.NullTime{Time: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), Valid: true} }, []string{"过期时间"}},
		{"清除有效期", func(r *Record) { r.ExpiresAt = sql.NullTime{} }, []string{"过期时间"}},
		{"有效期值相同", func(r *Record) { r.ExpiresAt = sql.NullTime{Time: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), Valid: true} }, nil},
		{"多项同时变化", func(r *Record) { r.Type = "欺诈行为"; r.Level = "medium"; r.StartsAt = sql.NullTime{} }, []string{"违规类型", "违规等级", "生效时间"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cur := base()
			c.mutate(cur)
			got := materialChange(base(), cur)
			if len(got) != len(c.want) {
				t.Fatalf("materialChange = %v want %v", got, c.want)
			}
			for i := range c.want {
				if got[i] != c.want[i] {
					t.Fatalf("materialChange = %v want %v", got, c.want)
				}
			}
		})
	}
}

// 更新/撤销文案包含记录编号与变更项。
func TestUpdatedAndRemovedBodies(t *testing.T) {
	upd := violationUpdatedBody(7, []string{"违规等级", "处置措施"})
	for _, want := range []string{"#7", "违规等级、处置措施", "申诉"} {
		if !strings.Contains(upd, want) {
			t.Fatalf("更新文案缺少 %q:\n%s", want, upd)
		}
	}
	del := violationRemovedBody(7)
	if !strings.Contains(del, "#7") || !strings.Contains(del, "不再生效") {
		t.Fatalf("撤销文案不完整:\n%s", del)
	}
}
