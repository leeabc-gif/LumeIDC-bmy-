package violation

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"strings"
)

// 用户通知文案标题。站内信与（开启邮件转发的站点的）纯文本补发共用同一份文案，
// 故正文必须自包含：不引用站内链接、不出现仅管理员可见的备注（Note）。
const (
	notifyTitleCreated = "您有一条新的违规记录"
	notifyTitleUpdated = "您的违规记录已更新"
	notifyTitleRemoved = "您的违规记录已撤销"
)

// notifyUser 向被处置用户发送站内信（Host.Notify 通用消息，无邮件模板 code）。
// 开关 notifyUser=0 时静默；Host.Notify 未注入（nil）时降级为空操作——
// 通知失败仅记日志，绝不影响管理端保存/删除主流程的结果。
func (p *Plugin) notifyUser(ctx context.Context, userID int64, title, body string) {
	if p.host == nil || p.host.Notify == nil || userID <= 0 {
		return
	}
	if !p.cfgBool(ctx, "notifyUser", true) {
		return
	}
	if err := p.host.Notify.Notify(ctx, userID, title, body); err != nil {
		log.Printf("[violation] 用户通知发送失败 user=%d title=%q: %v", userID, title, err)
	}
}

// violationCreatedBody 新增违规通知正文：只陈述与用户权益相关的事实
// （类型/等级/措施/有效期），不含管理员内部备注，也不含 evidence_url——
// 该字段多为后台内部凭据/材料链接（JSON 接口已对非管理员收敛），
// 不得经站内信原文直达被处置用户。
func violationCreatedBody(rec *Record) string {
	var b strings.Builder
	b.WriteString("您的账户新增一条违规记录：\n")
	b.WriteString("违规类型：" + valueOr(rec.Type, "未填写") + "\n")
	b.WriteString("违规等级：" + levelLabel(rec.Level) + "\n")
	b.WriteString("处置措施：" + valueOr(rec.Action, "无") + "\n")
	b.WriteString("有效期：" + validityText(rec.StartsAt, rec.ExpiresAt))
	b.WriteString("\n如有异议，请联系管理员申诉。")
	return b.String()
}

// violationUpdatedBody 实质变更通知正文；changes 为变化项中文标签（materialChange 产出）。
func violationUpdatedBody(id int64, changes []string) string {
	return fmt.Sprintf("您的违规记录（编号 #%d）信息已更新，变更项：%s。如有异议，请联系管理员申诉。",
		id, strings.Join(changes, "、"))
}

// violationRemovedBody 撤销通知正文：处置不再生效，回头是岸。
func violationRemovedBody(id int64) string {
	return fmt.Sprintf("您的违规记录（编号 #%d）已被撤销，相关处置不再生效。", id)
}

// materialChange 比对编辑前后的实质性变化（忽略描述措辞、举证链接、管理员备注等
// 不影响用户权益的字段），返回变化项中文标签；无实质变化返回 nil，此时不打扰用户。
func materialChange(old, cur *Record) []string {
	var out []string
	if old.Type != cur.Type {
		out = append(out, "违规类型")
	}
	if old.Level != cur.Level {
		out = append(out, "违规等级")
	}
	if old.Action != cur.Action {
		out = append(out, "处置措施")
	}
	if old.Public != cur.Public {
		out = append(out, "公示状态")
	}
	if !sameNullTime(old.StartsAt, cur.StartsAt) {
		out = append(out, "生效时间")
	}
	if !sameNullTime(old.ExpiresAt, cur.ExpiresAt) {
		out = append(out, "过期时间")
	}
	return out
}

// sameNullTime 两个 NullTime 是否等价（有效位与时间值都相同）。
func sameNullTime(a, b sql.NullTime) bool {
	if a.Valid != b.Valid {
		return false
	}
	return !a.Valid || a.Time.Equal(b.Time)
}

// levelLabel 等级存储值 → 中文标签；未知值原样返回（配置固定三项，防御性兜底）。
func levelLabel(level string) string {
	if label, ok := levelLabels[level]; ok {
		return label
	}
	return level
}

// validityText 有效期人话描述（无效时间为空串的场景都要有明确说法）。
func validityText(starts, expires sql.NullTime) string {
	s, e := fmtTime(starts), fmtTime(expires)
	switch {
	case s == "" && e == "":
		return "未设置"
	case s == "":
		return "至 " + e + " 截止"
	case e == "":
		return "自 " + s + " 起，无截止日期"
	default:
		return s + " 至 " + e
	}
}

// valueOr 空串兜底（用户可见文案不出现裸空值）。
func valueOr(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
