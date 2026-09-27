package refund

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"lumeidc/internal/plugin"
)

// fakeNotifier 记录管理员告警调用（站内信方法在本包测试中不使用）。
type fakeNotifier struct {
	admins []fakeAdminAlert
}

type fakeAdminAlert struct {
	key      string
	category string
	subject  string
	body     string
}

func (f *fakeNotifier) NotifyTemplate(ctx context.Context, userID int64, code, title, body string, values ...map[string]string) error {
	return nil
}

func (f *fakeNotifier) Notify(ctx context.Context, userID int64, title, body string) error {
	return nil
}

func (f *fakeNotifier) NotifyAdminOnce(ctx context.Context, alertKey, category, subject, body string) error {
	f.admins = append(f.admins, fakeAdminAlert{alertKey, category, subject, body})
	return nil
}

// notifyTestPlugin 注入 Notify 的插件实例（fakeSettings/testPlugin 见 plugin_test.go）。
func notifyTestPlugin(vals map[string]string) (*Plugin, *fakeNotifier) {
	fr := &fakeNotifier{}
	p := &Plugin{}
	p.host = (&plugin.Host{Settings: &fakeSettings{vals: vals}, Notify: fr}).ForPlugin(Name)
	return p, fr
}

// 非 2xx 响应必须告警管理员，且告警键按「渠道+日期」稳定可去重。
func TestPushChannel_HTTPErrorAlertsAdmin(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	p, fr := notifyTestPlugin(nil)
	p.pushChannel(srv.URL, "wecom", "标题\n内容")
	if hits != 1 {
		t.Fatalf("应实际发出 1 次推送，实际 %d 次", hits)
	}
	if len(fr.admins) != 1 {
		t.Fatalf("应产生 1 条管理员告警，实际 %d 条", len(fr.admins))
	}
	a := fr.admins[0]
	wantKey := "refund_notify:wecom:" + time.Now().Format("2006-01-02")
	if a.key != wantKey {
		t.Fatalf("告警键错误: %q want %q", a.key, wantKey)
	}
	if a.category != "退款通知" || !strings.Contains(a.subject, "wecom") {
		t.Fatalf("告警分类/主题错误: %+v", a)
	}
	if !strings.Contains(a.body, "HTTP 500") {
		t.Fatalf("告警正文缺少失败原因:\n%s", a.body)
	}

	// 同日同渠道再次失败：键保持不变（去重由 NotifyAdminOnce 负责，插件必须产出稳定键）。
	p.pushChannel(srv.URL, "wecom", "标题\n内容")
	if len(fr.admins) != 2 || fr.admins[1].key != a.key {
		t.Fatalf("同日同渠道告警键应稳定: %+v", fr.admins)
	}
	// 不同渠道的键必须不同，避免一个渠道故障静默另一个渠道。
	p.pushChannel(srv.URL, "feishu", "标题\n内容")
	if fr.admins[2].key == a.key {
		t.Fatal("不同渠道的告警键不应相同")
	}
}

// 推送成功不得告警（否则正常退款会刷屏）。
func TestPushChannel_SuccessNoAlert(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p, fr := notifyTestPlugin(nil)
	p.pushChannel(srv.URL, "dingtalk", "标题\n内容")
	if len(fr.admins) != 0 {
		t.Fatalf("推送成功不应告警，实际 %d 条", len(fr.admins))
	}
}

// 网络层失败（连接被拒）同样要告警。
func TestPushChannel_NetworkErrorAlertsAdmin(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close() // 先关再推：连接被拒

	p, fr := notifyTestPlugin(nil)
	p.pushChannel(url, "wecom", "标题\n内容")
	if len(fr.admins) != 1 {
		t.Fatalf("网络失败应告警，实际 %d 条", len(fr.admins))
	}
	if !strings.Contains(fr.admins[0].body, "推送失败") {
		t.Fatalf("告警正文应说明失败:\n%s", fr.admins[0].body)
	}
}

// 未开启/未配 URL 的渠道不进列表；Notify 未注入时告警静默降级。
func TestLoadChannelsAndNilNotifyNoPanic(t *testing.T) {
	p := testPlugin(map[string]string{
		"plugin.refund.notifyWecom":     "1",
		"plugin.refund.notifyWecomUrl":  "https://wecom.example.com/hook",
		"plugin.refund.notifyDingtalk":  "1", // 缺 URL → 不入列
		"plugin.refund.notifyFeishu":    "0",
		"plugin.refund.notifyFeishuUrl": "https://feishu.example.com/hook",
	})
	got := p.loadChannels(context.Background())
	if len(got) != 1 || got[0].kind != "wecom" || got[0].url != "https://wecom.example.com/hook" {
		t.Fatalf("渠道加载错误: %+v", got)
	}

	p2, fr2 := notifyTestPlugin(nil)
	p2.host.Notify = nil
	p2.alertChannelOnce("wecom", http.ErrHandlerTimeout) // 不得 panic
	if len(fr2.admins) != 0 {
		t.Fatal("Notify 未注入时不应产生告警")
	}
}
