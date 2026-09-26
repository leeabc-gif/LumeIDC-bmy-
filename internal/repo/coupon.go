package repo

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"strings"
	"time"
)

// Coupons 优惠码管理。
type Coupons struct{ db *sql.DB }

type Coupon struct {
	ID         int64
	Code       string
	Type       string // percent | fixed
	Value      float64
	MinAmount  float64
	StartsAt   time.Time
	ExpiresAt  *time.Time
	UsageLimit int
	UsedCount  int
	Active     bool
	UserID     *int64
	// 场景化（迁移 080）：
	// ApplyScope     new=仅新购 | renew=仅续费 | both=均可
	// Recurring      同一用户可用总次数 = 1+Recurring（0=每人一次）
	// NeedProductIDs 需求商品：非空时用户须持有激活（status=1）的指定产品服务
	ApplyScope     string
	Recurring      int
	NeedProductIDs []int64
}

var (
	ErrCouponInvalid     = errors.New("优惠码无效或已失效")
	ErrCouponMin         = errors.New("订单金额未达优惠码最低消费")
	ErrCouponExhausted   = errors.New("优惠码已被领完")
	ErrCouponUsed        = errors.New("该优惠码您的可用次数已用完")
	ErrCouponScene       = errors.New("该优惠码不适用于当前场景")
	ErrCouponNeedProduct = errors.New("该优惠码需持有指定产品服务后才可使用")
)

const couponColumns = `id,code,type,value,min_amount,starts_at,expires_at,usage_limit,used_count,active,user_id,apply_scope,recurring,need_product_ids`

// Validate 在事务内锁定并校验优惠码，返回优惠金额（已封顶到订单金额）。
// scene 为使用场景："new"（新购下单）或 "renew"（续费）。
// 调用方需负责写入 coupon_usages 并递增 used_count。
func (c *Coupons) Validate(ctx context.Context, tx *sql.Tx, code string, userID int64, orderAmount string, scene string) (couponID int64, discount string, err error) {
	var cp Coupon
	var expires sql.NullTime
	var need []byte
	err = tx.QueryRowContext(ctx,
		`SELECT `+couponColumns+`
		 FROM coupons WHERE code=$1 AND starts_at<=now() AND (user_id IS NULL OR user_id=$2) FOR UPDATE`, code, userID).
		Scan(&cp.ID, &cp.Code, &cp.Type, &cp.Value, &cp.MinAmount, &cp.StartsAt, &expires, &cp.UsageLimit, &cp.UsedCount, &cp.Active, &cp.UserID, &cp.ApplyScope, &cp.Recurring, &need)
	if errors.Is(err, sql.ErrNoRows) || !cp.Active {
		return 0, "", ErrCouponInvalid
	}
	if err != nil {
		return 0, "", err
	}
	if len(need) > 0 {
		_ = json.Unmarshal(need, &cp.NeedProductIDs)
	}
	// 场景开关：new=仅新购、renew=仅续费、both=均可。
	if cp.ApplyScope != "" && cp.ApplyScope != "both" && cp.ApplyScope != scene {
		return 0, "", ErrCouponScene
	}
	if expires.Valid && expires.Time.Before(time.Now()) {
		return 0, "", ErrCouponInvalid
	}
	if cp.UsageLimit > 0 && cp.UsedCount >= cp.UsageLimit {
		return 0, "", ErrCouponExhausted
	}
	var used int
	if err := tx.QueryRowContext(ctx,
		`SELECT count(*) FROM coupon_usages WHERE coupon_id=$1 AND user_id=$2`, cp.ID, userID).Scan(&used); err != nil {
		return 0, "", err
	}
	// 每人可用总次数 = 1 + recurring（0=每人一次；N=新购 1 次 + 续费再输码 N 次）。
	if used > cp.Recurring {
		return 0, "", ErrCouponUsed
	}
	// 需求商品：须持有激活状态（status=1）的指定产品服务。
	if len(cp.NeedProductIDs) > 0 {
		var ok bool
		if err := tx.QueryRowContext(ctx,
			`SELECT EXISTS(SELECT 1 FROM services WHERE user_id=$1 AND status=1 AND product_id = ANY($2))`,
			userID, cp.NeedProductIDs).Scan(&ok); err != nil {
			return 0, "", err
		}
		if !ok {
			return 0, "", ErrCouponNeedProduct
		}
	}
	amt, _ := strconvParse(orderAmount)
	if amt < cp.MinAmount {
		return 0, "", ErrCouponMin
	}
	var d float64
	if cp.Type == "fixed" {
		d = cp.Value
	} else {
		d = amt * cp.Value / 100
	}
	if d > amt {
		d = amt
	}
	d = float64(int64(d*100+0.5)) / 100
	return cp.ID, format2(d), nil
}

// Use 写入使用记录并递增计数（在订单事务内调用）。
func (c *Coupons) Use(ctx context.Context, tx *sql.Tx, couponID, userID, orderID int64, discount string) error {
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO coupon_usages(coupon_id,user_id,order_id,discount) VALUES($1,$2,$3,$4)`,
		couponID, userID, orderID, discount); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE coupons SET used_count=used_count+1 WHERE id=$1`, couponID)
	return err
}

// ReleaseByOrders 释放这批订单占用的优惠码（账单过期未支付时调用）：
// 回退 used_count 并删除使用记录，使用户可以再次使用该券。
// 调用方只应传入「本轮刚由未支付变为过期」的订单，故本方法可安全重复调用：
// 计数与删除取自同一批记录，第二次调用已删不到行、也就减不到计数。
// coupon_usages 没有唯一约束，幂等只能靠「按 order_id 删」这一条件保证。
func (c *Coupons) ReleaseByOrders(ctx context.Context, tx *sql.Tx, orderIDs []int64) error {
	if len(orderIDs) == 0 {
		return nil
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE coupons c SET used_count = GREATEST(c.used_count - s.cnt, 0)
		 FROM (
		    SELECT cu.coupon_id AS cid, count(*) AS cnt
		      FROM coupon_usages cu
		     WHERE cu.order_id = ANY($1)
		       AND NOT EXISTS (SELECT 1 FROM invoices i2 WHERE i2.order_id=cu.order_id AND i2.status=1)
		     GROUP BY cu.coupon_id
		 ) s
		 WHERE c.id = s.cid`, orderIDs); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx,
		`DELETE FROM coupon_usages cu
		  WHERE cu.order_id = ANY($1)
		    AND NOT EXISTS (SELECT 1 FROM invoices i2 WHERE i2.order_id=cu.order_id AND i2.status=1)`, orderIDs)
	return err
}

func (c *Coupons) List(ctx context.Context) ([]Coupon, error) {
	rows, err := c.db.QueryContext(ctx,
		`SELECT id,code,type,value,min_amount,expires_at,usage_limit,used_count,active,apply_scope,recurring,need_product_ids
		 FROM coupons ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Coupon
	for rows.Next() {
		var cp Coupon
		var expires sql.NullTime
		var need []byte
		if err := rows.Scan(&cp.ID, &cp.Code, &cp.Type, &cp.Value, &cp.MinAmount, &expires, &cp.UsageLimit, &cp.UsedCount, &cp.Active,
			&cp.ApplyScope, &cp.Recurring, &need); err != nil {
			return nil, err
		}
		if expires.Valid {
			cp.ExpiresAt = &expires.Time
		}
		if len(need) > 0 {
			_ = json.Unmarshal(need, &cp.NeedProductIDs)
		}
		out = append(out, cp)
	}
	return out, rows.Err()
}

// Get 按 ID 获取优惠码。
func (c *Coupons) Get(ctx context.Context, id int64) (*Coupon, error) {
	var cp Coupon
	var expires sql.NullTime
	var need []byte
	err := c.db.QueryRowContext(ctx,
		`SELECT id,code,type,value,min_amount,expires_at,usage_limit,used_count,active,apply_scope,recurring,need_product_ids
		 FROM coupons WHERE id=$1`, id).
		Scan(&cp.ID, &cp.Code, &cp.Type, &cp.Value, &cp.MinAmount, &expires, &cp.UsageLimit, &cp.UsedCount, &cp.Active,
			&cp.ApplyScope, &cp.Recurring, &need)
	if err != nil {
		return nil, err
	}
	if expires.Valid {
		cp.ExpiresAt = &expires.Time
	}
	if len(need) > 0 {
		_ = json.Unmarshal(need, &cp.NeedProductIDs)
	}
	return &cp, nil
}

// CouponOptions 场景化可选项（080）：适用范围 / 循环期数 / 需求商品。
type CouponOptions struct {
	ApplyScope     string  // new | renew | both；空按 new
	Recurring      int     // 同一用户可用总次数 = 1+Recurring
	NeedProductIDs []int64 // 需求商品 id 列表（空=不限）
}

// Create 新增优惠码。expires 为空表示永不过期。
func (c *Coupons) Create(ctx context.Context, code, typ string, value, minAmount float64, usageLimit int, expires *time.Time) error {
	_, err := c.create(ctx, code, typ, value, minAmount, usageLimit, expires, nil, CouponOptions{})
	return err
}

// CreateWithOptions 新增带场景化配置的优惠码。
func (c *Coupons) CreateWithOptions(ctx context.Context, code, typ string, value, minAmount float64, usageLimit int, expires *time.Time, opts CouponOptions) error {
	_, err := c.create(ctx, code, typ, value, minAmount, usageLimit, expires, nil, opts)
	return err
}

func (c *Coupons) CreateForUser(ctx context.Context, code, typ string, value, minAmount float64, usageLimit int, expires *time.Time, userID int64) (int64, error) {
	return c.create(ctx, code, typ, value, minAmount, usageLimit, expires, &userID, CouponOptions{})
}

func (c *Coupons) create(ctx context.Context, code, typ string, value, minAmount float64, usageLimit int, expires *time.Time, userID *int64, opts CouponOptions) (int64, error) {
	if code == "" {
		return 0, errors.New("优惠码不能为空")
	}
	if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || math.IsNaN(minAmount) || math.IsInf(minAmount, 0) || minAmount < 0 || usageLimit < 0 || !(typ == "fixed" || (typ == "percent" && value <= 100)) {
		return 0, errors.New("优惠码参数无效")
	}
	scope := opts.ApplyScope
	if scope == "" {
		scope = "new"
	}
	if scope != "new" && scope != "renew" && scope != "both" {
		return 0, errors.New("优惠码适用范围无效")
	}
	if opts.Recurring < 0 {
		return 0, errors.New("循环期数无效")
	}
	for _, pid := range opts.NeedProductIDs {
		if pid <= 0 {
			return 0, errors.New("需求商品无效")
		}
	}
	need, _ := json.Marshal(opts.NeedProductIDs)
	if opts.NeedProductIDs == nil {
		need = []byte("[]")
	}
	var id int64
	err := c.db.QueryRowContext(ctx,
		`INSERT INTO coupons(code,type,value,min_amount,usage_limit,expires_at,active,user_id,apply_scope,recurring,need_product_ids)
		 VALUES($1,$2,$3,$4,$5,$6,true,$7,$8,$9,$10) RETURNING id`,
		code, typ, value, minAmount, usageLimit, expires, userID, scope, opts.Recurring, need).Scan(&id)
	return id, err
}

func strconvParse(s string) (float64, bool) {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return f, err == nil
}

func format2(f float64) string {
	return strconv.FormatFloat(f, 'f', 2, 64)
}
