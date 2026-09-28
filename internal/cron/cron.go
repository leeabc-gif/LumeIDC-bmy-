package cron

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/robfig/cron/v3"

	"lumeidc/internal/gateway"
	"lumeidc/internal/money"
	"lumeidc/internal/plugin"
	"lumeidc/internal/repo"
	"lumeidc/internal/server"
	"lumeidc/internal/service"
)

type Jobs struct {
	DB            *sql.DB
	Lifecycle     *service.Lifecycle
	Fulfillment   *service.Fulfillment
	Notifier      *service.Notifier
	Providers     *server.Registry
	Servers       *repo.Servers
	Products      *repo.Products
	Coupons       *repo.Coupons // 账单过期时释放优惠码占用；为 nil 时跳过
	Gateways      *repo.Gateways
	Payment       *service.Payment
	Settings      *repo.Settings // 后台配置读取（生命周期天数等）；为 nil 时全部走默认值
	OrderQueriers map[string]gateway.OrderQuerier
}

// 生命周期天数默认值：到期即停、到期 3 天后标记删除、到期前 3 天提醒。
// ponytail: 上游（魔方财务）通常 1~7 天就真实销毁实例，本地删除阈值若远大于上游，
// 会出现"机器已没、本地仍显示可续费"的幽灵服务，故默认取 3 天而非 30 天。
const (
	defaultSuspendAfterDays   = 0
	defaultTerminateAfterDays = 3
	defaultExpireWarnDays     = 3
)

// lifecycleDays 读取后台配置的生命周期天数；缺失、非数字或越界一律回退默认值，
// 保证配置写坏时不会把服务提前删掉或永久不删。
func (j *Jobs) lifecycleDays(ctx context.Context) (suspend, terminate, warn int) {
	suspend, terminate, warn = defaultSuspendAfterDays, defaultTerminateAfterDays, defaultExpireWarnDays
	if j.Settings == nil {
		return
	}
	vals, err := j.Settings.GetMany(ctx,
		"service_suspend_after_days", "service_terminate_after_days", "service_expire_warn_days")
	if err != nil {
		log.Printf("[cron] 读取生命周期配置失败，按默认值执行: %v", err)
		return
	}
	if n, e := strconv.Atoi(strings.TrimSpace(vals["service_suspend_after_days"])); e == nil && n >= 0 && n <= 365 {
		suspend = n
	}
	if n, e := strconv.Atoi(strings.TrimSpace(vals["service_terminate_after_days"])); e == nil && n >= 1 && n <= 3650 {
		terminate = n
	}
	if n, e := strconv.Atoi(strings.TrimSpace(vals["service_expire_warn_days"])); e == nil && n >= 1 && n <= 365 {
		warn = n
	}
	if terminate < suspend {
		terminate = suspend // 删除不得早于停机
	}
	return
}

// 定时任务的告警标识。仅用于「同一任务每天最多一封告警邮件」的去重键，
// 与日志文案解耦，改名不影响告警语义。
const (
	taskExpireInvoices     = "expire_invoices"
	taskRunExpired         = "expired_services"
	taskReleaseStock       = "release_expired_stock"
	taskReconcilePayments  = "reconcile_pending_payments"
	taskNotifyExpiringSoon = "notify_expiring_soon"
	taskNotifyEndingPromos = "notify_ending_promotions"
	taskSyncPrices         = "sync_prices"
)

// alertTaskFailure 定时任务级失败时给管理员发告警邮件。
//
// 这些任务没有任何用户可见入口：失败只写一行日志就等于静默停摆——到期停机/删除、
// 异步通知丢失后的补单、账单过期退款、活动名额与优惠码释放全靠它们，管理员往往
// 要等到用户投诉才发现。去重键含当天日期：同一任务每天最多一封，既不刷屏，
// 也不会在修好之后再次故障时彻底哑掉（永久去重键会永久闭嘴）。
func (j *Jobs) alertTaskFailure(ctx context.Context, task, what string, err error) {
	if j.Notifier == nil || err == nil {
		return
	}
	key := "cron:" + task + ":" + time.Now().Format("2006-01-02")
	body := "定时任务「" + what + "」执行失败，相关功能可能已经停摆。\n" +
		"失败原因：" + err.Error() + "\n" +
		"发生时间：" + time.Now().Format("2006-01-02 15:04:05") + "\n" +
		"处理建议：按同一时间点在服务端日志中检索 [cron] 定位原因；修复后下一轮会自动恢复，" +
		"失败期间跳过的处理会由后续轮次补做。"
	if aerr := j.Notifier.NotifyAdminOnce(ctx, key, "cron", what+"失败", body); aerr != nil {
		log.Printf("[cron] 管理员告警发送失败（%s）: %v", task, aerr)
	}
}

// failTask 记录任务级失败并发告警。日志与告警成对出现，避免后人只补一半：
// 只在日志里留一行，等于把「整类业务静默停摆」藏起来。
func (j *Jobs) failTask(ctx context.Context, task, what string, err error) {
	log.Printf("[cron] %s失败: %v", what, err)
	j.alertTaskFailure(ctx, task, what, err)
}

func (j *Jobs) Start() *cron.Cron {
	c := cron.New(cron.WithChain(
		cron.SkipIfStillRunning(cron.DefaultLogger),
	))
	c.AddFunc("@every 15s", func() {
		if j.Fulfillment != nil {
			j.Fulfillment.Drain(context.Background(), 10)
		}
	})
	c.AddFunc("@every 10m", func() {
		ctx := context.Background()
		suspend, _, _ := j.lifecycleDays(ctx)
		j.runExpired(ctx,
			`SELECT id FROM services WHERE status=1 AND expires_at < now() - make_interval(days => $1)`, "停机",
			func(ctx context.Context, id int64) error { return j.Lifecycle.Suspend(ctx, id) }, suspend)
	})
	c.AddFunc("@every 1h", func() {
		ctx := context.Background()
		_, terminate, _ := j.lifecycleDays(ctx)
		j.runExpired(ctx,
			`SELECT id FROM services WHERE status=2 AND expires_at < now() - make_interval(days => $1)`, "删除",
			func(ctx context.Context, id int64) error { return j.Lifecycle.Terminate(ctx, id) }, terminate)
	})
	c.AddFunc("@every 10m", func() { j.releaseExpiredStock(context.Background()) })
	c.AddFunc("@every 10m", func() { j.expireInvoices(context.Background()) })
	c.AddFunc("@every 2m", func() { j.reconcilePendingPayments(context.Background()) })
	// 到期前 3 天提醒（每 6 小时一次，避免重复发送由 expire_warn_sent 标记保证）
	c.AddFunc("@every 6h", func() { j.notifyExpiringSoon(context.Background()) })
	// 按上游同步本地服务状态（@every 30s），保证本地状态跟随上游真实状态
	c.AddFunc("@every 30s", func() {
		if j.Lifecycle != nil {
			j.Lifecycle.SyncUpstreamStatus(context.Background())
		}
	})
	// 定时同步上游产品价格与库存（每 6 小时）
	c.AddFunc("@every 6h", func() { j.syncPrices(context.Background()) })
	c.AddFunc("@every 1h", func() { j.notifyEndingPromotions(context.Background()) })
	// 插件定时任务（CronContributor 能力）：插件禁用时跳过；失败与核心任务同待遇告警。
	for _, pl := range plugin.All() {
		cc, ok := pl.(plugin.CronContributor)
		if !ok {
			continue
		}
		pluginName := pl.Info().Name
		for _, job := range cc.CronJobs() {
			job := job // 闭包捕获
			if _, err := c.AddFunc(job.Spec, func() {
				if !plugin.Enabled(pluginName) {
					return
				}
				ctx := context.Background()
				if err := job.Run(ctx); err != nil {
					j.failTask(ctx, "plugin:"+pluginName+":"+job.Name, job.What, err)
				}
			}); err != nil {
				log.Printf("[cron] 插件 %s 任务 %s 注册失败（表达式 %q 无效）: %v", pluginName, job.Name, job.Spec, err)
			}
		}
	}
	c.Start()
	return c
}

func (j *Jobs) notifyEndingPromotions(ctx context.Context) {
	if j.DB == nil || j.Notifier == nil {
		return
	}
	rows, err := j.DB.QueryContext(ctx, `SELECT pr.id,pr.name,pr.ends_at,participants.user_id
		FROM promotions pr
		JOIN (
			SELECT promotion_id,user_id FROM promotion_coupon_claims
			UNION
			SELECT promotion_id,user_id FROM orders WHERE promotion_id IS NOT NULL
		) participants ON participants.promotion_id=pr.id
		LEFT JOIN promotion_ending_notifications n ON n.promotion_id=pr.id AND n.user_id=participants.user_id
		WHERE pr.ends_at > now() AND pr.ends_at <= now() + interval '24 hours' AND n.promotion_id IS NULL`)
	if err != nil {
		j.failTask(ctx, taskNotifyEndingPromos, "查询即将结束活动提醒", err)
		return
	}
	type item struct {
		promotionID, userID int64
		name                string
		endsAt              time.Time
	}
	var items []item
	for rows.Next() {
		var it item
		if err := rows.Scan(&it.promotionID, &it.name, &it.endsAt, &it.userID); err != nil {
			rows.Close()
			j.failTask(ctx, taskNotifyEndingPromos, "读取即将结束活动提醒", err)
			return
		}
		items = append(items, it)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		j.failTask(ctx, taskNotifyEndingPromos, "遍历即将结束活动提醒", err)
		return
	}
	rows.Close()
	type promo struct {
		name   string
		endsAt time.Time
	}
	notified := map[int64]promo{}
	for _, it := range items {
		endsAt := it.endsAt.Format("2006-01-02 15:04")
		body := fmt.Sprintf("你参与的活动「%s」将于 %s 结束。", it.name, endsAt)
		if err := j.Notifier.NotifyTemplate(ctx, it.userID, "promotion_ending", "活动即将结束", body, map[string]string{"promotion_name": it.name, "ends_at": endsAt}); err != nil {
			continue
		}
		if _, err := j.DB.ExecContext(ctx, `INSERT INTO promotion_ending_notifications(promotion_id,user_id) VALUES($1,$2) ON CONFLICT DO NOTHING`, it.promotionID, it.userID); err != nil {
			j.failTask(ctx, taskNotifyEndingPromos, "写入活动即将结束提醒标记", err)
		}
		notified[it.promotionID] = promo{name: it.name, endsAt: it.endsAt}
	}
	// 外发插件事件：按活动去重（而非按参与者逐条），webhook 类订阅方一活动收一次。
	for id, pr := range notified {
		plugin.Emit(ctx, plugin.EventPromotionEnding, plugin.PromotionEndingPayload{
			PromotionID: id, Name: pr.name, EndsAt: pr.endsAt,
		})
	}
}

// reconcilePendingPayments recovers successful payments whose asynchronous
// notification could not reach this instance, such as when the site is behind
// NAT. Only gateways that implement OrderQuerier participate.
func (j *Jobs) reconcilePendingPayments(ctx context.Context) {
	if j.Gateways == nil || j.Payment == nil || len(j.OrderQueriers) == 0 {
		return
	}
	attempts, err := j.Gateways.PendingPaymentAttempts(ctx)
	if err != nil {
		j.failTask(ctx, taskReconcilePayments, "查询待补单记录", err)
		return
	}
	for _, attempt := range attempts {
		impl, ok := j.OrderQueriers[attempt.Driver]
		if !ok {
			continue
		}
		order, qerr := impl.QueryOrder(ctx, gateway.QueryOrderRequest{InvoiceNo: attempt.InvoiceNo, Config: attempt.Config})
		if qerr != nil || !order.Paid || order.OutTradeNo != attempt.InvoiceNo || !sameAmount(order.Amount, attempt.Amount) {
			if qerr != nil {
				log.Printf("[cron] 网关 %s 订单 %s 查询失败: %v", attempt.GatewayCode, attempt.InvoiceNo, qerr)
			}
			continue
		}
		if err := j.Payment.MarkPaid(ctx, attempt.InvoiceNo, order.TradeNo, attempt.GatewayCode, attempt.ID); err != nil && err != service.ErrAlreadyPaid {
			log.Printf("[cron] 网关 %s 订单 %s 补单失败: %v", attempt.GatewayCode, attempt.InvoiceNo, err)
			continue
		}
		log.Printf("[cron] 网关 %s 订单 %s 自动补单成功 trade_no=%s", attempt.GatewayCode, attempt.InvoiceNo, order.TradeNo)
	}
}

func sameAmount(a, b string) bool {
	_, aCents, aErr := money.ParsePositive(strings.TrimSpace(a), 999999999999)
	_, bCents, bErr := money.ParsePositive(strings.TrimSpace(b), 999999999999)
	return aErr == nil && bErr == nil && aCents == bCents
}

// expireInvoices 关闭超过支付窗口的未支付账单，并同时关闭旧支付尝试。
// 组合支付已抵扣的余额在过期时归还用户余额，避免占用。
// 释放动作（抢购名额、优惠码占用）只针对「本轮真正由未支付变为过期」的账单，
// 因此本函数可安全重复执行；不能按 status=3 全量扫描，否则历史账单会被反复释放。
func (j *Jobs) expireInvoices(ctx context.Context) {
	if j.DB == nil {
		return
	}
	tx, err := j.DB.BeginTx(ctx, nil)
	if err != nil {
		j.failTask(ctx, taskExpireInvoices, "开启过期账单事务", err)
		return
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx,
		`SELECT id,user_id,no,coalesce(credit,0)::text FROM invoices
		 WHERE status=0 AND due_at IS NOT NULL AND due_at <= now() AND credit > 0 FOR UPDATE`)
	if err != nil {
		j.failTask(ctx, taskExpireInvoices, "查询待过期账单抵扣", err)
		return
	}
	type creditRefund struct {
		id, userID int64
		no, credit string
	}
	var refunds []creditRefund
	for rows.Next() {
		var r creditRefund
		if err := rows.Scan(&r.id, &r.userID, &r.no, &r.credit); err != nil {
			rows.Close()
			j.failTask(ctx, taskExpireInvoices, "读取待过期账单抵扣", err)
			return
		}
		refunds = append(refunds, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		j.failTask(ctx, taskExpireInvoices, "遍历待过期账单抵扣", err)
		return
	}
	// 归还失败的行本轮不置过期，留待下一轮重试：单条坏数据不能卡死整批账单。
	var pending []int64
	for _, r := range refunds {
		_, cents, perr := money.ParseNonNegative(r.credit, 999999999999)
		if perr != nil || cents <= 0 {
			continue
		}
		amount := money.FormatCents(cents)
		if _, err := tx.ExecContext(ctx, `UPDATE users SET balance=balance+$2::numeric WHERE id=$1`, r.userID, amount); err != nil {
			log.Printf("[cron] 退回账单 %s 抵扣余额失败: %v", r.no, err)
			pending = append(pending, r.id)
			continue
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO balance_logs(user_id,amount,balance_after,type,note)
			 SELECT $1,$2::numeric,balance,'refund',$3 FROM users WHERE id=$1`,
			r.userID, amount, "账单过期退回抵扣 "+r.no); err != nil {
			log.Printf("[cron] 记录账单 %s 抵扣退回流水失败: %v", r.no, err)
			pending = append(pending, r.id)
			continue
		}
	}
	// 关闭过期账单并取回订单号：只有本轮真正 0→3 的账单进入后续释放动作。
	// COALESCE 保证 pending 为空（Go 侧为 nil 切片）时也不会退化成 NULL 比较而漏掉全部账单。
	expired, err := tx.QueryContext(ctx,
		`UPDATE invoices SET status=3,credit=0
		  WHERE status=0 AND due_at IS NOT NULL AND due_at <= now()
		    AND NOT (id = ANY(COALESCE($1::bigint[], '{}'::bigint[])))
		 RETURNING id,order_id`, pending)
	if err != nil {
		j.failTask(ctx, taskExpireInvoices, "处理过期账单", err)
		return
	}
	var orderIDs []int64
	var invIDs []int64
	var n int
	for expired.Next() {
		var id int64
		var orderID sql.NullInt64
		if err := expired.Scan(&id, &orderID); err != nil {
			expired.Close()
			j.failTask(ctx, taskExpireInvoices, "读取过期账单", err)
			return
		}
		n++
		invIDs = append(invIDs, id)
		if orderID.Valid {
			orderIDs = append(orderIDs, orderID.Int64)
		}
	}
	expired.Close()
	if err := expired.Err(); err != nil {
		j.failTask(ctx, taskExpireInvoices, "遍历过期账单", err)
		return
	}
	if len(orderIDs) > 0 {
		// 释放限量抢购活动名额（账单过期未支付的订单）
		if err := repo.NewPromotions(j.DB).ReleaseQuotaByOrders(ctx, tx, orderIDs); err != nil {
			j.failTask(ctx, taskExpireInvoices, "释放活动名额", err)
			return
		}
		// 释放优惠码占用：下单即核销的券在这里退还，用户可再次使用
		if j.Coupons != nil {
			if err := j.Coupons.ReleaseByOrders(ctx, tx, orderIDs); err != nil {
				j.failTask(ctx, taskExpireInvoices, "释放优惠码占用", err)
				return
			}
		}
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE payment_attempts SET status=2 WHERE status=0 AND invoice_id IN (SELECT id FROM invoices WHERE status=3)`); err != nil {
		j.failTask(ctx, taskExpireInvoices, "关闭过期支付尝试", err)
		return
	}
	if err := tx.Commit(); err != nil {
		j.failTask(ctx, taskExpireInvoices, "提交过期账单事务", err)
		return
	}
	if n > 0 || len(refunds) > 0 {
		log.Printf("[cron] 已将 %d 条账单标记为过期，退回 %d 笔抵扣", n, len(refunds))
	}
	if n > 0 {
		plugin.Emit(ctx, plugin.EventInvoiceExpired, plugin.InvoiceExpiredPayload{InvoiceIDs: invIDs, Count: n})
	}
}

// runExpired 对满足条件的到期服务批量执行生命周期操作（停机/删除共用）。
// query 返回待处理服务 id；opName 用于日志（如 "停机"/"删除"）；op 为对单个服务的操作；
// args 为 query 的占位参数（如停机/删除天数），由调用方按后台配置传入。
func (j *Jobs) runExpired(ctx context.Context, query, opName string, op func(context.Context, int64) error, args ...any) {
	if j.Lifecycle == nil {
		return
	}
	rows, err := j.DB.QueryContext(ctx, query, args...)
	if err != nil {
		j.failTask(ctx, taskRunExpired+":"+opName, "查询待"+opName+"服务", err)
		return
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			log.Printf("[cron] 读取到期服务失败: %v", err)
			continue
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		j.failTask(ctx, taskRunExpired+":"+opName, "遍历到期服务", err)
	}
	rows.Close()
	for _, id := range ids {
		if err := op(ctx, id); err != nil {
			log.Printf("[cron] 服务 %d %s失败: %v", id, opName, err)
			continue
		}
		log.Printf("[cron] 服务 %d 已%s", id, opName)
	}
}

// notifyExpiringSoon 服务到期前向用户发送提醒（站内信+邮件），每个服务仅提醒一次。
// 提前天数由后台配置 service_expire_warn_days 决定，默认 3 天。
func (j *Jobs) notifyExpiringSoon(ctx context.Context) {
	if j.Notifier == nil {
		return
	}
	_, _, warn := j.lifecycleDays(ctx)
	rows, err := j.DB.QueryContext(ctx,
		`SELECT sv.id, sv.user_id, sv.product_id, coalesce(u.email,''), sv.expires_at
		 FROM services sv JOIN users u ON u.id=sv.user_id
		 WHERE sv.status=1 AND sv.expire_warn_sent=false
		   AND sv.expires_at BETWEEN now() AND now() + make_interval(days => $1)`, warn)
	if err != nil {
		j.failTask(ctx, taskNotifyExpiringSoon, "查询即将到期服务", err)
		return
	}
	type item struct {
		id  int64
		uid int64
		pid int64
		exp time.Time
	}
	var items []item
	for rows.Next() {
		var it item
		var email string
		if err := rows.Scan(&it.id, &it.uid, &it.pid, &email, &it.exp); err != nil {
			continue
		}
		items = append(items, it)
	}
	rows.Close()
	var warnedIDs []int64
	for _, it := range items {
		body := fmt.Sprintf("您的服务将于 %s 到期，请及时续费以免停机。", it.exp.Format("2006-01-02 15:04"))
		if err := j.Notifier.NotifyTemplate(ctx, it.uid, "service_expiring", "服务即将到期", body, map[string]string{"service_id": fmt.Sprint(it.id), "expires_at": it.exp.Format("2006-01-02 15:04")}); err != nil {
			continue
		}
		warnedIDs = append(warnedIDs, it.id)
		// 外发插件事件，让 webhook 类订阅方也能感知到期提醒（站内信/邮件之外的渠道）。
		plugin.Emit(ctx, plugin.EventServiceExpiring, plugin.ServiceExpiringPayload{
			ServiceID: it.id, UserID: it.uid, ProductID: it.pid, ExpiresAt: it.exp,
		})
	}
	// 标记合并为单条 UPDATE，避免逐行往返。
	if len(warnedIDs) > 0 {
		if _, err := j.DB.ExecContext(ctx,
			`UPDATE services SET expire_warn_sent=true WHERE id = ANY($1)`, warnedIDs); err != nil {
			// 标记写不上，下一轮会把同一批用户再提醒一次，管理员应当知道。
			j.failTask(ctx, taskNotifyExpiringSoon, "批量更新到期提醒标记", err)
		}
	}
}

// releaseExpiredStock 释放过期的库存预留（未支付订单超时）。
func (j *Jobs) releaseExpiredStock(ctx context.Context) {
	res, err := j.DB.ExecContext(ctx,
		`UPDATE stock_reservations SET status='released'
		 WHERE status='reserved' AND expires_at < now()`)
	if err != nil {
		j.failTask(ctx, taskReleaseStock, "释放过期库存预留", err)
		return
	}
	if n, _ := res.RowsAffected(); n > 0 {
		log.Printf("[cron] 已释放 %d 条过期库存预留", n)
	}
}

// splitByPresence 按“上游可售目录是否仍包含该商品”把绑定产品拆成 停售/在售 两组。
// pids: upstreamPID -> localProductID；present: 本次目录命中的 localProductID 集合。
func splitByPresence(pids map[int64]int64, present map[int64]bool) (offline, online []int64) {
	for _, pid := range pids {
		if present[pid] {
			online = append(online, pid)
			continue
		}
		offline = append(offline, pid)
	}
	return offline, online
}

// syncPrices 定时同步上游产品基础价、库存、配置项价格，并联动上游下架。
// 按服务器分组，每台服务器只拉一次全量目录（目录已顺带解析配置项，无额外上游请求）。
// 目录拉取失败时整台服务器跳过，不做任何写入——避免把“拉取异常”误判为“全部下架”。
// 价格不做倒挂判定：售价由本地成本实时换算（售价 = 成本 × (1+利润)），覆盖成本即自动跟上，不会亏。
// ponytail: 以“商品是否仍出现在上游可售目录 /cart/all”判定下架；若上游该接口仍返回已下架商品，
// 需改为解析商品自身的上下架字段（provider 侧补充解析）。
func (j *Jobs) syncPrices(ctx context.Context) {
	if j.Providers == nil || j.Servers == nil || j.Products == nil {
		return
	}
	bound, err := j.Products.ListBound(ctx)
	if err != nil {
		log.Printf("[sync] 查询已绑定产品失败: %v", err)
		// 保留原 [sync] 日志前缀（有人按它做检索），只额外补管理员告警。
		j.alertTaskFailure(ctx, taskSyncPrices, "查询已绑定产品", err)
		return
	}
	if len(bound) == 0 {
		return
	}
	// 按 server_id 分组
	type group struct {
		sv   *repo.Server
		pids map[int64]int64 // upstream_pid -> local product_id
	}
	groups := map[int64]*group{}
	for _, bp := range bound {
		g, ok := groups[bp.ServerID]
		if !ok {
			sv, serr := j.Servers.Get(ctx, bp.ServerID)
			if serr != nil {
				log.Printf("[sync] 读取服务器 %d 失败: %v", bp.ServerID, serr)
				continue
			}
			g = &group{sv: sv, pids: map[int64]int64{}}
			groups[bp.ServerID] = g
		}
		g.pids[bp.UpstreamPID] = bp.ID
	}
	updated, failed, unshelved, restored := 0, 0, 0, 0
	for _, g := range groups {
		prov, err := j.Providers.Get(g.sv.Provider)
		if err != nil {
			log.Printf("[sync] 供应商 %s 不可用: %v", g.sv.Provider, err)
			continue
		}
		cfg := server.Config{APIURL: g.sv.APIURL, APIUsername: g.sv.APIUsername, APIKey: g.sv.APIKey}
		list, err := prov.Catalog(ctx, cfg)
		if err != nil {
			log.Printf("[sync] 拉取 %s 目录失败: %v", g.sv.Name, err)
			continue
		}
		present := make(map[int64]bool, len(list))
		for _, up := range list {
			pid, ok := g.pids[int64(up.PID)]
			if !ok {
				continue
			}
			present[pid] = true
			// 逐周期覆盖：上游为 0 的周期保留本地现值，避免把有效价写成 0（0 元购）；
			// 三周期全 0（上游解析失败或纯配置计价）时等价于只同步库存，同样不覆盖价格。
			if err := j.Products.UpdatePriceAndStockSkippingZero(ctx, pid, up.Monthly, up.Quarterly, up.Yearly, up.Stock); err != nil {
				log.Printf("[sync] 更新产品 %d 失败: %v", pid, err)
				failed++
				continue
			}
			updated++
			// 配置项价格随基础价一并刷新（上游改动内存/硬盘/带宽等档位加价即时生效）。
			if len(up.ConfigOptions) > 0 {
				if err := j.Products.SaveConfigOptions(ctx, pid, up.ConfigOptions); err != nil {
					log.Printf("[sync] 更新产品 %d 配置项失败: %v", pid, err)
					failed++
				}
			}
		}
		// 上游目录中已不存在 → 本地下架；重新出现 → 自动恢复（不影响管理员手动隐藏）。
		offIDs, onIDs := splitByPresence(g.pids, present)
		if n, err := j.Products.SetUpstreamOfflineReason(ctx, onIDs, ""); err != nil {
			log.Printf("[sync] 恢复上游在售失败: %v", err)
		} else {
			restored += int(n)
		}
		if n, err := j.Products.SetUpstreamOfflineReason(ctx, offIDs, repo.UpstreamOfflineUnshelved); err != nil {
			log.Printf("[sync] 标记上游下架失败: %v", err)
		} else {
			unshelved += int(n)
		}
	}
	if updated > 0 || failed > 0 || unshelved > 0 || restored > 0 {
		log.Printf("[sync] 价格/库存/配置项同步完成: 更新 %d，失败 %d，上游下架 %d，恢复 %d",
			updated, failed, unshelved, restored)
	}
}
