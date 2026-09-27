package refund

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"
)

// channelTimeout 单次 webhook 推送超时。
const channelTimeout = 5 * time.Second

// notifyChannels 向已开启的机器人渠道推送文本通知（异步，不阻塞主流程）。
func (p *Plugin) notifyChannels(ctx context.Context, title, content string) {
	channels := p.loadChannels(ctx)
	if len(channels) == 0 {
		return
	}
	text := title + "\n" + content
	for _, ch := range channels {
		go p.pushChannel(ch.url, ch.kind, text)
	}
}

type botChannel struct {
	kind string // wecom|dingtalk|feishu
	url  string
}

func (p *Plugin) loadChannels(ctx context.Context) []botChannel {
	var out []botChannel
	if p.cfgBool(ctx, "notifyWecom", false) {
		if u := p.cfg(ctx, "notifyWecomUrl", ""); u != "" {
			out = append(out, botChannel{kind: "wecom", url: u})
		}
	}
	if p.cfgBool(ctx, "notifyDingtalk", false) {
		if u := p.cfg(ctx, "notifyDingtalkUrl", ""); u != "" {
			out = append(out, botChannel{kind: "dingtalk", url: u})
		}
	}
	if p.cfgBool(ctx, "notifyFeishu", false) {
		if u := p.cfg(ctx, "notifyFeishuUrl", ""); u != "" {
			out = append(out, botChannel{kind: "feishu", url: u})
		}
	}
	return out
}

func (p *Plugin) pushChannel(rawURL, kind, text string) {
	var body []byte
	switch kind {
	case "wecom", "dingtalk":
		body, _ = json.Marshal(map[string]any{
			"msgtype": "text",
			"text":    map[string]string{"content": text},
		})
	case "feishu":
		body, _ = json.Marshal(map[string]any{
			"msg_type": "text",
			"content":  map[string]string{"text": text},
		})
	default:
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), channelTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, bytes.NewReader(body))
	if err != nil {
		log.Printf("[refund] %s webhook 构造失败: %v", kind, err)
		p.alertChannelOnce(kind, err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("[refund] %s webhook 推送失败: %v", kind, err)
		p.alertChannelOnce(kind, err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		cause := fmt.Errorf("HTTP %d", resp.StatusCode)
		log.Printf("[refund] %s webhook 响应异常: %s", kind, cause)
		p.alertChannelOnce(kind, cause)
	}
}

// alertChannelOnce 机器人渠道推送失败告警：webhook 配错、机器人被移出群、群容量满
// 这类持续性故障此前只写服务器日志，站长无从知晓通知早已中断。这里按「渠道+日期」
// 去重（同一天同一渠道只报一次），既不会因每单退款而刷屏，也不会彻底沉默。
// 与 alertAdmin（退款单据后续处理失败）同属管理侧链路，不受用户开关控制。
func (p *Plugin) alertChannelOnce(kind string, cause error) {
	if p.host == nil || p.host.Notify == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), channelTimeout)
	defer cancel()
	key := fmt.Sprintf("refund_notify:%s:%s", kind, time.Now().Format("2006-01-02"))
	body := fmt.Sprintf("退款通知推送失败（%s 渠道）：%v。请检查 webhook 地址、机器人是否仍在群内以及群容量限制。", kind, cause)
	if err := p.host.Notify.NotifyAdminOnce(ctx, key, "退款通知", kind+" 渠道推送失败", body); err != nil {
		log.Printf("[refund] %s 渠道失败告警发送失败: %v", kind, err)
	}
}
