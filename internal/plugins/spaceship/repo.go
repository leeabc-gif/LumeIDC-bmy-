package spaceship

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	ErrContactNotFound = errors.New("联系人不存在")
	ErrDomainNotFound  = errors.New("域名不存在")
	ErrDomainExists    = errors.New("该域名已存在")
	ErrOpNotFound      = errors.New("异步操作不存在")
	ErrPriceNotFound   = errors.New("该后缀未配置价格，请联系管理员")
	ErrTLDDisabled     = errors.New("该后缀暂不支持自助注册")
	ErrYearsOutOfRange = errors.New("注册年限超出该后缀允许范围")
)

// dbExec 数据库最小执行接口：*sql.DB 与 *sql.Tx 均满足，WithTx 复用同一批方法。
type dbExec interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// Repo Spaceship 插件数据库访问层。
type Repo struct{ db dbExec }

func NewRepo(db *sql.DB) *Repo { return &Repo{db: db} }

// WithTx 返回绑定同一事务的新 Repo（调用方负责 Commit/Rollback）。
// 返回 repoStore 接口：Plugin.repo 为接口类型，事务内替换需同类型（方法体不解引用 receiver，nil receiver 亦安全）。
func (r *Repo) WithTx(tx *sql.Tx) repoStore { return &Repo{db: tx} }

// ---- Contacts ----

type ContactRow struct {
	ID            int64
	ContactID     string
	UserID        sql.NullInt64
	Label         string
	FirstName     string
	LastName      string
	Organization  sql.NullString
	Email         string
	Address1      string
	Address2      sql.NullString
	City          string
	StateProvince sql.NullString
	PostalCode    sql.NullString
	Country       string
	Phone         string
	IsDefault     bool
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

func (r *Repo) SaveContact(ctx context.Context, c *ContactRow) (*ContactRow, error) {
	const upsert = `INSERT INTO plugin_spaceship_contacts
		(contact_id,user_id,label,first_name,last_name,organization,email,address1,address2,city,state_province,postal_code,country,phone,is_default)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)
		ON CONFLICT(contact_id) DO UPDATE SET
			user_id=EXCLUDED.user_id, label=EXCLUDED.label,
			first_name=EXCLUDED.first_name, last_name=EXCLUDED.last_name,
			organization=EXCLUDED.organization, email=EXCLUDED.email,
			address1=EXCLUDED.address1, address2=EXCLUDED.address2,
			city=EXCLUDED.city, state_province=EXCLUDED.state_province,
			postal_code=EXCLUDED.postal_code, country=EXCLUDED.country,
			phone=EXCLUDED.phone, is_default=EXCLUDED.is_default, updated_at=now()
		RETURNING id,contact_id,user_id,label,first_name,last_name,organization,email,address1,address2,city,state_province,postal_code,country,phone,is_default,created_at,updated_at`
	var out ContactRow
	var uid sql.NullInt64
	if c.UserID.Valid {
		uid = c.UserID
	}
	err := r.db.QueryRowContext(ctx, upsert,
		c.ContactID, uid, c.Label, c.FirstName, c.LastName, c.Organization, c.Email,
		c.Address1, c.Address2, c.City, c.StateProvince, c.PostalCode, c.Country, c.Phone, c.IsDefault,
	).Scan(&out.ID, &out.ContactID, &out.UserID, &out.Label, &out.FirstName, &out.LastName,
		&out.Organization, &out.Email, &out.Address1, &out.Address2, &out.City,
		&out.StateProvince, &out.PostalCode, &out.Country, &out.Phone, &out.IsDefault,
		&out.CreatedAt, &out.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func (r *Repo) GetContact(ctx context.Context, id int64) (*ContactRow, error) {
	const q = `SELECT id,contact_id,user_id,label,first_name,last_name,organization,email,address1,address2,city,state_province,postal_code,country,phone,is_default,created_at,updated_at
		FROM plugin_spaceship_contacts WHERE id=$1`
	var out ContactRow
	err := r.db.QueryRowContext(ctx, q, id).Scan(
		&out.ID, &out.ContactID, &out.UserID, &out.Label, &out.FirstName, &out.LastName,
		&out.Organization, &out.Email, &out.Address1, &out.Address2, &out.City,
		&out.StateProvince, &out.PostalCode, &out.Country, &out.Phone, &out.IsDefault,
		&out.CreatedAt, &out.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrContactNotFound
	}
	return &out, err
}

func (r *Repo) ListContacts(ctx context.Context, userID sql.NullInt64) ([]*ContactRow, error) {
	var rows *sql.Rows
	var err error
	if userID.Valid {
		rows, err = r.db.QueryContext(ctx,
			`SELECT id,contact_id,user_id,label,first_name,last_name,organization,email,address1,address2,city,state_province,postal_code,country,phone,is_default,created_at,updated_at
			 FROM plugin_spaceship_contacts WHERE user_id=$1 OR user_id IS NULL ORDER BY is_default DESC, id`, userID.Int64)
	} else {
		rows, err = r.db.QueryContext(ctx,
			`SELECT id,contact_id,user_id,label,first_name,last_name,organization,email,address1,address2,city,state_province,postal_code,country,phone,is_default,created_at,updated_at
			 FROM plugin_spaceship_contacts ORDER BY is_default DESC, id`)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*ContactRow
	for rows.Next() {
		var q ContactRow
		if err := rows.Scan(&q.ID, &q.ContactID, &q.UserID, &q.Label, &q.FirstName, &q.LastName,
			&q.Organization, &q.Email, &q.Address1, &q.Address2, &q.City,
			&q.StateProvince, &q.PostalCode, &q.Country, &q.Phone, &q.IsDefault,
			&q.CreatedAt, &q.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, &q)
	}
	return out, rows.Err()
}

func (r *Repo) DeleteContact(ctx context.Context, id int64) error {
	res, err := r.db.ExecContext(ctx, `DELETE FROM plugin_spaceship_contacts WHERE id=$1`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrContactNotFound
	}
	return nil
}

// SetDefaultContact 设置默认联系人，作用域严格限定：
// userID 有效＝只影响该用户的联系人；userID 无效（管理员共享模板）＝只影响共享模板。
// 历史实现在 userID 无效时执行不带 WHERE 的全表清空，会让所有用户的默认联系人一起失效。
func (r *Repo) SetDefaultContact(ctx context.Context, userID sql.NullInt64, id int64) error {
	// 设置目标必须落在同一作用域内：userID 有效＝该用户自己的联系人；
	// userID 无效＝仅共享模板。否则后台/越权调用可把他人联系人设为默认。
	if userID.Valid {
		_, _ = r.db.ExecContext(ctx, `UPDATE plugin_spaceship_contacts SET is_default=false WHERE user_id=$1`, userID.Int64)
		_, err := r.db.ExecContext(ctx, `UPDATE plugin_spaceship_contacts SET is_default=true WHERE id=$1 AND user_id=$2`, id, userID.Int64)
		return err
	}
	_, _ = r.db.ExecContext(ctx, `UPDATE plugin_spaceship_contacts SET is_default=false WHERE user_id IS NULL`)
	_, err := r.db.ExecContext(ctx, `UPDATE plugin_spaceship_contacts SET is_default=true WHERE id=$1 AND user_id IS NULL`, id)
	return err
}

func (r *Repo) GetDefaultContact(ctx context.Context, userID sql.NullInt64) (*ContactRow, error) {
	var row *sql.Row
	if userID.Valid {
		row = r.db.QueryRowContext(ctx,
			`SELECT id,contact_id,user_id,label,first_name,last_name,organization,email,address1,address2,city,state_province,postal_code,country,phone,is_default,created_at,updated_at
			 FROM plugin_spaceship_contacts WHERE is_default=true AND (user_id=$1 OR user_id IS NULL) ORDER BY CASE WHEN user_id=$1 THEN 0 ELSE 1 END LIMIT 1`,
			userID.Int64, userID.Int64)
	} else {
		row = r.db.QueryRowContext(ctx,
			`SELECT id,contact_id,user_id,label,first_name,last_name,organization,email,address1,address2,city,state_province,postal_code,country,phone,is_default,created_at,updated_at
			 FROM plugin_spaceship_contacts WHERE is_default=true LIMIT 1`)
	}
	var out ContactRow
	err := row.Scan(
		&out.ID, &out.ContactID, &out.UserID, &out.Label, &out.FirstName, &out.LastName,
		&out.Organization, &out.Email, &out.Address1, &out.Address2, &out.City,
		&out.StateProvince, &out.PostalCode, &out.Country, &out.Phone, &out.IsDefault,
		&out.CreatedAt, &out.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrContactNotFound
	}
	return &out, err
}

// ---- Domains ----

type DomainRow struct {
	ID                int64
	UserID            int64
	Domain            string
	WhoisID           sql.NullInt64
	ContactID         sql.NullString
	Years             int
	PaidAmountCents   int64
	Status            string
	PrivacyLevel      string
	AutoRenew         bool
	SpaceshipDomainID sql.NullString
	RegisteredAt      sql.NullTime
	ExpiresAt         sql.NullTime
	DeletedAt         sql.NullTime
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

func (r *Repo) CreateDomain(ctx context.Context, d *DomainRow) (int64, error) {
	const q = `INSERT INTO plugin_spaceship_domains
		(user_id,domain,whois_id,contact_id,years,paid_amount_cents,status,privacy_level,auto_renew)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING id`
	var whoisID sql.NullInt64
	var contactID sql.NullString
	if d.WhoisID.Valid {
		whoisID = d.WhoisID
	}
	if d.ContactID.Valid {
		contactID = d.ContactID
	}
	var id int64
	err := r.db.QueryRowContext(ctx, q, d.UserID, d.Domain, whoisID, contactID,
		d.Years, d.PaidAmountCents, d.Status, d.PrivacyLevel, d.AutoRenew).Scan(&id)
	return id, err
}

func (r *Repo) GetDomain(ctx context.Context, id int64) (*DomainRow, error) {
	const q = `SELECT id,user_id,domain,whois_id,contact_id,years,paid_amount_cents,status,privacy_level,auto_renew,spaceship_domain_id,registered_at,expires_at,deleted_at,created_at,updated_at
		FROM plugin_spaceship_domains WHERE id=$1`
	return scanDomain(r.db.QueryRowContext(ctx, q, id))
}

func (r *Repo) GetDomainByName(ctx context.Context, domain string) (*DomainRow, error) {
	const q = `SELECT id,user_id,domain,whois_id,contact_id,years,paid_amount_cents,status,privacy_level,auto_renew,spaceship_domain_id,registered_at,expires_at,deleted_at,created_at,updated_at
		FROM plugin_spaceship_domains WHERE domain=$1`
	return scanDomain(r.db.QueryRowContext(ctx, q, domain))
}

func scanDomain(row interface{ Scan(...any) error }) (*DomainRow, error) {
	var out DomainRow
	err := row.Scan(&out.ID, &out.UserID, &out.Domain, &out.WhoisID, &out.ContactID,
		&out.Years, &out.PaidAmountCents, &out.Status, &out.PrivacyLevel, &out.AutoRenew,
		&out.SpaceshipDomainID, &out.RegisteredAt, &out.ExpiresAt, &out.DeletedAt,
		&out.CreatedAt, &out.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrDomainNotFound
	}
	return &out, err
}

func (r *Repo) ListDomains(ctx context.Context, userID sql.NullInt64) ([]*DomainRow, error) {
	var rows *sql.Rows
	var err error
	if userID.Valid {
		rows, err = r.db.QueryContext(ctx,
			`SELECT id,user_id,domain,whois_id,contact_id,years,paid_amount_cents,status,privacy_level,auto_renew,spaceship_domain_id,registered_at,expires_at,deleted_at,created_at,updated_at
			 FROM plugin_spaceship_domains WHERE user_id=$1 ORDER BY id DESC`, userID.Int64)
	} else {
		rows, err = r.db.QueryContext(ctx,
			`SELECT id,user_id,domain,whois_id,contact_id,years,paid_amount_cents,status,privacy_level,auto_renew,spaceship_domain_id,registered_at,expires_at,deleted_at,created_at,updated_at
			 FROM plugin_spaceship_domains ORDER BY id DESC`)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*DomainRow
	for rows.Next() {
		q, err := scanDomain(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, q)
	}
	return out, rows.Err()
}

func (r *Repo) UpdateDomain(ctx context.Context, id int64, patches map[string]any) error {
	if len(patches) == 0 {
		return nil
	}
	var set []string
	var args []any
	i := 1
	for k, v := range patches {
		set = append(set, fmt.Sprintf("%s=$%d", k, i))
		args = append(args, v)
		i++
	}
	set = append(set, "updated_at=now()")
	args = append(args, id)
	q := "UPDATE plugin_spaceship_domains SET " + strings.Join(set, ",") + " WHERE id=$" + fmt.Sprintf("%d", i)
	_, err := r.db.ExecContext(ctx, q, args...)
	return err
}

// SetRegistered 注册成功后回补 Spaceship 域名 id + 注册/到期时间。
func (r *Repo) SetRegistered(ctx context.Context, id int64, spaceshipDomainID string, registeredAt, expiresAt time.Time) error {
	_, err := r.db.ExecContext(ctx, `UPDATE plugin_spaceship_domains SET
		spaceship_domain_id=$1, registered_at=$2, expires_at=$3, status='active', updated_at=now()
		WHERE id=$4`, spaceshipDomainID, registeredAt, expiresAt, id)
	return err
}

// ---- Operations ----

type OperationRow struct {
	ID          int64
	OperationID string
	DomainID    sql.NullInt64
	Domain      string
	OpType      string
	Status      string
	ErrorMsg    sql.NullString
	Result      sql.NullString // jsonb 以 string 读
	StartedAt   time.Time
	FinishedAt  sql.NullTime
}

func (r *Repo) CreateOperation(ctx context.Context, op *OperationRow) (int64, error) {
	const q = `INSERT INTO plugin_spaceship_operations
		(operation_id,domain_id,domain,op_type,status,error_msg,result,started_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8) RETURNING id`
	var did sql.NullInt64
	if op.DomainID.Valid {
		did = op.DomainID
	}
	var errMsg sql.NullString
	if op.ErrorMsg.Valid {
		errMsg = op.ErrorMsg
	}
	var result sql.NullString
	if op.Result.Valid {
		result = op.Result
	}
	var id int64
	err := r.db.QueryRowContext(ctx, q, op.OperationID, did, op.Domain, op.OpType,
		op.Status, errMsg, result, op.StartedAt).Scan(&id)
	return id, err
}

func (r *Repo) GetOperation(ctx context.Context, id int64) (*OperationRow, error) {
	return scanOp(r.db.QueryRowContext(ctx,
		`SELECT id,operation_id,domain_id,domain,op_type,status,error_msg,result,started_at,finished_at
		 FROM plugin_spaceship_operations WHERE id=$1`, id))
}

func (r *Repo) ListPending(ctx context.Context, limit int) ([]*OperationRow, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id,operation_id,domain_id,domain,op_type,status,error_msg,result,started_at,finished_at
		 FROM plugin_spaceship_operations WHERE status='pending' AND started_at > now() - interval '15 minutes'
		 ORDER BY started_at LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*OperationRow
	for rows.Next() {
		q, err := scanOp(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, q)
	}
	return out, rows.Err()
}

func scanOp(row interface{ Scan(...any) error }) (*OperationRow, error) {
	var out OperationRow
	err := row.Scan(&out.ID, &out.OperationID, &out.DomainID, &out.Domain, &out.OpType,
		&out.Status, &out.ErrorMsg, &out.Result, &out.StartedAt, &out.FinishedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrOpNotFound
	}
	return &out, err
}

func (r *Repo) MarkSuccess(ctx context.Context, id int64, result any) error {
	var resultStr sql.NullString
	if result != nil {
		b, err := json.Marshal(result)
		if err == nil {
			resultStr = sql.NullString{String: string(b), Valid: true}
		}
	}
	_, err := r.db.ExecContext(ctx, `UPDATE plugin_spaceship_operations SET
		status='success', result=$1, finished_at=now() WHERE id=$2`, resultStr, id)
	return err
}

func (r *Repo) MarkFailed(ctx context.Context, id int64, errMsg string, result any) error {
	var errNS sql.NullString
	if errMsg != "" {
		errNS = sql.NullString{String: errMsg, Valid: true}
	}
	var resultStr sql.NullString
	if result != nil {
		b, err := json.Marshal(result)
		if err == nil {
			resultStr = sql.NullString{String: string(b), Valid: true}
		}
	}
	_, err := r.db.ExecContext(ctx, `UPDATE plugin_spaceship_operations SET
		status='failed', error_msg=$1, result=$2, finished_at=now() WHERE id=$3`, errNS, resultStr, id)
	return err
}

// MarkPendingRetried 把 pending 操作的时间窗复位为 now()。ListPending 只挑
// "started_at > now() - 15min" 的 pending，cron 因此会丢弃卡死操作；管理员
// 显式 retry 应能把操作重新放回窗口，让下一轮 cron 也能正常拣到再轮询。
// 返回 true 表示确实改到了一条 pending 记录（终态记录不会被"复活"）。
func (r *Repo) MarkPendingRetried(ctx context.Context, id int64) (bool, error) {
	res, err := r.db.ExecContext(ctx,
		`UPDATE plugin_spaceship_operations SET started_at=now() WHERE id=$1 AND status='pending'`, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// UpdateOpDomain 注册成功后把 domain_id 关联上（CreateOperation 时 domain_id 还未知）。
func (r *Repo) UpdateOpDomain(ctx context.Context, opID, domainID int64) error {
	_, err := r.db.ExecContext(ctx, `UPDATE plugin_spaceship_operations SET domain_id=$1 WHERE id=$2`, domainID, opID)
	return err
}

// ListOperations 分页查操作日志。
func (r *Repo) ListOperations(ctx context.Context, limit, offset int) ([]*OperationRow, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id,operation_id,domain_id,domain,op_type,status,error_msg,result,started_at,finished_at
		 FROM plugin_spaceship_operations ORDER BY started_at DESC LIMIT $1 OFFSET $2`, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*OperationRow
	for rows.Next() {
		q, err := scanOp(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, q)
	}
	return out, rows.Err()
}

// CountOperations 操作日志总数。
func (r *Repo) CountOperations(ctx context.Context) (int, error) {
	var n int
	err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM plugin_spaceship_operations`).Scan(&n)
	return n, err
}

// ---- Prices（服务端定价，杜绝客户端传价） ----

// PriceRow 价目表一行。
type PriceRow struct {
	ID            int64
	TLD           string
	RegisterCents int64
	RenewCents    int64
	Currency      string
	Enabled       bool
	MinYears      int
	MaxYears      int
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// GetPrice 按后缀读价目（tld 不含点，小写）。无记录返回 ErrPriceNotFound。
func (r *Repo) GetPrice(ctx context.Context, tld string) (*PriceRow, error) {
	const q = `SELECT id,tld,register_cents,renew_cents,currency,enabled,min_years,max_years,created_at,updated_at
		FROM plugin_spaceship_prices WHERE tld=$1`
	var out PriceRow
	err := r.db.QueryRowContext(ctx, q, strings.ToLower(tld)).Scan(
		&out.ID, &out.TLD, &out.RegisterCents, &out.RenewCents, &out.Currency,
		&out.Enabled, &out.MinYears, &out.MaxYears, &out.CreatedAt, &out.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrPriceNotFound
	}
	return &out, err
}

// ListPrices 价目表（后台管理用），按后缀排序。
func (r *Repo) ListPrices(ctx context.Context) ([]*PriceRow, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id,tld,register_cents,renew_cents,currency,enabled,min_years,max_years,created_at,updated_at
		 FROM plugin_spaceship_prices ORDER BY tld`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*PriceRow
	for rows.Next() {
		var q PriceRow
		if err := rows.Scan(&q.ID, &q.TLD, &q.RegisterCents, &q.RenewCents, &q.Currency,
			&q.Enabled, &q.MinYears, &q.MaxYears, &q.CreatedAt, &q.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, &q)
	}
	return out, rows.Err()
}

// UpsertPrice 新增或更新某后缀价格（后台价格管理）。
func (r *Repo) UpsertPrice(ctx context.Context, row *PriceRow) (*PriceRow, error) {
	const q = `INSERT INTO plugin_spaceship_prices
		(tld,register_cents,renew_cents,currency,enabled,min_years,max_years)
		VALUES($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT(tld) DO UPDATE SET
			register_cents=EXCLUDED.register_cents, renew_cents=EXCLUDED.renew_cents,
			currency=EXCLUDED.currency, enabled=EXCLUDED.enabled,
			min_years=EXCLUDED.min_years, max_years=EXCLUDED.max_years, updated_at=now()
		RETURNING id,tld,register_cents,renew_cents,currency,enabled,min_years,max_years,created_at,updated_at`
	var out PriceRow
	cur := row.Currency
	if cur == "" {
		cur = "CNY"
	}
	minY, maxY := row.MinYears, row.MaxYears
	if minY < 1 {
		minY = 1
	}
	if maxY < minY {
		maxY = 10
	}
	err := r.db.QueryRowContext(ctx, q, strings.ToLower(row.TLD), row.RegisterCents, row.RenewCents,
		cur, row.Enabled, minY, maxY).Scan(
		&out.ID, &out.TLD, &out.RegisterCents, &out.RenewCents, &out.Currency,
		&out.Enabled, &out.MinYears, &out.MaxYears, &out.CreatedAt, &out.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// DeletePrice 删除某后缀价目（删除后该后缀不可自助注册/续费）。
func (r *Repo) DeletePrice(ctx context.Context, tld string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM plugin_spaceship_prices WHERE tld=$1`, strings.ToLower(tld))
	return err
}

// ---- Orders（扣款/退款轨迹，供对账） ----

// OrderRow 一笔注册/续费扣款。
type OrderRow struct {
	ID          int64
	DomainID    sql.NullInt64
	Domain      string
	UserID      int64
	Kind        string // register / renew
	Years       int
	AmountCents int64
	Status      string // paid / refunded
	Note        sql.NullString
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// CreateOrder 记录扣款（在扣款事务内调用）。
func (r *Repo) CreateOrder(ctx context.Context, o *OrderRow) (int64, error) {
	const q = `INSERT INTO plugin_spaceship_orders
		(domain_id,domain,user_id,kind,years,amount_cents,status,note)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8) RETURNING id`
	var did sql.NullInt64
	if o.DomainID.Valid {
		did = o.DomainID
	}
	var note sql.NullString
	if o.Note.Valid {
		note = o.Note
	}
	status := o.Status
	if status == "" {
		status = "paid"
	}
	var id int64
	err := r.db.QueryRowContext(ctx, q, did, o.Domain, o.UserID, o.Kind, o.Years,
		o.AmountCents, status, note).Scan(&id)
	return id, err
}

// RefundOrderIfPaid 原子认领退款资格：仅当 status='paid' 时置为 refunded。
// 返回 true 表示本调用抢到退款资格（RowsAffected==1），调用方据此才执行余额回补；
// 并发/重复调用只有一个赢家，从根上杜绝"查单→加钱→标记"三步竞态导致的双倍退款。
func (r *Repo) RefundOrderIfPaid(ctx context.Context, id int64) (bool, error) {
	res, err := r.db.ExecContext(ctx,
		`UPDATE plugin_spaceship_orders SET status='refunded', updated_at=now() WHERE id=$1 AND status='paid'`, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// UpdateOrderDomain 上游受理后回填订单 domain_id（注册订单创建时域名尚未落库）。
func (r *Repo) UpdateOrderDomain(ctx context.Context, orderID, domainID int64) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE plugin_spaceship_orders SET domain_id=$1, updated_at=now() WHERE id=$2`, domainID, orderID)
	return err
}

// LatestPaidOrder 取某域名最近一笔未退款的订单（失败自动退款用，保证幂等）。
func (r *Repo) LatestPaidOrder(ctx context.Context, domainID int64, kind string) (*OrderRow, error) {
	var out OrderRow
	err := r.db.QueryRowContext(ctx,
		`SELECT id,domain_id,domain,user_id,kind,years,amount_cents,status,note,created_at,updated_at
		 FROM plugin_spaceship_orders
		 WHERE domain_id=$1 AND kind=$2 AND status='paid' ORDER BY id DESC LIMIT 1`,
		domainID, kind).Scan(&out.ID, &out.DomainID, &out.Domain, &out.UserID, &out.Kind,
		&out.Years, &out.AmountCents, &out.Status, &out.Note, &out.CreatedAt, &out.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// ListOrders 订单列表（后台对账/我的订单）。
func (r *Repo) ListOrders(ctx context.Context, userID sql.NullInt64, limit, offset int) ([]*OrderRow, error) {
	var rows *sql.Rows
	var err error
	if userID.Valid {
		rows, err = r.db.QueryContext(ctx,
			`SELECT id,domain_id,domain,user_id,kind,years,amount_cents,status,note,created_at,updated_at
			 FROM plugin_spaceship_orders WHERE user_id=$1 ORDER BY id DESC LIMIT $2 OFFSET $3`,
			userID.Int64, limit, offset)
	} else {
		rows, err = r.db.QueryContext(ctx,
			`SELECT id,domain_id,domain,user_id,kind,years,amount_cents,status,note,created_at,updated_at
			 FROM plugin_spaceship_orders ORDER BY id DESC LIMIT $1 OFFSET $2`, limit, offset)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*OrderRow
	for rows.Next() {
		var q OrderRow
		if err := rows.Scan(&q.ID, &q.DomainID, &q.Domain, &q.UserID, &q.Kind, &q.Years,
			&q.AmountCents, &q.Status, &q.Note, &q.CreatedAt, &q.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, &q)
	}
	return out, rows.Err()
}
