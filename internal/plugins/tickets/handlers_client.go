package tickets

import (
	"encoding/json"
	"errors"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"

	"lumeidc/internal/plugin"
)

// ---- 用户侧 handler（/plugin/tickets/，登录用户；原 handler/tickets.go 搬迁） ----

// clientList GET /plugin/tickets/list — 我的工单列表（搜索/筛选/分页）。
func (p *Plugin) clientList(w http.ResponseWriter, r *http.Request) {
	userID, ok := plugin.RequireUserID(w, r)
	if !ok {
		return
	}
	where := ` WHERE t.user_id=$1`
	args := []any{userID}
	next := 2
	if value := strings.TrimSpace(r.URL.Query().Get("q")); value != "" {
		where += ` AND (t.subject ILIKE $` + strconv.Itoa(next) + ` OR t.body ILIKE $` + strconv.Itoa(next) + `)`
		args = append(args, "%"+value+"%")
		next++
	}
	for key, column := range map[string]string{"status": "t.status", "priority": "t.priority", "category": "t.category"} {
		if value := r.URL.Query().Get(key); value != "" {
			where += ` AND ` + column + `=$` + strconv.Itoa(next)
			args = append(args, value)
			next++
		}
	}
	var total int
	if err := p.host.DB.QueryRowContext(r.Context(), `SELECT count(*) FROM tickets t`+where, args...).Scan(&total); err != nil {
		plugin.StatusFail(w, 500, "查询工单失败")
		return
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit < 1 || limit > 100 {
		limit = 10
	}
	args = append(args, limit, (page-1)*limit)
	rows, err := p.host.DB.QueryContext(r.Context(), `SELECT t.id,t.subject,t.body,t.category,t.priority,t.status,coalesce(t.service_id,0),coalesce(s.name,''),coalesce(s.hostname,''),to_char(t.created_at,'YYYY-MM-DD HH24:MI'),to_char(t.updated_at,'YYYY-MM-DD HH24:MI') FROM tickets t LEFT JOIN services s ON s.id=t.service_id`+where+` ORDER BY t.updated_at DESC LIMIT $`+strconv.Itoa(next)+` OFFSET $`+strconv.Itoa(next+1), args...)
	if err != nil {
		plugin.StatusFail(w, 500, "查询工单失败")
		return
	}
	defer rows.Close()
	list := make([]map[string]any, 0)
	for rows.Next() {
		var id int64
		var subject, body, category, priority, status, serviceName, serviceHostname, createdAt, updatedAt string
		var serviceID int64
		if err := rows.Scan(&id, &subject, &body, &category, &priority, &status, &serviceID, &serviceName, &serviceHostname, &createdAt, &updatedAt); err != nil {
			plugin.StatusFail(w, 500, "读取工单失败")
			return
		}
		list = append(list, map[string]any{"id": id, "subject": subject, "body": body, "category": category, "priority": priority, "status": status, "service_id": serviceID, "service_name": serviceName, "service_hostname": serviceHostname, "created_at": createdAt, "updated_at": updatedAt})
	}
	if err := rows.Err(); err != nil {
		plugin.StatusFail(w, 500, "读取工单失败")
		return
	}
	plugin.WriteJSON(w, map[string]any{"ok": 1, "list": list, "total": total, "page": page, "limit": limit})
}

// clientCreate POST /plugin/tickets/create — 新建工单。
func (p *Plugin) clientCreate(w http.ResponseWriter, r *http.Request) {
	userID, ok := plugin.RequireUserID(w, r)
	if !ok {
		return
	}
	var input struct {
		Subject   string `json:"subject"`
		Body      string `json:"body"`
		Priority  string `json:"priority"`
		ServiceID int64  `json:"service_id"`
		Category  string `json:"category"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024)).Decode(&input); err != nil {
		plugin.StatusFail(w, http.StatusBadRequest, "请求格式无效")
		return
	}
	input.Subject = strings.TrimSpace(input.Subject)
	input.Body = strings.TrimSpace(input.Body)
	if input.Subject == "" || input.Body == "" {
		plugin.StatusFail(w, http.StatusBadRequest, "请填写工单主题和问题描述")
		return
	}
	if len(input.Subject) > 160 || len(input.Body) > 10000 {
		plugin.StatusFail(w, http.StatusBadRequest, "工单内容过长")
		return
	}
	if input.Priority != "urgent" && input.Priority != "high" && input.Priority != "normal" {
		input.Priority = "normal"
	}
	if input.Category != "technical" && input.Category != "billing" && input.Category != "account" && input.Category != "pre-sale" {
		input.Category = "technical"
	}
	if input.ServiceID != 0 {
		var owned bool
		if err := p.host.DB.QueryRowContext(r.Context(), `SELECT EXISTS(SELECT 1 FROM services WHERE id=$1 AND user_id=$2 AND status<3)`, input.ServiceID, userID).Scan(&owned); err != nil || !owned {
			plugin.StatusFail(w, http.StatusBadRequest, "关联的服务不存在或无权访问")
			return
		}
	}
	var id int64
	err := p.host.DB.QueryRowContext(r.Context(), `INSERT INTO tickets(user_id,subject,body,priority,category,service_id) VALUES($1,$2,$3,$4,$5,NULLIF($6,0)) RETURNING id`, userID, input.Subject, input.Body, input.Priority, input.Category, input.ServiceID).Scan(&id)
	if err != nil {
		plugin.StatusFail(w, 500, "提交工单失败")
		return
	}
	p.ticketNotify(r.Context(), userID, "created", input.Subject)
	plugin.Emit(r.Context(), EventTicketOpened, TicketPayload{TicketID: id, UserID: userID, Subject: input.Subject})
	plugin.WriteJSON(w, map[string]any{"ok": 1, "id": id})
}

// clientDetail GET /plugin/tickets/{ticketID} — 工单详情（标记管理员回复已读）。
func (p *Plugin) clientDetail(w http.ResponseWriter, r *http.Request) {
	userID, ok := plugin.RequireUserID(w, r)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("ticketID"), 10, 64)
	if err != nil || id <= 0 {
		plugin.StatusFail(w, 400, "工单 ID 无效")
		return
	}
	var subject, body, category, priority, status, serviceName, hostname, created, updated string
	err = p.host.DB.QueryRowContext(r.Context(), `SELECT t.subject,t.body,t.category,t.priority,t.status,coalesce(s.name,''),coalesce(s.hostname,''),to_char(t.created_at,'YYYY-MM-DD HH24:MI'),to_char(t.updated_at,'YYYY-MM-DD HH24:MI') FROM tickets t LEFT JOIN services s ON s.id=t.service_id WHERE t.id=$1 AND t.user_id=$2`, id, userID).Scan(&subject, &body, &category, &priority, &status, &serviceName, &hostname, &created, &updated)
	if err != nil {
		plugin.StatusFail(w, 404, "工单不存在")
		return
	}
	_, _ = p.host.DB.ExecContext(r.Context(), `UPDATE ticket_messages SET read_by_user_at=now(),read_at=now() WHERE ticket_id=$1 AND author_type='admin' AND read_by_user_at IS NULL`, id)
	rows, err := p.host.DB.QueryContext(r.Context(), `SELECT id,coalesce(admin_id,0),content,to_char(created_at,'YYYY-MM-DD HH24:MI') FROM ticket_messages WHERE ticket_id=$1 AND is_internal=false ORDER BY id`, id)
	if err != nil {
		plugin.StatusFail(w, 500, "读取回复失败")
		return
	}
	defer rows.Close()
	messages := make([]map[string]any, 0)
	for rows.Next() {
		var mid, aid int64
		var content, at string
		if err := rows.Scan(&mid, &aid, &content, &at); err != nil {
			plugin.StatusFail(w, 500, "读取回复失败")
			return
		}
		messages = append(messages, map[string]any{"id": mid, "admin_id": aid, "content": content, "created_at": at})
	}
	attachments := make([]map[string]any, 0)
	attachmentRows, err := p.host.DB.QueryContext(r.Context(), `SELECT id,coalesce(message_id,0),original_name,mime,size FROM ticket_attachments WHERE ticket_id=$1 ORDER BY id`, id)
	if err == nil {
		defer attachmentRows.Close()
		for attachmentRows.Next() {
			var aid, messageID, size int64
			var name, mimeType string
			if err := attachmentRows.Scan(&aid, &messageID, &name, &mimeType, &size); err == nil {
				attachments = append(attachments, map[string]any{"id": aid, "message_id": messageID, "name": name, "mime": mimeType, "size": size, "url": "/plugin/tickets/" + strconv.FormatInt(id, 10) + "/attachments/" + strconv.FormatInt(aid, 10)})
			}
		}
	}
	var unread int
	_ = p.host.DB.QueryRowContext(r.Context(), `SELECT count(*) FROM ticket_messages WHERE ticket_id=$1 AND author_type='admin' AND read_by_user_at IS NULL`, id).Scan(&unread)
	plugin.WriteJSON(w, map[string]any{"ok": 1, "ticket": map[string]any{"id": id, "subject": subject, "body": body, "category": category, "priority": priority, "status": status, "service_name": serviceName, "service_hostname": hostname, "created_at": created, "updated_at": updated}, "messages": messages, "attachments": attachments, "unread": unread})
}

// clientReply POST /plugin/tickets/{ticketID}/reply — 用户回复（可带附件）。
func (p *Plugin) clientReply(w http.ResponseWriter, r *http.Request) {
	userID, ok := plugin.RequireUserID(w, r)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("ticketID"), 10, 64)
	if err != nil || id <= 0 {
		plugin.StatusFail(w, 400, "工单 ID 无效")
		return
	}
	content, header, err := replyRequest(r)
	if err != nil {
		plugin.StatusFail(w, 400, err.Error())
		return
	}
	if content == "" && header == nil {
		plugin.StatusFail(w, 400, "回复内容不能为空")
		return
	}
	var previous string
	if err := p.host.DB.QueryRowContext(r.Context(), `SELECT status FROM tickets WHERE id=$1 AND user_id=$2 AND status<>'closed'`, id, userID).Scan(&previous); err != nil {
		plugin.StatusFail(w, 400, "工单不存在或已关闭")
		return
	}
	ref, name, mimeType := "", "", ""
	var size int64
	if header != nil {
		if p.files == nil || p.files.Root == "" {
			plugin.StatusFail(w, 500, "附件服务未配置")
			return
		}
		ref, name, mimeType, size, err = p.files.SaveAttachment(header)
		if err != nil {
			plugin.StatusFail(w, 400, err.Error())
			return
		}
	}
	tx, err := p.host.DB.BeginTx(r.Context(), nil)
	var messageID int64
	if err == nil {
		err = tx.QueryRowContext(r.Context(), `INSERT INTO ticket_messages(ticket_id,user_id,content,author_type) VALUES($1,$2,$3,'user') RETURNING id`, id, userID, content).Scan(&messageID)
	}
	if err == nil {
		_, err = tx.ExecContext(r.Context(), `UPDATE tickets SET status='pending',updated_at=now(),last_reply_by='user' WHERE id=$1`, id)
	}
	if err == nil && ref != "" {
		_, err = tx.ExecContext(r.Context(), `INSERT INTO ticket_attachments(ticket_id,message_id,user_id,storage_ref,original_name,mime,size) VALUES($1,$2,$3,$4,$5,$6,$7)`, id, messageID, userID, ref, name, mimeType, size)
	}
	if err == nil {
		if previous != "pending" {
			_, err = tx.ExecContext(r.Context(), `INSERT INTO ticket_status_history(ticket_id,from_status,to_status,note) VALUES($1,$2,'pending','用户回复')`, id, previous)
		}
	}
	if err == nil {
		err = tx.Commit()
	} else if tx != nil {
		_ = tx.Rollback()
	}
	if err != nil && ref != "" {
		_ = p.files.Delete(ref)
	}
	if err != nil {
		plugin.StatusFail(w, 500, "回复失败")
		return
	}
	var subject string
	_ = p.host.DB.QueryRowContext(r.Context(), `SELECT subject FROM tickets WHERE id=$1`, id).Scan(&subject)
	plugin.Emit(r.Context(), EventTicketReplied, TicketPayload{TicketID: id, UserID: userID, Subject: subject, ByAdmin: false})
	plugin.WriteJSON(w, map[string]any{"ok": 1})
}

// replyRequest 解析回复请求（JSON 或 multipart 带附件）。
func replyRequest(r *http.Request) (string, *multipart.FileHeader, error) {
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		if err := r.ParseMultipartForm(12 << 20); err != nil {
			return "", nil, errors.New("回复请求无效")
		}
		_, header, err := r.FormFile("file")
		if err != nil && err != http.ErrMissingFile {
			return "", nil, errors.New("读取附件失败")
		}
		return strings.TrimSpace(r.FormValue("content")), header, nil
	}
	var input struct {
		Content string `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		return "", nil, errors.New("请求格式无效")
	}
	return strings.TrimSpace(input.Content), nil, nil
}

// clientClose POST /{ticketID}/close；clientReopen POST /{ticketID}/reopen。
func (p *Plugin) clientClose(w http.ResponseWriter, r *http.Request)  { p.changeStatus(w, r, "closed") }
func (p *Plugin) clientReopen(w http.ResponseWriter, r *http.Request) { p.changeStatus(w, r, "pending") }

func (p *Plugin) changeStatus(w http.ResponseWriter, r *http.Request, target string) {
	userID, ok := plugin.RequireUserID(w, r)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("ticketID"), 10, 64)
	if err != nil || id <= 0 {
		plugin.StatusFail(w, 400, "工单 ID 无效")
		return
	}
	var previous string
	if err := p.host.DB.QueryRowContext(r.Context(), `SELECT status FROM tickets WHERE id=$1 AND user_id=$2`, id, userID).Scan(&previous); err != nil {
		plugin.StatusFail(w, 404, "工单不存在")
		return
	}
	if target == "closed" && previous == "closed" {
		plugin.StatusFail(w, 400, "工单已经关闭")
		return
	}
	if target == "pending" && previous != "closed" {
		plugin.StatusFail(w, 400, "工单当前无需重新打开")
		return
	}
	closeReason := ""
	if target == "closed" {
		var input struct {
			Reason string `json:"reason"`
		}
		_ = json.NewDecoder(r.Body).Decode(&input)
		closeReason = strings.TrimSpace(input.Reason)
		if closeReason == "" {
			plugin.StatusFail(w, 400, "请填写关闭原因")
			return
		}
		if len(closeReason) > 500 {
			plugin.StatusFail(w, 400, "关闭原因不能超过 500 个字符")
			return
		}
	}
	tx, err := p.host.DB.BeginTx(r.Context(), nil)
	if err == nil {
		_, err = tx.ExecContext(r.Context(), `UPDATE tickets SET status=$1,closed_at=CASE WHEN $1='closed' THEN now() ELSE NULL END,closed_reason=CASE WHEN $1='closed' THEN $2 ELSE '' END,updated_at=now(),last_reply_by='user' WHERE id=$3 AND user_id=$4`, target, closeReason, id, userID)
	}
	if err == nil {
		note := map[bool]string{true: "用户关闭：" + closeReason, false: "用户重新打开"}[target == "closed"]
		_, err = tx.ExecContext(r.Context(), `INSERT INTO ticket_status_history(ticket_id,from_status,to_status,note) VALUES($1,$2,$3,$4)`, id, previous, target, note)
	}
	if err == nil {
		err = tx.Commit()
	} else if tx != nil {
		_ = tx.Rollback()
	}
	if err != nil {
		plugin.StatusFail(w, 500, "更新工单失败")
		return
	}
	plugin.WriteJSON(w, map[string]any{"ok": 1, "status": target})
}

// clientAttachment POST /{ticketID}/attachments — 上传附件。
func (p *Plugin) clientAttachment(w http.ResponseWriter, r *http.Request) {
	userID, ok := plugin.RequireUserID(w, r)
	if !ok {
		return
	}
	if p.files == nil || p.files.Root == "" {
		plugin.JSONFail(w, "附件存储未配置")
		return
	}
	id, err := strconv.ParseInt(r.PathValue("ticketID"), 10, 64)
	if err != nil || id <= 0 {
		plugin.StatusFail(w, 400, "工单 ID 无效")
		return
	}
	var open bool
	if err := p.host.DB.QueryRowContext(r.Context(), `SELECT status<>'closed' FROM tickets WHERE id=$1 AND user_id=$2`, id, userID).Scan(&open); err != nil || !open {
		plugin.StatusFail(w, 400, "工单不存在或已关闭")
		return
	}
	if err := r.ParseMultipartForm(12 << 20); err != nil {
		plugin.StatusFail(w, 400, "附件请求无效")
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		plugin.StatusFail(w, 400, "请选择附件")
		return
	}
	file.Close()
	ref, name, mimeType, size, err := p.files.SaveAttachment(header)
	if err != nil {
		plugin.StatusFail(w, 400, err.Error())
		return
	}
	var attachmentID int64
	if err := p.host.DB.QueryRowContext(r.Context(), `INSERT INTO ticket_attachments(ticket_id,user_id,storage_ref,original_name,mime,size) VALUES($1,$2,$3,$4,$5,$6) RETURNING id`, id, userID, ref, name, mimeType, size).Scan(&attachmentID); err != nil {
		_ = p.files.Delete(ref)
		plugin.StatusFail(w, 500, "保存附件失败")
		return
	}
	plugin.WriteJSON(w, map[string]any{"ok": 1, "id": attachmentID, "name": name, "size": size})
}

// clientAttachmentDownload GET /{ticketID}/attachments/{attachmentID} — 下载（仅工单属主）。
func (p *Plugin) clientAttachmentDownload(w http.ResponseWriter, r *http.Request) {
	userID, ok := plugin.RequireUserID(w, r)
	if !ok {
		return
	}
	if p.files == nil || p.files.Root == "" {
		plugin.JSONFail(w, "附件存储未配置")
		return
	}
	id, _ := strconv.ParseInt(r.PathValue("ticketID"), 10, 64)
	aid, _ := strconv.ParseInt(r.PathValue("attachmentID"), 10, 64)
	var ref, name, mimeType string
	if err := p.host.DB.QueryRowContext(r.Context(), `SELECT a.storage_ref,a.original_name,a.mime FROM ticket_attachments a JOIN tickets t ON t.id=a.ticket_id WHERE a.id=$1 AND a.ticket_id=$2 AND t.user_id=$3`, aid, id, userID).Scan(&ref, &name, &mimeType); err != nil {
		http.NotFound(w, r)
		return
	}
	serveAttachment(w, r, p.files, ref, name, mimeType)
}
