package spaceship

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ---- 测试辅助 ----

func postJSON(t *testing.T, body any) *http.Request {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(string(raw)))
	r.Header.Set("Content-Type", "application/json")
	return r
}

func getReq(path string) *http.Request {
	return httptest.NewRequest(http.MethodGet, path, nil)
}

// call 执行 handler 并解析响应体（{ok:...} 约定）。
func call(h http.HandlerFunc, r *http.Request) (int, map[string]any) {
	w := httptest.NewRecorder()
	h(w, r)
	var m map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &m)
	return w.Code, m
}

// createTestContact 直接落一条本地联系人（绕过 Spaceship 上游），返回本地 id。
// userID 无效时代表「管理员共享模板」（user_id=NULL）。
func createTestContact(t *testing.T, d *sql.DB, userID sql.NullInt64, isDefault bool) int64 {
	t.Helper()
	// 用 t 名称 + 随机数 + 纳秒避免 -count>1 / -shuffle 并发时 contact_id 重复
	// （plugin_spaceship_contacts.contact_id 有 UNIQUE 约束）。
	cid := fmt.Sprintf("SP-TEST-%s-%d", t.Name(), time.Now().UnixNano())
	var id int64
	err := d.QueryRowContext(context.Background(),
		`INSERT INTO plugin_spaceship_contacts
		 (contact_id,user_id,label,first_name,last_name,email,address1,city,country,phone,is_default)
		 VALUES($1,$2,'t','Test','User','t@lumeidc.test','addr','City','CN','+86.13800000000',$3)
		 RETURNING id`, cid, userID, isDefault).Scan(&id)
	if err != nil {
		t.Fatalf("创建测试联系人失败: %v", err)
	}
	t.Cleanup(func() {
		_, _ = d.ExecContext(context.Background(), `DELETE FROM plugin_spaceship_contacts WHERE id=$1`, id)
	})
	return id
}

// =====================================================================
// P0-1 后台代注册不扣款：管理员为用户注册域名后，用户余额纹丝不动
// 期望：按 paidAmount 从归属用户余额扣款（白送域名＝直接资损）
// =====================================================================

func TestP0_AdminRegister_MustDeductBalance(t *testing.T) {
	up := newFakeUpstream()
	defer up.Close()
	withFakeUpstream(t, up)
	p, d := newTestPlugin(t, up, nil)

	uid := createTestUser(t, d, "adminreg", "100.00")
	cid := createTestContact(t, d, sql.NullInt64{}, true)

	before := balanceOf(t, d, uid)
	code, res := call(p.adminRegisterDomain, withAdmin(postJSON(t, map[string]any{
		"domain":     "admin-p0-a.com",
		"years":      1,
		"contactId":  cid,
		"userId":     uid,
		"paidAmount": "30.00",
	})))
	if code != http.StatusOK || res["ok"] != float64(1) {
		t.Fatalf("后台代注册应成功，实际 code=%d body=%v", code, res)
	}
	after := balanceOf(t, d, uid)

	// 期望扣款额＝服务端价目表（.com 注册价），与客户端 paidAmount 无关
	price, err := p.repo.GetPrice(context.Background(), "com")
	if err != nil {
		t.Fatalf("读取价目失败: %v", err)
	}
	want := price.RegisterCents
	if after != before-want {
		t.Errorf("P0 资损：后台代注册扣款异常，余额 before=%d after=%d（服务端价应扣 %d 分）", before, after, want)
	}
	if after >= before {
		t.Errorf("P0 资损：后台代注册未从归属用户扣款，余额 before=%d after=%d", before, after)
	}
}

// =====================================================================
// P0-2 价格由客户端决定：同一 TLD + 同样年限，客户端填多少就扣多少
// 期望：金额由服务端定价决定，两个不同 paidAmount 的扣款必须一致
// =====================================================================

func TestP0_Register_PriceMustComeFromServer(t *testing.T) {
	up := newFakeUpstream()
	defer up.Close()
	withFakeUpstream(t, up)
	p, d := newTestPlugin(t, up, nil)

	u1 := createTestUser(t, d, "price-a", "500.00")
	c1 := createTestContact(t, d, sql.NullInt64{Int64: u1, Valid: true}, true)
	u2 := createTestUser(t, d, "price-b", "500.00")
	c2 := createTestContact(t, d, sql.NullInt64{Int64: u2, Valid: true}, true)

	// 用户 A：声称只付 1 元
	b1 := balanceOf(t, d, u1)
	_, r1 := call(p.clientRegister, withUser(postJSON(t, map[string]any{
		"domain": "price-p0-a.com", "years": 1, "paidAmount": "1.00", "contactId": c1,
	}), u1))
	if r1["ok"] != float64(1) {
		t.Fatalf("注册 A 失败: %v", r1)
	}
	paid1 := b1 - balanceOf(t, d, u1)

	// 用户 B：同一 TLD、同样年限，声称付 88 元
	b2 := balanceOf(t, d, u2)
	_, r2 := call(p.clientRegister, withUser(postJSON(t, map[string]any{
		"domain": "price-p0-b.com", "years": 1, "paidAmount": "88.00", "contactId": c2,
	}), u2))
	if r2["ok"] != float64(1) {
		t.Fatalf("注册 B 失败: %v", r2)
	}
	paid2 := b2 - balanceOf(t, d, u2)

	if paid1 != paid2 {
		t.Errorf("P0 定价失控：同一 TLD 同样年限，客户端传 1 元扣了 %d 分、传 88 元扣了 %d 分，价格由客户端决定", paid1, paid2)
	}
}

// =====================================================================
// P0-3 注册顺序反转：余额不足时上游已经把域名注册走了（孤儿域名 + 白嫖）
// 期望：先扣款再调上游；余额不足时绝不产生上游注册请求
// =====================================================================

func TestP0_Register_MustNotCallUpstreamWhenBalanceInsufficient(t *testing.T) {
	up := newFakeUpstream()
	defer up.Close()
	withFakeUpstream(t, up)
	p, d := newTestPlugin(t, up, nil)

	uid := createTestUser(t, d, "poor", "0.00")
	cid := createTestContact(t, d, sql.NullInt64{Int64: uid, Valid: true}, true)

	code, res := call(p.clientRegister, withUser(postJSON(t, map[string]any{
		"domain": "poor-p0.com", "years": 1, "paidAmount": "10.00", "contactId": cid,
	}), uid))

	if up.registerCount() != 0 {
		t.Errorf("P0 孤儿域名：余额为 0 时上游仍被调注册 %d 次（域名已注册但钱没收到）", up.registerCount())
	}
	if code == http.StatusOK && res["ok"] == float64(1) {
		t.Errorf("余额不足却返回成功: %v", res)
	}
}

// 上游注册同步失败时，已扣的钱必须回到用户账上（不得钱货两空）。
func TestP0_Register_UpstreamFailureMustRefund(t *testing.T) {
	up := newFakeUpstream()
	defer up.Close()
	withFakeUpstream(t, up)
	p, d := newTestPlugin(t, up, nil)

	uid := createTestUser(t, d, "failrefund", "100.00")
	cid := createTestContact(t, d, sql.NullInt64{Int64: uid, Valid: true}, true)

	up.setFailMessage("domain is not available")
	before := balanceOf(t, d, uid)

	_, res := call(p.clientRegister, withUser(postJSON(t, map[string]any{
		"domain": "failrefund-p0.com", "years": 1, "paidAmount": "20.00", "contactId": cid,
	}), uid))
	if res["ok"] == float64(1) {
		t.Fatalf("上游失败时不应返回成功: %v", res)
	}

	after := balanceOf(t, d, uid)
	if after != before {
		t.Errorf("P0 钱货两空：上游注册失败但已扣款未退回，before=%d after=%d", before, after)
	}
}

// P1 轮询发现异步失败 → 真实退款链路：认领订单 + 回补余额必须在真库跑通，
// 且重复退款（并发/人工重试）不得双倍入账（RefundOrderIfPaid 条件 UPDATE 认领）。
func TestP1_PollFailed_RefundsRealOrderIdempotently(t *testing.T) {
	up := newFakeUpstream()
	defer up.Close()
	withFakeUpstream(t, up)
	p, d := newTestPlugin(t, up, nil)

	uid := createTestUser(t, d, "pollrefund", "100.00")
	did := insertActiveDomain(t, d, uid, "pollrefund-p1.com", time.Now().AddDate(1, 0, 0))

	// 模拟已扣款：余额 100.00 - 88.00 = 12.00；订单记录 8800 分
	if _, err := d.ExecContext(context.Background(),
		`UPDATE users SET balance = balance - 88 WHERE id=$1`, uid); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ExecContext(context.Background(),
		`INSERT INTO plugin_spaceship_orders (domain_id,domain,user_id,kind,years,amount_cents,status)
		 VALUES($1,'pollrefund-p1.com',$2,'register',1,8800,'paid')`, did, uid); err != nil {
		t.Fatal(err)
	}

	const opIDStr = "op-failed-refund-1"
	if _, err := d.ExecContext(context.Background(),
		`INSERT INTO plugin_spaceship_operations (operation_id,domain_id,domain,op_type,status,started_at)
		 VALUES($1,$2,'pollrefund-p1.com','domain_create','pending',now())`, opIDStr, did); err != nil {
		t.Fatal(err)
	}
	up.SetOpStatus(opIDStr, "failed")

	if err := p.pollOperations(context.Background()); err != nil {
		t.Fatalf("轮询不应报错: %v", err)
	}

	var status string
	if err := d.QueryRowContext(context.Background(),
		`SELECT status FROM plugin_spaceship_operations WHERE operation_id=$1`, opIDStr).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "failed" {
		t.Fatalf("上游 failed 应落库为 failed: %s", status)
	}
	if got := balanceOf(t, d, uid); got != 10000 {
		t.Errorf("P1 资金悬挂：异步失败退款后余额应=10000 分，got %d", got)
	}

	// 幂等：同一 op 再触发一次退款（模拟 cron 与人工并发），不得双倍入账
	p.refundByDomainOperation(context.Background(), &OperationRow{
		DomainID: sql.NullInt64{Int64: did, Valid: true}, OpType: "domain_create",
	})
	if got := balanceOf(t, d, uid); got != 10000 {
		t.Errorf("P0 双倍退款：重复退款后余额=%d 分（应仍为 10000）", got)
	}
}

// P1 轮询超时（pending >15min）→ 真库退款链路：标 failed + 订单认领 + 余额回补。
func TestP1_PollTimeout_RefundsRealOrder(t *testing.T) {
	up := newFakeUpstream()
	defer up.Close()
	withFakeUpstream(t, up)
	p, d := newTestPlugin(t, up, nil)

	uid := createTestUser(t, d, "timeoutref", "100.00")
	did := insertActiveDomain(t, d, uid, "timeoutref-p1.com", time.Now().AddDate(1, 0, 0))

	if _, err := d.ExecContext(context.Background(),
		`UPDATE users SET balance = balance - 88 WHERE id=$1`, uid); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ExecContext(context.Background(),
		`INSERT INTO plugin_spaceship_orders (domain_id,domain,user_id,kind,years,amount_cents,status)
		 VALUES($1,'timeoutref-p1.com',$2,'renew',1,8800,'paid')`, did, uid); err != nil {
		t.Fatal(err)
	}
	// 超出 15min 窗口的 pending 续费操作；fake 上游默认返回 pending
	if _, err := d.ExecContext(context.Background(),
		`INSERT INTO plugin_spaceship_operations (operation_id,domain_id,domain,op_type,status,started_at)
		 VALUES('op-timeout-1',$1,'timeoutref-p1.com','domain_renew','pending',$2)`,
		did, time.Now().Add(-16*time.Minute)); err != nil {
		t.Fatal(err)
	}

	if err := p.pollOperations(context.Background()); err != nil {
		t.Fatalf("轮询不应报错: %v", err)
	}

	var status string
	if err := d.QueryRowContext(context.Background(),
		`SELECT status FROM plugin_spaceship_operations WHERE operation_id='op-timeout-1'`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "failed" {
		t.Fatalf("超时应标 failed: %s", status)
	}
	if got := balanceOf(t, d, uid); got != 10000 {
		t.Errorf("P1 资金悬挂：超时退款后余额应=10000 分，got %d", got)
	}
}

// =====================================================================
// P0-4 共享联系人越权：普通用户可删除/占用管理员共享联系人（user_id IS NULL）
// 期望：共享模板对普通用户只读，删除与设为默认均需拒绝
// =====================================================================

func TestP0_SharedContact_MustNotBeDeletableByUser(t *testing.T) {
	up := newFakeUpstream()
	defer up.Close()
	withFakeUpstream(t, up)
	p, d := newTestPlugin(t, up, nil)

	uid := createTestUser(t, d, "shared", "100.00")
	shared := createTestContact(t, d, sql.NullInt64{}, false)

	r := httptest.NewRequest(http.MethodPost, "/my/contacts/1/delete", nil)
	r.SetPathValue("id", fmt.Sprint(shared))
	code, res := call(p.clientDeleteContact, withUser(r, uid))

	if res["ok"] == float64(1) {
		t.Errorf("P0 越权：普通用户删除了管理员共享联系人（code=%d body=%v），其他用户的注册将全部失败", code, res)
	}
}

func TestP0_SharedContact_MustNotBecomeUserDefault(t *testing.T) {
	up := newFakeUpstream()
	defer up.Close()
	withFakeUpstream(t, up)
	p, d := newTestPlugin(t, up, nil)

	uid := createTestUser(t, d, "shared2", "100.00")
	shared := createTestContact(t, d, sql.NullInt64{}, false)

	r := httptest.NewRequest(http.MethodPost, "/my/contacts/1/default", strings.NewReader(`{}`))
	r.SetPathValue("id", fmt.Sprint(shared))
	_, res := call(p.clientSetDefaultContact, withUser(r, uid))

	if res["ok"] == float64(1) {
		t.Errorf("P0 越权：普通用户把管理员共享联系人设为了自己的默认（body=%v）", res)
	}
}

// 后台改默认联系人时不得波及全部用户：目前会清空所有人的 is_default。
func TestP0_AdminSetDefault_MustBeScoped(t *testing.T) {
	up := newFakeUpstream()
	defer up.Close()
	withFakeUpstream(t, up)
	p, d := newTestPlugin(t, up, nil)

	uid := createTestUser(t, d, "admindef", "100.00")
	mine := createTestContact(t, d, sql.NullInt64{Int64: uid, Valid: true}, true)
	other := createTestContact(t, d, sql.NullInt64{}, false)

	r := httptest.NewRequest(http.MethodPost, "/contacts/1/default", strings.NewReader(`{}`))
	r.SetPathValue("id", fmt.Sprint(other))
	_, _ = call(p.adminSetDefaultContact, withAdmin(r))

	// 传空 userID 表示"共享模板"作用域，后台改默认不应清空普通用户的默认设置。
	var mineStill bool
	if err := d.QueryRowContext(context.Background(),
		`SELECT is_default FROM plugin_spaceship_contacts WHERE id=$1`, mine).Scan(&mineStill); err != nil {
		t.Fatal(err)
	}
	if !mineStill {
		t.Errorf("P0 越权：后台设置共享默认联系人时清空了普通用户的默认联系人")
	}
}

// =====================================================================
// P1 续费：必须按服务端续费价扣款，且调用官方 renew 接口（带 currentExpirationDate）
// =====================================================================

// insertActiveDomain 插入一条 active 域名（含到期时间），返回本地 id。
func insertActiveDomain(t *testing.T, d *sql.DB, userID int64, domain string, expiresAt time.Time) int64 {
	t.Helper()
	var id int64
	err := d.QueryRowContext(context.Background(),
		`INSERT INTO plugin_spaceship_domains
		 (user_id,domain,years,paid_amount_cents,status,privacy_level,auto_renew,spaceship_domain_id,registered_at,expires_at)
		 VALUES($1,$2,1,8800,'active','high',false,'sp-x',$3,$4) RETURNING id`,
		userID, domain, time.Now().AddDate(-1, 0, 0), expiresAt).Scan(&id)
	if err != nil {
		t.Fatalf("插入测试域名失败: %v", err)
	}
	t.Cleanup(func() {
		_, _ = d.ExecContext(context.Background(), `DELETE FROM plugin_spaceship_orders WHERE domain=$1`, domain)
		_, _ = d.ExecContext(context.Background(), `DELETE FROM plugin_spaceship_domains WHERE id=$1`, id)
	})
	return id
}

func TestP1_Renew_MustChargeServerPriceAndCallUpstream(t *testing.T) {
	up := newFakeUpstream()
	defer up.Close()
	withFakeUpstream(t, up)
	p, d := newTestPlugin(t, up, nil)

	uid := createTestUser(t, d, "renew", "500.00")
	did := insertActiveDomain(t, d, uid, "renew-p1.com", time.Now().AddDate(0, 6, 0))

	price, err := p.repo.GetPrice(context.Background(), "com")
	if err != nil {
		t.Fatalf("读取价目失败: %v", err)
	}

	before := balanceOf(t, d, uid)
	r := postJSON(t, map[string]any{"years": 1})
	r.SetPathValue("id", fmt.Sprint(did))
	code, res := call(p.clientRenew, withUser(r, uid))
	if code != http.StatusOK || res["ok"] != float64(1) {
		t.Fatalf("续费应成功: code=%d body=%v", code, res)
	}
	if got := before - balanceOf(t, d, uid); got != price.RenewCents {
		t.Errorf("P1 续费定价异常：扣了 %d 分，服务端续费价应为 %d 分", got, price.RenewCents)
	}
	if up.renewCount() != 1 {
		t.Errorf("P1 续费未调用官方 renew 接口（调用 %d 次）", up.renewCount())
	}
}

// 续费别人的域名必须拒绝（越权）。
func TestP1_Renew_OtherUsersDomainMustBeRejected(t *testing.T) {
	up := newFakeUpstream()
	defer up.Close()
	withFakeUpstream(t, up)
	p, d := newTestPlugin(t, up, nil)

	owner := createTestUser(t, d, "renew-owner", "500.00")
	other := createTestUser(t, d, "renew-other", "500.00")
	did := insertActiveDomain(t, d, owner, "renew-other-p1.com", time.Now().AddDate(0, 6, 0))

	before := balanceOf(t, d, owner)
	r := postJSON(t, map[string]any{"years": 1})
	r.SetPathValue("id", fmt.Sprint(did))
	_, res := call(p.clientRenew, withUser(r, other))
	if res["ok"] == float64(1) {
		t.Fatalf("P1 越权：续费了他人域名 body=%v", res)
	}
	if balanceOf(t, d, owner) != before {
		t.Errorf("P1 越权：他人续费影响了域名所有者的余额")
	}
}

// 未生效（pending）的域名不允许续费：上游会拒绝，且扣款无意义。
func TestP1_Renew_PendingDomainMustBeRejected(t *testing.T) {
	up := newFakeUpstream()
	defer up.Close()
	withFakeUpstream(t, up)
	p, d := newTestPlugin(t, up, nil)

	uid := createTestUser(t, d, "renew-pending", "500.00")
	var did int64
	if err := d.QueryRowContext(context.Background(),
		`INSERT INTO plugin_spaceship_domains (user_id,domain,years,paid_amount_cents,status,privacy_level,auto_renew)
		 VALUES($1,'renew-pending-p1.com',1,8800,'pending','high',false) RETURNING id`, uid).Scan(&did); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = d.ExecContext(context.Background(), `DELETE FROM plugin_spaceship_domains WHERE id=$1`, did)
	})

	before := balanceOf(t, d, uid)
	r := postJSON(t, map[string]any{"years": 1})
	r.SetPathValue("id", fmt.Sprint(did))
	_, res := call(p.clientRenew, withUser(r, uid))
	if res["ok"] == float64(1) {
		t.Fatalf("P1 状态机：pending 域名却续费成功 body=%v", res)
	}
	if balanceOf(t, d, uid) != before {
		t.Errorf("P1 状态机：pending 域名续费失败却扣了款")
	}
}

// =====================================================================
// 协议契约：请求体字段名/类型必须与官方文档一致（错一处真实凭证下直接 400）
// =====================================================================

// 续费：currentExpirationDate 必须是 RFC3339 字符串（官方 string <date-time>）。
// 早期实现传 Unix 毫秒整数，真实上游会判类型错误而 400，续费永远失败。
func TestP1_Renew_ExpirationDateMustBeRFC3339String(t *testing.T) {
	up := newFakeUpstream()
	defer up.Close()
	withFakeUpstream(t, up)
	p, d := newTestPlugin(t, up, nil)

	uid := createTestUser(t, d, "renew-proto", "500.00")
	exp := time.Now().AddDate(0, 6, 0)
	did := insertActiveDomain(t, d, uid, "renew-proto.com", exp)

	r := postJSON(t, map[string]any{"years": 2})
	r.SetPathValue("id", fmt.Sprint(did))
	code, res := call(p.clientRenew, withUser(r, uid))
	if code != http.StatusOK || res["ok"] != float64(1) {
		t.Fatalf("续费应成功: code=%d body=%v", code, res)
	}

	body := up.snapshotLastRenew()
	if body == nil {
		t.Fatal("未向官方发送续费请求")
	}
	if body["years"] != float64(2) {
		t.Errorf("续费年限未透传: %v", body["years"])
	}
	raw, ok := body["currentExpirationDate"]
	if !ok {
		t.Fatalf("续费请求缺少官方必填字段 currentExpirationDate: %v", body)
	}
	expStr, isStr := raw.(string)
	if !isStr {
		t.Fatalf("currentExpirationDate 必须是 string <date-time>（官方类型），实际 %T=%v", raw, raw)
	}
	parsed, err := time.Parse(time.RFC3339, expStr)
	if err != nil {
		t.Fatalf("currentExpirationDate 必须是 RFC3339 日期时间: %q (%v)", expStr, err)
	}
	// 值必须是本域名的当前到期时间（以本地库为准，秒级格式化允许 1 秒误差）
	if diff := parsed.Sub(exp); diff > 2*time.Second || diff < -2*time.Second {
		t.Errorf("currentExpirationDate 与本地到期时间不符: 上游 %v 本地 %v", parsed, exp)
	}
}

// 自动续费：官方请求体字段名是 isEnabled，不是 autoRenew。
func TestP1_AutoRenew_MustSendIsEnabledField(t *testing.T) {
	up := newFakeUpstream()
	defer up.Close()
	withFakeUpstream(t, up)
	p, d := newTestPlugin(t, up, nil)

	uid := createTestUser(t, d, "ar-proto", "500.00")
	did := insertActiveDomain(t, d, uid, "ar-proto.com", time.Now().AddDate(0, 6, 0))

	r := postJSON(t, map[string]any{"enable": true})
	r.SetPathValue("id", fmt.Sprint(did))
	code, res := call(p.clientAutoRenew, withUser(r, uid))
	if code != http.StatusOK || res["ok"] != float64(1) {
		t.Fatalf("开启自动续费应成功: code=%d body=%v", code, res)
	}
	if up.autoRenewCount() != 1 {
		t.Fatalf("未调用官方 autorenew 接口（调用 %d 次）", up.autoRenewCount())
	}
	if st, ok := up.autoRenewStateOf("ar-proto.com"); !ok || !st {
		t.Errorf("上游侧未收到 isEnabled=true（state=%v ok=%v）", st, ok)
	}

	// 关闭方向同样要透传
	r2 := postJSON(t, map[string]any{"enable": false})
	r2.SetPathValue("id", fmt.Sprint(did))
	c2, res2 := call(p.clientAutoRenew, withUser(r2, uid))
	if c2 != http.StatusOK || res2["ok"] != float64(1) {
		t.Fatalf("关闭自动续费应成功: code=%d body=%v", c2, res2)
	}
	if st, ok := up.autoRenewStateOf("ar-proto.com"); !ok || st {
		t.Errorf("上游侧未收到 isEnabled=false（state=%v ok=%v）", st, ok)
	}
}

// 旧字段名 autoRenew 必须被判 400。若这里放行，说明契约校验失效：
// 真实环境会表现为"点了自动续费、本地显示已开启、上游其实没生效"。
func TestP1_AutoRenew_LegacyFieldNameMustBeRejected(t *testing.T) {
	up := newFakeUpstream()
	defer up.Close()

	req, err := http.NewRequest(http.MethodPut,
		up.server.URL+"/api/v1/domains/legacy.com/autorenew", strings.NewReader(`{"autoRenew":true}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Api-Key", "k")
	req.Header.Set("X-Api-Secret", "s")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("旧字段名 autoRenew 应被判 400（官方只认 isEnabled），实际 %d", resp.StatusCode)
	}
}

// 客户端层护栏：年限越界与缺失到期时间必须在发出请求前就报错，
// 不能把明显会被官方拒的请求发出去（避免先扣款再失败）。
func TestClient_RenewDomain_PreflightGuards(t *testing.T) {
	c := NewClient("k", "s")
	if _, err := c.RenewDomain(context.Background(), "a.com", 1, time.Time{}); err == nil {
		t.Error("缺少当前到期时间应报错（官方必填 currentExpirationDate）")
	}
	if _, err := c.RenewDomain(context.Background(), "a.com", 0, time.Now()); err == nil {
		t.Error("年限 0 应报错")
	}
	if _, err := c.RenewDomain(context.Background(), "a.com", 11, time.Now()); err == nil {
		t.Error("年限 11 应报错（官方上限 10）")
	}
}

// =====================================================================
// P1 价目表：未上架后缀必须拒绝注册；价格只能由后台维护
// =====================================================================

func TestP1_Register_DisabledOrUnknownTLDMustBeRejected(t *testing.T) {
	up := newFakeUpstream()
	defer up.Close()
	withFakeUpstream(t, up)
	p, d := newTestPlugin(t, up, nil)

	uid := createTestUser(t, d, "tld", "500.00")
	cid := createTestContact(t, d, sql.NullInt64{Int64: uid, Valid: true}, true)

	// 未配置价目的后缀
	before := balanceOf(t, d, uid)
	_, res := call(p.clientRegister, withUser(postJSON(t, map[string]any{
		"domain": "tld-p1.zzz", "years": 1, "contactId": cid,
	}), uid))
	if res["ok"] == float64(1) {
		t.Fatalf("P1 定价：未配置价目的后缀却注册成功 body=%v", res)
	}
	if balanceOf(t, d, uid) != before {
		t.Errorf("P1 定价：未配置价目的后缀注册失败却扣了款")
	}
	if up.registerCount() != 0 {
		t.Errorf("P1 定价：未配置价目的后缀仍调用了上游注册")
	}

	// 上架后关闭
	if _, err := p.repo.UpsertPrice(context.Background(), &PriceRow{
		TLD: "xyz", RegisterCents: 1000, RenewCents: 1000, Enabled: false,
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = d.ExecContext(context.Background(), `DELETE FROM plugin_spaceship_prices WHERE tld='xyz'`)
	})
	_, res2 := call(p.clientRegister, withUser(postJSON(t, map[string]any{
		"domain": "tld-p1.xyz", "years": 1, "contactId": cid,
	}), uid))
	if res2["ok"] == float64(1) {
		t.Fatalf("P1 定价：已下架后缀却注册成功 body=%v", res2)
	}
}

// 后台改价后，注册按新价扣款（证明价格确实来自服务端价目表）。
func TestP1_Register_PriceChangeTakesEffect(t *testing.T) {
	up := newFakeUpstream()
	defer up.Close()
	withFakeUpstream(t, up)
	p, d := newTestPlugin(t, up, nil)

	if _, err := p.repo.UpsertPrice(context.Background(), &PriceRow{
		TLD: "io", RegisterCents: 12345, RenewCents: 13000, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = d.ExecContext(context.Background(), `DELETE FROM plugin_spaceship_prices WHERE tld='io'`)
	})

	uid := createTestUser(t, d, "io", "500.00")
	cid := createTestContact(t, d, sql.NullInt64{Int64: uid, Valid: true}, true)

	before := balanceOf(t, d, uid)
	_, res := call(p.clientRegister, withUser(postJSON(t, map[string]any{
		"domain": "price-io-p1.io", "years": 1, "paidAmount": "1.00", "contactId": cid,
	}), uid))
	if res["ok"] != float64(1) {
		t.Fatalf("注册失败: %v", res)
	}
	if got := before - balanceOf(t, d, uid); got != 12345 {
		t.Errorf("P1 定价：后台改价未生效，扣了 %d 分（应为 12345 分）", got)
	}
}

// 年限必须落在价目表区间内（防止"选 10 年按 1 年价"的套利）。
func TestP1_Register_YearsMustRespectPriceRange(t *testing.T) {
	up := newFakeUpstream()
	defer up.Close()
	withFakeUpstream(t, up)
	p, d := newTestPlugin(t, up, nil)

	if _, err := p.repo.UpsertPrice(context.Background(), &PriceRow{
		TLD: "dev", RegisterCents: 5000, RenewCents: 5000, Enabled: true, MinYears: 1, MaxYears: 2,
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = d.ExecContext(context.Background(), `DELETE FROM plugin_spaceship_prices WHERE tld='dev'`)
	})

	uid := createTestUser(t, d, "dev", "500.00")
	cid := createTestContact(t, d, sql.NullInt64{Int64: uid, Valid: true}, true)

	before := balanceOf(t, d, uid)
	_, res := call(p.clientRegister, withUser(postJSON(t, map[string]any{
		"domain": "years-p1.dev", "years": 5, "contactId": cid,
	}), uid))
	if res["ok"] == float64(1) {
		t.Fatalf("P1 年限：超出价目表上限（max 2 年）却注册成功 body=%v", res)
	}
	if balanceOf(t, d, uid) != before {
		t.Errorf("P1 年限：超年限注册失败却扣了款")
	}
}

// =====================================================================
// P1 溢价域名：未经管理员确认不得自助注册（溢价价不受价目表控制）
// =====================================================================

func TestP1_Register_PremiumRequiresAdminConsent(t *testing.T) {
	up := newFakeUpstream()
	defer up.Close()
	withFakeUpstream(t, up)
	p, d := newTestPlugin(t, up, nil)

	uid := createTestUser(t, d, "premium", "500.00")
	cid := createTestContact(t, d, sql.NullInt64{Int64: uid, Valid: true}, true)
	up.setAvailability("premium-p1.com", &CheckResult{
		Domain: "premium-p1.com", Result: "available", Premium: true,
		PremiumPricing: &Pricing{PremiumPrice: 9999, Currency: "USD"},
	})

	before := balanceOf(t, d, uid)
	_, res := call(p.clientRegister, withUser(postJSON(t, map[string]any{
		"domain": "premium-p1.com", "years": 1, "contactId": cid,
	}), uid))
	if res["ok"] == float64(1) {
		t.Fatalf("P1 溢价：未确认价格却直接注册成功（按普通价扣款＝资损）body=%v", res)
	}
	if balanceOf(t, d, uid) != before {
		t.Errorf("P1 溢价：被拒绝的溢价注册却扣了款")
	}
}

// 溢价域名闭环：管理员查到溢价报价 → 确认后代注册（商业上必须能卖溢价域名）。
func TestP1_Premium_AdminCanRegisterAfterConfirm(t *testing.T) {
	up := newFakeUpstream()
	defer up.Close()
	withFakeUpstream(t, up)
	p, d := newTestPlugin(t, up, nil)

	uid := createTestUser(t, d, "premium-adm", "500.00")
	cid := createTestContact(t, d, sql.NullInt64{Int64: uid, Valid: true}, true)
	up.setAvailability("premium-adm.com", &CheckResult{
		Domain: "premium-adm.com", Result: "available", Premium: true,
		PremiumPricing: &Pricing{PremiumPrice: 9999, Currency: "USD"},
	})

	// 管理员先查报价：必须能看到溢价标记与溢价价
	_, chk := call(p.adminCheck, withAdmin(postJSON(t, map[string]any{"domain": "premium-adm.com"})))
	if chk["ok"] != float64(1) {
		t.Fatalf("管理员查询溢价报价失败: %v", chk)
	}
	item, _ := chk["item"].(map[string]any)
	if item == nil || item["isPremium"] != true {
		t.Fatalf("管理员查询未识别出溢价域名: %v", chk)
	}
	if item["premiumPrice"] == nil {
		t.Errorf("管理员查询未返回溢价价格，无法确认后售卖: %v", chk)
	}

	// 总开关默认关闭 → 后台代注册溢价域名应被拒（即便 allowPremium=true 也无济于事）。
	// 这是商业安全默认值：上游溢价可能远高于价目表，关闭可避免卖穿成本。
	if got, _ := p.host.Settings.(*fakeSettings).Get(context.Background(), "plugin.spaceship.allowPremium"); got != "0" && got != "" {
		t.Fatalf("测试前置：allowPremium 默认应为空或 0，实际 %q", got)
	}
	_, rej := call(p.adminRegisterDomain, withAdmin(postJSON(t, map[string]any{
		"domain": "premium-adm.com", "years": 1, "userId": uid, "contactId": cid, "allowPremium": true,
	})))
	if rej["ok"] == float64(1) {
		t.Errorf("P1 溢价：allowPremium 总开关默认关闭却仍能代注册（卖穿成本风险）body=%v", rej)
	}

	// 总开关开启后 → 管理员确认即可代注册（按价目表扣款）
	_ = p.host.Settings.(*fakeSettings).Set(context.Background(), "plugin.spaceship.allowPremium", "1")
	before := balanceOf(t, d, uid)
	_, ok := call(p.adminRegisterDomain, withAdmin(postJSON(t, map[string]any{
		"domain": "premium-adm.com", "years": 1, "userId": uid, "contactId": cid, "allowPremium": true,
	})))
	if ok["ok"] != float64(1) {
		t.Fatalf("P1 溢价：管理员确认后仍无法代注册（溢价域名卖不出去）body=%v", ok)
	}
	if balanceOf(t, d, uid) >= before {
		t.Errorf("P1 溢价：管理员代注册溢价域名未扣款")
	}
	// 清理：删除本次注册的域名与订单
	t.Cleanup(func() {
		_, _ = d.ExecContext(context.Background(), `DELETE FROM plugin_spaceship_orders WHERE domain='premium-adm.com'`)
		_, _ = d.ExecContext(context.Background(), `DELETE FROM plugin_spaceship_domains WHERE domain='premium-adm.com'`)
	})
}

// =====================================================================
// P1 联系人与重试：编辑必须带上原 contactId（否则上游会不断新建联系人）
// =====================================================================

func TestP1_ContactEdit_MustReuseUpstreamContactID(t *testing.T) {
	up := newFakeUpstream()
	defer up.Close()
	withFakeUpstream(t, up)
	p, d := newTestPlugin(t, up, nil)

	uid := createTestUser(t, d, "edit", "100.00")
	cid := createTestContact(t, d, sql.NullInt64{Int64: uid, Valid: true}, true)

	body := map[string]any{
		"firstName": "New", "lastName": "Name", "email": "new@lumeidc.test",
		"address1": "addr", "city": "City", "country": "CN", "phone": "+86.13800000000",
		"existingId": cid,
	}
	_, res := call(p.clientSaveContact, withUser(postJSON(t, body), uid))
	if res["ok"] != float64(1) {
		t.Fatalf("编辑联系人失败: %v", res)
	}
	saved := up.snapshotContacts()
	if len(saved) == 0 {
		t.Fatal("未调用上游保存联系人")
	}
	last := saved[len(saved)-1]
	if last["contactId"] == nil || last["contactId"] == "" {
		t.Errorf("P1 联系人：编辑未带上游 contactId，会在 Spaceship 侧新建重复联系人: %v", last)
	}
}

// 终态操作不得重复重试。
func TestP1_Retry_TerminalOperationMustBeRejected(t *testing.T) {
	up := newFakeUpstream()
	defer up.Close()
	withFakeUpstream(t, up)
	p, d := newTestPlugin(t, up, nil)

	uid := createTestUser(t, d, "retry", "100.00")
	var opID int64
	if err := d.QueryRowContext(context.Background(),
		`INSERT INTO plugin_spaceship_operations (operation_id,domain,op_type,status,started_at)
		 VALUES('op-done','retry-p1.com','domain_create','success',$1) RETURNING id`, time.Now()).Scan(&opID); err != nil {
		t.Fatal(err)
	}
	_ = uid
	t.Cleanup(func() {
		_, _ = d.ExecContext(context.Background(), `DELETE FROM plugin_spaceship_operations WHERE id=$1`, opID)
	})

	r := httptest.NewRequest(http.MethodPost, "/operations/1/retry", strings.NewReader(`{}`))
	r.SetPathValue("id", fmt.Sprint(opID))
	_, res := call(p.adminRetryOperation, withAdmin(r))
	if res["ok"] == float64(1) {
		t.Errorf("P1 状态机：已 success 的操作仍可重试 body=%v", res)
	}
}

// 卡死 pending 操作可由管理员手动复活：started_at 被复位到 now()，下一次 cron 即便
// 现在跑也能拣到；同时 pollOne 立刻拉一次把"上游其实早成功了"的状态落库。
func TestP1_Retry_CanResurrectStuckPending(t *testing.T) {
	up := newFakeUpstream()
	defer up.Close()
	withFakeUpstream(t, up)
	p, d := newTestPlugin(t, up, nil)

	uid := createTestUser(t, d, "stuck", "100.00")
	_ = uid

	// 注册接口会进注册路径；这里只要个 pending op —— 直接 SQL 插。
	const opIDStr = "op-stuck-1"
	stuckAt := time.Now().Add(-30 * time.Minute) // 远在 15min 窗口外
	var opID int64
	if err := d.QueryRowContext(context.Background(),
		`INSERT INTO plugin_spaceship_operations (operation_id,domain,op_type,status,started_at)
		 VALUES($1,'stuck-p1.com','domain_create','pending',$2) RETURNING id`, opIDStr, stuckAt).Scan(&opID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = d.ExecContext(context.Background(), `DELETE FROM plugin_spaceship_operations WHERE id=$1`, opID)
	})

	// 第一次 GET /v1/async-operations/op-stuck-1：服务端拿到的是 pending（卡死），
	// 但此时 fakeUpstream 这边"其实已经注册成功了"。先布置上游状态。
	up.SetOpStatus(opIDStr, "success")

	// retry 前断言 started_at 仍是卡死远点
	var pre time.Time
	if err := d.QueryRowContext(context.Background(),
		`SELECT started_at FROM plugin_spaceship_operations WHERE id=$1`, opID).Scan(&pre); err != nil {
		t.Fatal(err)
	}
	if time.Since(pre) < 15*time.Minute {
		t.Fatalf("测试前置条件失败：started_at 应该 >15min 之前，got %s", pre)
	}

	r := httptest.NewRequest(http.MethodPost, "/operations/1/retry", strings.NewReader(`{}`))
	r.SetPathValue("id", fmt.Sprint(opID))
	_, res := call(p.adminRetryOperation, withAdmin(r))
	if res["ok"] != float64(1) {
		t.Fatalf("P1 复活：卡死 pending 未能 retry，body=%v", res)
	}

	// 1) 数据库层面 started_at 已被复位到近 1 分钟内
	var postStarted time.Time
	var postStatus string
	var postError sql.NullString
	if err := d.QueryRowContext(context.Background(),
		`SELECT started_at,status,error_msg FROM plugin_spaceship_operations WHERE id=$1`, opID).
		Scan(&postStarted, &postStatus, &postError); err != nil {
		t.Fatal(err)
	}
	if time.Since(postStarted) > 1*time.Minute {
		t.Errorf("P1 复活：started_at 未被复位到 now() 附近，got %s", postStarted)
	}
	// 2) 立刻 pollOne 把上游 success 状态落库：status 应该变 success
	if postStatus != "success" {
		t.Errorf("P1 复活：pollOne 未把上游 success 落库，status=%s err=%v", postStatus, postError)
	}
}

// =====================================================================
// 联系人可见性：共享模板对用户可见但必须标注 shared（只读），后台列表同理
// =====================================================================

func TestP1_ContactList_MustExposeSharedFlag(t *testing.T) {
	up := newFakeUpstream()
	defer up.Close()
	withFakeUpstream(t, up)
	p, d := newTestPlugin(t, up, nil)

	uid := createTestUser(t, d, "flag", "100.00")
	mine := createTestContact(t, d, sql.NullInt64{Int64: uid, Valid: true}, false)
	shared := createTestContact(t, d, sql.NullInt64{}, false)

	// 前台：共享模板必须出现在列表里（否则用户无联系人可用），且带 shared 标记
	_, res := call(p.clientMyContacts, withUser(getReq("/my/contacts"), uid))
	list, _ := res["list"].([]any)
	find := func(id int64) map[string]any {
		for _, it := range list {
			m, _ := it.(map[string]any)
			if m != nil && int64(m["id"].(float64)) == id {
				return m
			}
		}
		return nil
	}
	rowMine, rowShared := find(mine), find(shared)
	if rowMine == nil || rowShared == nil {
		t.Fatalf("前台联系人列表应同时包含自建与共享模板: %v", res)
	}
	if rowMine["shared"] != false {
		t.Errorf("P1 可见性：自己的联系人被误标为共享模板: %v", rowMine)
	}
	if rowShared["shared"] != true {
		t.Errorf("P1 可见性：管理员共享模板未标注 shared，用户会以为可以删除/修改: %v", rowShared)
	}
	// 编辑回填所需字段必须一并返回（否则前台编辑会清空未展示字段）
	for _, k := range []string{"firstName", "lastName", "address1", "city", "postalCode"} {
		if _, ok := rowMine[k]; !ok {
			t.Errorf("P1 联系人：前台列表缺少回填字段 %q，编辑会丢失数据", k)
		}
	}

	// 后台：同样要能区分归属（管理员需要知道哪些是共享模板）
	_, ares := call(p.adminListContacts, withAdmin(getReq("/contacts")))
	alist, _ := ares["list"].([]any)
	var found int
	for _, it := range alist {
		m, _ := it.(map[string]any)
		if m == nil {
			continue
		}
		id := int64(m["id"].(float64))
		if id != mine && id != shared {
			continue
		}
		found++
		want := id == shared
		if m["shared"] != want {
			t.Errorf("P1 可见性：后台列表 id=%d shared 应为 %v，实际 %v", id, want, m["shared"])
		}
	}
	if found != 2 {
		t.Errorf("后台联系人列表应包含两条测试联系人，实际匹配 %d 条", found)
	}
}

// 后台编辑用户自建联系人：必须保留归属（否则会被"提升"成共享模板，对所有用户可见＝隐私越权）。
func TestP1_AdminEditUserContact_MustKeepOwnership(t *testing.T) {
	up := newFakeUpstream()
	defer up.Close()
	withFakeUpstream(t, up)
	p, d := newTestPlugin(t, up, nil)

	uid := createTestUser(t, d, "adminedit", "100.00")
	cid := createTestContact(t, d, sql.NullInt64{Int64: uid, Valid: true}, false)

	_, res := call(p.adminSaveContact, withAdmin(postJSON(t, map[string]any{
		"firstName": "Adm", "lastName": "Edit", "email": "adm-edit@lumeidc.test",
		"address1": "addr", "city": "City", "country": "CN", "phone": "+86.13800000000",
		"existingId": cid,
	})))
	if res["ok"] != float64(1) {
		t.Fatalf("后台编辑联系人应成功: %v", res)
	}
	var owner sql.NullInt64
	if err := d.QueryRowContext(context.Background(),
		`SELECT user_id FROM plugin_spaceship_contacts WHERE id=$1`, cid).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	if !owner.Valid || owner.Int64 != uid {
		t.Errorf("P1 越权：后台编辑用户自建联系人后归属丢失（user_id=%v），该联系人对其他用户可见", owner)
	}
	// 编辑必须复用上游 contactId（新建会留下孤儿联系人）
	saved := up.snapshotContacts()
	if len(saved) == 0 || saved[len(saved)-1]["contactId"] == nil {
		t.Errorf("P1 联系人：后台编辑未带上游 contactId: %v", saved)
	}
}

// 后台设默认：作用域必须跟随联系人自身归属，不得清空其他用户的默认设置。
func TestP1_AdminSetDefault_MustFollowContactOwnership(t *testing.T) {
	up := newFakeUpstream()
	defer up.Close()
	withFakeUpstream(t, up)
	p, d := newTestPlugin(t, up, nil)

	uid := createTestUser(t, d, "ownscope", "100.00")
	mine := createTestContact(t, d, sql.NullInt64{Int64: uid, Valid: true}, true)
	other := createTestContact(t, d, sql.NullInt64{Int64: uid, Valid: true}, false)

	r := httptest.NewRequest(http.MethodPost, "/contacts/1/default", strings.NewReader(`{}`))
	r.SetPathValue("id", fmt.Sprint(other))
	if _, res := call(p.adminSetDefaultContact, withAdmin(r)); res["ok"] != float64(1) {
		t.Fatalf("后台设默认应成功: %v", res)
	}
	var mineDef, otherDef bool
	if err := d.QueryRowContext(context.Background(),
		`SELECT is_default FROM plugin_spaceship_contacts WHERE id=$1`, mine).Scan(&mineDef); err != nil {
		t.Fatal(err)
	}
	if err := d.QueryRowContext(context.Background(),
		`SELECT is_default FROM plugin_spaceship_contacts WHERE id=$1`, other).Scan(&otherDef); err != nil {
		t.Fatal(err)
	}
	if !otherDef {
		t.Errorf("P1：后台设默认未生效（目标 id=%d is_default=%v）", other, otherDef)
	}
	if mineDef {
		t.Errorf("P1：后台设默认未清理同用户旧默认（id=%d 仍为默认），会出现两个默认联系人", mine)
	}
}

// 用户编辑共享模板必须拒绝（只读，后端兜底 403）。
func TestP1_UserEditSharedContact_MustBeRejected(t *testing.T) {
	up := newFakeUpstream()
	defer up.Close()
	withFakeUpstream(t, up)
	p, d := newTestPlugin(t, up, nil)

	uid := createTestUser(t, d, "editshared", "100.00")
	shared := createTestContact(t, d, sql.NullInt64{}, false)

	code, res := call(p.clientSaveContact, withUser(postJSON(t, map[string]any{
		"firstName": "Hack", "lastName": "Er", "email": "hack@lumeidc.test",
		"address1": "addr", "city": "City", "country": "CN", "phone": "+86.13800000000",
		"existingId": shared,
	}), uid))
	if res["ok"] == float64(1) {
		t.Errorf("P1 越权：普通用户改写了管理员共享联系人（code=%d body=%v）", code, res)
	}
}
