package refund

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// refundFixture 一套「用户 + 产品 + 已付订单」的自建自删测试数据。
type refundFixture struct {
	db        *sql.DB
	userID    int64
	productID int64
	orderID   int64
}

// setupRefundDB 执行幂等迁移建表并插入测试用户与已支付订单；无 TEST_DATABASE_DSN 跳过。
// 清理仅删本用例产生的数据。
func setupRefundDB(t *testing.T) *refundFixture {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("no dsn")
	}
	d, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	mig, err := fs.ReadFile(migrationsFS, "migrations/001_init.sql")
	if err != nil {
		d.Close()
		t.Fatal(err)
	}
	if _, err := d.ExecContext(ctx, string(mig)); err != nil {
		d.Close()
		t.Fatalf("执行迁移失败: %v", err)
	}

	// 订单需要 priceset_id：复用库里最小的一条，避免额外造价格组。
	var psID int64
	if err := d.QueryRowContext(ctx, `SELECT min(id) FROM pricesets`).Scan(&psID); err != nil {
		d.Close()
		t.Skipf("库中暂无价格组，跳过: %v", err)
	}
	f := &refundFixture{db: d}
	if err := d.QueryRowContext(ctx,
		`INSERT INTO users(email,password_hash) VALUES($1,'x') RETURNING id`,
		fmt.Sprintf("refund_test_%d@example.com", time.Now().UnixNano())).Scan(&f.userID); err != nil {
		d.Close()
		t.Fatal(err)
	}
	if err := d.QueryRowContext(ctx,
		`INSERT INTO products(type_id,name,stock) VALUES(NULL,'单元测试-自助退款',-1) RETURNING id`).
		Scan(&f.productID); err != nil {
		d.Close()
		t.Fatal(err)
	}
	if err := d.QueryRowContext(ctx,
		`INSERT INTO orders(user_id,product_id,priceset_id,cycle,amount,status,paid_at)
		 VALUES($1,$2,$3,'monthly','100.00',1,now()) RETURNING id`,
		f.userID, f.productID, psID).Scan(&f.orderID); err != nil {
		d.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c := context.Background()
		for _, s := range []struct {
			q   string
			arg int64
		}{
			{`DELETE FROM refunds WHERE order_id=$1`, f.orderID},
			{`DELETE FROM plugin_refund_requests WHERE order_id=$1`, f.orderID},
			{`DELETE FROM orders WHERE id=$1`, f.orderID},
			{`DELETE FROM products WHERE id=$1`, f.productID},
			{`DELETE FROM users WHERE id=$1`, f.userID},
		} {
			if _, err := d.ExecContext(c, s.q, s.arg); err != nil {
				t.Errorf("清理测试数据失败(%s): %v", s.q, err)
			}
		}
		d.Close()
	})
	return f
}

func (f *refundFixture) requests() *Requests { return NewRequests(f.db) }

// 同一订单同时仅允许一条进行中申请（部分唯一索引）；撤回后可重新申请。
func TestRequestsCreateConflict(t *testing.T) {
	f := setupRefundDB(t)
	ctx := context.Background()
	reqs := f.requests()

	id, err := reqs.Create(ctx, &Request{OrderID: f.orderID, UserID: f.userID, Amount: "30.00", Reason: "不想要了", Method: "balance", Status: statusPending})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reqs.Create(ctx, &Request{OrderID: f.orderID, UserID: f.userID, Amount: "10.00", Reason: "重复购买", Method: "balance", Status: statusPending}); err != ErrExists {
		t.Fatalf("同订单二次申请应返回 ErrExists: %v", err)
	}
	if got, err := reqs.Get(ctx, id); err != nil || got == nil || got.Amount != "30.00" {
		t.Fatalf("申请字段不符: %+v err=%v", got, err)
	}

	// 撤回后 pending 唯一约束释放，可再次申请。
	done, err := reqs.Withdraw(ctx, id, f.userID)
	if err != nil || !done {
		t.Fatalf("撤回失败: %v err=%v", done, err)
	}
	if _, err := reqs.Create(ctx, &Request{OrderID: f.orderID, UserID: f.userID, Amount: "10.00", Reason: "重复购买", Method: "balance", Status: statusPending}); err != nil {
		t.Fatalf("撤回后应可重新申请: %v", err)
	}
}

// 双审防护：Claim 仅 pending 可流转，第二个管理员抢不到。
func TestRequestsClaimAtomic(t *testing.T) {
	f := setupRefundDB(t)
	ctx := context.Background()
	reqs := f.requests()

	id, err := reqs.Create(ctx, &Request{OrderID: f.orderID, UserID: f.userID, Amount: "10.00", Reason: "不想要了", Method: "balance", Status: statusPending})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := reqs.Claim(ctx, id, statusRejected, 9, "不符合退款政策")
	if err != nil || !claimed {
		t.Fatalf("首次 Claim 应成功: %v err=%v", claimed, err)
	}
	again, err := reqs.Claim(ctx, id, statusApproved, 10, "")
	if err != nil || again {
		t.Fatalf("非 pending 不可再次 Claim: %v err=%v", again, err)
	}
	got, err := reqs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != statusRejected || !got.HandledBy.Valid || got.HandledBy.Int64 != 9 || got.HandleNote != "不符合退款政策" {
		t.Fatalf("Claim 应一次写入状态/处理人/备注: %+v", got)
	}
}

// 退款执行失败回滚：Reopen 后回到 pending 且清空处理信息，可重试。
func TestRequestsReopenAfterFail(t *testing.T) {
	f := setupRefundDB(t)
	ctx := context.Background()
	reqs := f.requests()

	id, err := reqs.Create(ctx, &Request{OrderID: f.orderID, UserID: f.userID, Amount: "10.00", Reason: "不想要了", Method: "balance", Status: statusPending})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reqs.Claim(ctx, id, statusApproved, 9, ""); err != nil {
		t.Fatal(err)
	}
	if err := reqs.Reopen(ctx, id); err != nil {
		t.Fatal(err)
	}
	got, err := reqs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != statusPending || got.HandledBy.Valid || got.HandledAt.Valid {
		t.Fatalf("Reopen 应回到 pending 且清空处理信息: %+v", got)
	}
	// 回滚后可再次审核通过。
	claimed, err := reqs.Claim(ctx, id, statusApproved, 9, "")
	if err != nil || !claimed {
		t.Fatalf("Reopen 后应可重新 Claim: %v err=%v", claimed, err)
	}
}

// 撤回仅限本人 + pending。
func TestRequestsWithdraw(t *testing.T) {
	f := setupRefundDB(t)
	ctx := context.Background()
	reqs := f.requests()

	id, err := reqs.Create(ctx, &Request{OrderID: f.orderID, UserID: f.userID, Amount: "10.00", Reason: "不想要了", Method: "balance", Status: statusPending})
	if err != nil {
		t.Fatal(err)
	}
	if done, err := reqs.Withdraw(ctx, id, f.userID+999999); err != nil || done {
		t.Fatalf("他人不可撤回: %v err=%v", done, err)
	}
	if done, err := reqs.Withdraw(ctx, id, f.userID); err != nil || !done {
		t.Fatalf("本人应可撤回: %v err=%v", done, err)
	}
	if done, err := reqs.Withdraw(ctx, id, f.userID); err != nil || done {
		t.Fatalf("非 pending 不可重复撤回: %v err=%v", done, err)
	}
}

// 已退总额与退款单回写：无记录为 "0"，有记录求和；LatestRefundID 取最新一条。
func TestRequestsRefundedTotalAndLatest(t *testing.T) {
	f := setupRefundDB(t)
	ctx := context.Background()
	reqs := f.requests()

	total, err := reqs.RefundedTotal(ctx, f.orderID)
	if err != nil || total != "0" {
		t.Fatalf("无退款应返回 0: %q err=%v", total, err)
	}
	if rid, err := reqs.LatestRefundID(ctx, f.orderID); err != nil || rid != 0 {
		t.Fatalf("无退款单应返回 0: %d err=%v", rid, err)
	}
	for _, amount := range []string{"10.00", "20.00"} {
		if _, err := f.db.ExecContext(ctx,
			`INSERT INTO refunds(user_id,order_id,amount,method,reason,status) VALUES($1,$2,$3,'balance','测试','done')`,
			f.userID, f.orderID, amount); err != nil {
			t.Fatal(err)
		}
	}
	total, err = reqs.RefundedTotal(ctx, f.orderID)
	if err != nil || total != "30.00" {
		t.Fatalf("已退总额应为 30.00: %q err=%v", total, err)
	}
	rid, err := reqs.LatestRefundID(ctx, f.orderID)
	if err != nil || rid <= 0 {
		t.Fatalf("应取到最新退款单: %d err=%v", rid, err)
	}

	id, err := reqs.Create(ctx, &Request{OrderID: f.orderID, UserID: f.userID, Amount: "5.00", Reason: "不想要了", Method: "balance", Status: statusPending})
	if err != nil {
		t.Fatal(err)
	}
	if err := reqs.SetRefundID(ctx, id, rid); err != nil {
		t.Fatal(err)
	}
	got, err := reqs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if !got.RefundID.Valid || got.RefundID.Int64 != rid {
		t.Fatalf("refund_id 回写失败: %+v", got)
	}
}

// 可退订单：已支付、无进行中申请、可退余额>0、期限内；超期限/已退满/有待审申请被排除。
func TestEligibleOrders(t *testing.T) {
	f := setupRefundDB(t)
	ctx := context.Background()
	reqs := f.requests()

	orders, err := reqs.EligibleOrders(ctx, f.userID, 7, 100, 0)
	if err != nil || len(orders) != 1 || orders[0].ID != f.orderID {
		t.Fatalf("已支付订单应可退: %+v err=%v", orders, err)
	}
	if orders[0].Refundable != "100.00" {
		t.Fatalf("可退余额应为实付 100.00: %q", orders[0].Refundable)
	}

	// 有待审申请后排除。
	if _, err := reqs.Create(ctx, &Request{OrderID: f.orderID, UserID: f.userID, Amount: "10.00", Reason: "不想要了", Method: "balance", Status: statusPending}); err != nil {
		t.Fatal(err)
	}
	orders, err = reqs.EligibleOrders(ctx, f.userID, 7, 100, 0)
	if err != nil || len(orders) != 0 {
		t.Fatalf("有进行中申请应排除: %+v err=%v", orders, err)
	}

	// 撤回申请、改支付时间为 30 天前：windowDays=7 应排除，0（不限）应保留。
	if _, err := reqs.Withdraw(ctx, mustLastID(t, reqs, f.orderID), f.userID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ExecContext(ctx, `UPDATE orders SET paid_at=now()-interval '30 days' WHERE id=$1`, f.orderID); err != nil {
		t.Fatal(err)
	}
	orders, err = reqs.EligibleOrders(ctx, f.userID, 7, 100, 0)
	if err != nil || len(orders) != 0 {
		t.Fatalf("超期限应排除: %+v err=%v", orders, err)
	}
	orders, err = reqs.EligibleOrders(ctx, f.userID, 0, 100, 0)
	if err != nil || len(orders) != 1 {
		t.Fatalf("不限期限应保留: %+v err=%v", orders, err)
	}

	// 已退满后排除（退款额 = 实付）。
	if _, err := f.db.ExecContext(ctx,
		`INSERT INTO refunds(user_id,order_id,amount,method,reason,status) VALUES($1,$2,'100.00','balance','测试','done')`,
		f.userID, f.orderID); err != nil {
		t.Fatal(err)
	}
	orders, err = reqs.EligibleOrders(ctx, f.userID, 0, 100, 0)
	if err != nil || len(orders) != 0 {
		t.Fatalf("已退满应排除: %+v err=%v", orders, err)
	}
}

// P1：EligibleOrders 必须支持分页 + limit 参数化，handler 之前硬编码 LIMIT 100 会让
// 历史订单超过 100 条的用户看不到后面仍有可退订单 —— 这是体验断崖级 P1。
func TestEligibleOrders_PaginationAndLimit(t *testing.T) {
	f := setupRefundDB(t)
	ctx := context.Background()
	reqs := f.requests()

	// 制造 5 条额外订单（前 setupRefundDB 中已有 1 条 f.orderID），都为可退状态。
	for i := 0; i < 5; i++ {
		if _, err := f.db.ExecContext(ctx,
			`INSERT INTO orders(user_id,product_id,amount,cycle,status,paid_at) VALUES($1,$2,'10.00','month',1,now())`,
			f.userID, f.productID); err != nil {
			t.Fatal(err)
		}
	}

	// 1) limit=2：应只返 2 条；ID DESC 排序，最新插入的优先。
	page1, err := reqs.EligibleOrders(ctx, f.userID, 0, 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(page1) != 2 {
		t.Fatalf("limit=2 应返 2 条，got %d", len(page1))
	}
	if page1[0].ID <= page1[1].ID {
		t.Fatalf("应按 ID DESC 排序，got %d, %d", page1[0].ID, page1[1].ID)
	}

	// 2) offset=2 limit=2：下一页又是 2 条；ID 与 page1 完全不重叠。
	page2, err := reqs.EligibleOrders(ctx, f.userID, 0, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(page2) != 2 {
		t.Fatalf("offset=2 limit=2 应返 2 条，got %d", len(page2))
	}
	seen := map[int64]bool{page1[0].ID: true, page1[1].ID: true}
	if seen[page2[0].ID] || seen[page2[1].ID] {
		t.Fatalf("offset 分页结果应与 page1 不重叠: page1=%v page2=%v", page1, page2)
	}

	// 3) limit=10000 应被 repo 自己夹回 >=1，handler 会再夹到硬上限 500：单元层只验
	//    repo 不会再被任何调用方逼着一次性 OOM。即 limit=1 仍是合法的。
	oneOnly, err := reqs.EligibleOrders(ctx, f.userID, 0, 1, 0)
	if err != nil || len(oneOnly) != 1 {
		t.Fatalf("limit=1 应返 1 条: %+v err=%v", oneOnly, err)
	}

	// 4) 负 offset / 负 limit：repo 自身 clamp 到合理值，不应 panic。
	_, err = reqs.EligibleOrders(ctx, f.userID, 0, -50, -5)
	if err != nil {
		t.Fatalf("负 limit/offset 应被 clamp，而不是报错: %v", err)
	}
}

// 列表与统计：后台 JOIN 用户/订单，本人列表仅本人，状态筛选与关键词生效。
func TestRequestsListAndStats(t *testing.T) {
	f := setupRefundDB(t)
	ctx := context.Background()
	reqs := f.requests()

	id1, err := reqs.Create(ctx, &Request{OrderID: f.orderID, UserID: f.userID, Amount: "10.00", Reason: "不想要了", Method: "balance", Status: statusPending})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reqs.Claim(ctx, id1, statusApproved, 9, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := reqs.Create(ctx, &Request{OrderID: f.orderID, UserID: f.userID, Amount: "20.00", Reason: "重复购买", Method: "gateway", Status: statusPending}); err != nil {
		t.Fatal(err)
	}

	pending, approved, rejected, err := reqs.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if approved < 1 || pending < 1 || rejected < 0 {
		t.Fatalf("统计异常: pending=%d approved=%d rejected=%d", pending, approved, rejected)
	}

	rows, total, err := reqs.AdminList(ctx, Filter{Status: statusApproved, Page: 1, Limit: 10})
	if err != nil || total < 1 {
		t.Fatalf("后台列表错误: total=%d err=%v", total, err)
	}
	found := false
	for _, row := range rows {
		if row.ID == id1 {
			found = true
			if row.UserEmail == "" || row.OrderAmount != "100.00" || row.OrderCycle != "monthly" {
				t.Fatalf("后台行应带回用户与订单信息: %+v", row)
			}
		}
	}
	if !found {
		t.Fatal("后台列表应含已通过申请")
	}
	rows, _, err = reqs.AdminList(ctx, Filter{Keyword: "重复购买", Page: 1, Limit: 10})
	if err != nil || len(rows) != 1 || rows[0].Reason != "重复购买" {
		t.Fatalf("关键词（原因）筛选错误: %+v err=%v", rows, err)
	}

	mine, total, err := reqs.ListByUser(ctx, f.userID, 1, 10)
	if err != nil || total != 2 || len(mine) != 2 {
		t.Fatalf("本人列表应为 2 条: total=%d len=%d err=%v", total, len(mine), err)
	}
	if mine[0].ID <= mine[1].ID {
		t.Fatal("应按 id 倒序")
	}
	other, _, err := reqs.ListByUser(ctx, f.userID+999999, 1, 10)
	if err != nil || len(other) != 0 {
		t.Fatalf("他人列表应为空: %+v err=%v", other, err)
	}

	// 本人统计仅计入本人申请，他人查询应为 0。
	myPending, myApproved, myRejected, err := reqs.StatsByUser(ctx, f.userID)
	if err != nil {
		t.Fatal(err)
	}
	if myPending != 1 || myApproved != 1 || myRejected != 0 {
		t.Fatalf("本人统计异常: pending=%d approved=%d rejected=%d", myPending, myApproved, myRejected)
	}
	otherPending, otherApproved, otherRejected, err := reqs.StatsByUser(ctx, f.userID+999999)
	if err != nil || otherPending != 0 || otherApproved != 0 || otherRejected != 0 {
		t.Fatalf("他人统计应为 0: %d %d %d err=%v", otherPending, otherApproved, otherRejected, err)
	}
}

// PaidOrderByUser：仅本人已支付订单可见。
func TestPaidOrderByUser(t *testing.T) {
	f := setupRefundDB(t)
	ctx := context.Background()
	reqs := f.requests()

	got, err := reqs.PaidOrderByUser(ctx, f.orderID, f.userID)
	if err != nil || got == nil || got.Amount != "100.00" || !got.PaidAt.Valid {
		t.Fatalf("取本人已支付订单失败: %+v err=%v", got, err)
	}
	if miss, err := reqs.PaidOrderByUser(ctx, f.orderID, f.userID+999999); err != nil || miss != nil {
		t.Fatalf("他人订单应不可见: %+v err=%v", miss, err)
	}
}

// mustLastID 取订单最近一条申请 ID（测试辅助）。
func mustLastID(t *testing.T, reqs *Requests, orderID int64) int64 {
	t.Helper()
	var id int64
	if err := reqs.db.QueryRowContext(context.Background(),
		`SELECT id FROM plugin_refund_requests WHERE order_id=$1 ORDER BY id DESC LIMIT 1`, orderID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}
