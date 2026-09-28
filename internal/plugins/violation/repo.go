package violation

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// ErrNotFound 记录/公告不存在。
var ErrNotFound = errors.New("记录不存在")

// ---- 违规记录 ----

// Records 违规记录数据访问（表 plugin_violation_records）。
type Records struct{ db *sql.DB }

func NewRecords(db *sql.DB) *Records { return &Records{db: db} }

// Record 违规记录（字段同表）。
type Record struct {
	ID          int64
	UserID      int64
	Type        string
	Level       string // light | medium | severe
	Description string
	EvidenceURL string
	Action      string
	StartsAt    sql.NullTime
	ExpiresAt   sql.NullTime
	Public      bool
	HandledBy   int64
	Note        string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// RecordAdminRow 列表行（附带用户信息；公示列表UserName 为脱敏名、UserEmail 为空）。
type RecordAdminRow struct {
	Record
	UserName  string
	UserEmail string
}

const recordCols = `id,user_id,type,level,description,evidence_url,action,starts_at,expires_at,public,handled_by,note,created_at,updated_at`

func scanRecord(row interface{ Scan(...any) error }) (*Record, error) {
	var r Record
	if err := row.Scan(&r.ID, &r.UserID, &r.Type, &r.Level, &r.Description, &r.EvidenceURL,
		&r.Action, &r.StartsAt, &r.ExpiresAt, &r.Public, &r.HandledBy, &r.Note,
		&r.CreatedAt, &r.UpdatedAt); err != nil {
		return nil, err
	}
	return &r, nil
}

func scanRecordWithUser(row interface{ Scan(...any) error }) (*RecordAdminRow, error) {
	var r RecordAdminRow
	if err := row.Scan(&r.ID, &r.UserID, &r.Type, &r.Level, &r.Description, &r.EvidenceURL,
		&r.Action, &r.StartsAt, &r.ExpiresAt, &r.Public, &r.HandledBy, &r.Note,
		&r.CreatedAt, &r.UpdatedAt, &r.UserName, &r.UserEmail); err != nil {
		return nil, err
	}
	return &r, nil
}

// Create 新增违规记录，返回新记录 ID。
func (r *Records) Create(ctx context.Context, rec *Record) (int64, error) {
	var id int64
	err := r.db.QueryRowContext(ctx,
		`INSERT INTO plugin_violation_records
		 (user_id,type,level,description,evidence_url,action,starts_at,expires_at,public,handled_by,note)
		 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) RETURNING id`,
		rec.UserID, rec.Type, rec.Level, rec.Description, rec.EvidenceURL, rec.Action,
		rec.StartsAt, rec.ExpiresAt, rec.Public, rec.HandledBy, rec.Note).Scan(&id)
	return id, err
}

// Update 全量更新记录（user_id/created_at 不可改）。
func (r *Records) Update(ctx context.Context, rec *Record) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE plugin_violation_records
		 SET type=$2,level=$3,description=$4,evidence_url=$5,action=$6,starts_at=$7,expires_at=$8,
		     public=$9,handled_by=$10,note=$11,updated_at=now()
		 WHERE id=$1`,
		rec.ID, rec.Type, rec.Level, rec.Description, rec.EvidenceURL, rec.Action,
		rec.StartsAt, rec.ExpiresAt, rec.Public, rec.HandledBy, rec.Note)
	return err
}

// Get 按 ID 取记录；不存在返回 (nil, nil)。
func (r *Records) Get(ctx context.Context, id int64) (*Record, error) {
	rec, err := scanRecord(r.db.QueryRowContext(ctx,
		`SELECT `+recordCols+` FROM plugin_violation_records WHERE id=$1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return rec, err
}

// Delete 删除记录；返回是否确有记录被删（调用方据此决定是否外发事件）。
func (r *Records) Delete(ctx context.Context, id int64) (bool, error) {
	res, err := r.db.ExecContext(ctx, `DELETE FROM plugin_violation_records WHERE id=$1`, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// RecordFilter 后台列表筛选条件（Keyword 匹配用户邮箱/昵称/描述）。
type RecordFilter struct {
	Keyword string
	Level   string // 空=全部
	Type    string // 空=全部
	Page    int
	Limit   int
}

// AdminList 后台分页列表（JOIN users 取邮箱/昵称），返回行与总数。
func (r *Records) AdminList(ctx context.Context, f RecordFilter) ([]RecordAdminRow, int, error) {
	where := ` WHERE ($1='' OR u.email ILIKE '%'||$1||'%' OR coalesce(nullif(u.name,''),'') ILIKE '%'||$1||'%' OR r.description ILIKE '%'||$1||'%')
	           AND ($2='' OR r.level=$2)
	           AND ($3='' OR r.type=$3)`
	var total int
	if err := r.db.QueryRowContext(ctx,
		`SELECT count(*) FROM plugin_violation_records r JOIN users u ON u.id=r.user_id`+where,
		f.Keyword, f.Level, f.Type).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := r.db.QueryContext(ctx,
		`SELECT r.id,r.user_id,r.type,r.level,r.description,r.evidence_url,r.action,
		        r.starts_at,r.expires_at,r.public,r.handled_by,r.note,r.created_at,r.updated_at,
		        coalesce(nullif(u.name,''),u.email), u.email
		 FROM plugin_violation_records r
		 JOIN users u ON u.id=r.user_id`+where+`
		 ORDER BY r.id DESC LIMIT $4 OFFSET $5`,
		f.Keyword, f.Level, f.Type, f.Limit, (f.Page-1)*f.Limit)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := make([]RecordAdminRow, 0, f.Limit)
	for rows.Next() {
		r, err := scanRecordWithUser(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, *r)
	}
	return out, total, rows.Err()
}

// Stats 记录统计：总数、生效中（有效期内）、公示中（公示且在有效期内）。
func (r *Records) Stats(ctx context.Context) (total, active, public int, err error) {
	err = r.db.QueryRowContext(ctx,
		`SELECT count(*),
		        count(*) FILTER (WHERE (starts_at IS NULL OR starts_at<=now())
		                            AND (expires_at IS NULL OR expires_at>now())),
		        count(*) FILTER (WHERE public
		                            AND (starts_at IS NULL OR starts_at<=now())
		                            AND (expires_at IS NULL OR expires_at>now()))
		   FROM plugin_violation_records`).Scan(&total, &active, &public)
	return total, active, public, err
}

// PublicList 前台公示分页（public=true 且在有效期内；用户信息脱敏、无 email）。
func (r *Records) PublicList(ctx context.Context, page, pageSize int) ([]RecordAdminRow, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT r.id,r.user_id,r.type,r.level,r.description,r.evidence_url,r.action,
		        r.starts_at,r.expires_at,r.public,r.handled_by,r.note,r.created_at,r.updated_at,
		        coalesce(nullif(u.name,''),''), ''
		 FROM plugin_violation_records r
		 JOIN users u ON u.id=r.user_id
		 WHERE r.public AND (r.starts_at IS NULL OR r.starts_at<=now())
		   AND (r.expires_at IS NULL OR r.expires_at>now())
		 ORDER BY r.id DESC LIMIT $1 OFFSET $2`,
		pageSize, (page-1)*pageSize)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]RecordAdminRow, 0, pageSize)
	for rows.Next() {
		r, err := scanRecordWithUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

// PublicCount 前台公示记录总数（public=true 且在有效期内），供分页。
func (r *Records) PublicCount(ctx context.Context) (int, error) {
	var n int
	err := r.db.QueryRowContext(ctx,
		`SELECT count(*) FROM plugin_violation_records
		 WHERE public AND (starts_at IS NULL OR starts_at<=now())
		   AND (expires_at IS NULL OR expires_at>now())`).Scan(&n)
	return n, err
}

// PublicGet 前台记录详情（严格 public+有效期）；不满足返回 (nil, nil)。
func (r *Records) PublicGet(ctx context.Context, id int64) (*RecordAdminRow, error) {
	row, err := scanRecordWithUser(r.db.QueryRowContext(ctx,
		`SELECT r.id,r.user_id,r.type,r.level,r.description,r.evidence_url,r.action,
		        r.starts_at,r.expires_at,r.public,r.handled_by,r.note,r.created_at,r.updated_at,
		        coalesce(nullif(u.name,''),''), ''
		 FROM plugin_violation_records r
		 JOIN users u ON u.id=r.user_id
		 WHERE r.id=$1 AND r.public AND (r.starts_at IS NULL OR r.starts_at<=now())
		   AND (r.expires_at IS NULL OR r.expires_at>now())`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return row, err
}

// ---- 违规公示公告 ----

// Announcements 公示公告数据访问（表 plugin_violation_announcements）。
type Announcements struct{ db *sql.DB }

func NewAnnouncements(db *sql.DB) *Announcements { return &Announcements{db: db} }

// ViolationAnnouncement 公示公告（字段同表）。
type ViolationAnnouncement struct {
	ID        int64
	Title     string
	Content   string
	Pinned    bool
	Hidden    bool
	Reads     int64
	CreatedBy int64
	CreatedAt time.Time
	UpdatedAt time.Time
}

const annCols = `id,title,content,pinned,hidden,reads,created_by,created_at,updated_at`

func scanAnnouncement(row interface{ Scan(...any) error }) (*ViolationAnnouncement, error) {
	var a ViolationAnnouncement
	if err := row.Scan(&a.ID, &a.Title, &a.Content, &a.Pinned, &a.Hidden, &a.Reads,
		&a.CreatedBy, &a.CreatedAt, &a.UpdatedAt); err != nil {
		return nil, err
	}
	return &a, nil
}

// AdminList 后台列表（全部，置顶优先）。
func (a *Announcements) AdminList(ctx context.Context) ([]ViolationAnnouncement, error) {
	rows, err := a.db.QueryContext(ctx,
		`SELECT `+annCols+` FROM plugin_violation_announcements ORDER BY pinned DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]ViolationAnnouncement, 0, 16)
	for rows.Next() {
		an, err := scanAnnouncement(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *an)
	}
	return out, rows.Err()
}

// PublicList 前台可见公告（hidden=false，置顶优先）。
func (a *Announcements) PublicList(ctx context.Context) ([]ViolationAnnouncement, error) {
	rows, err := a.db.QueryContext(ctx,
		`SELECT `+annCols+` FROM plugin_violation_announcements
		 WHERE hidden=false ORDER BY pinned DESC, id DESC LIMIT 50`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]ViolationAnnouncement, 0, 16)
	for rows.Next() {
		an, err := scanAnnouncement(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *an)
	}
	return out, rows.Err()
}

// Save 新增(id=0)或更新，返回公告 ID。
func (a *Announcements) Save(ctx context.Context, ann *ViolationAnnouncement) (int64, error) {
	if ann.ID == 0 {
		var id int64
		err := a.db.QueryRowContext(ctx,
			`INSERT INTO plugin_violation_announcements(title,content,pinned,hidden,created_by)
			 VALUES($1,$2,$3,$4,$5) RETURNING id`,
			ann.Title, ann.Content, ann.Pinned, ann.Hidden, ann.CreatedBy).Scan(&id)
		return id, err
	}
	_, err := a.db.ExecContext(ctx,
		`UPDATE plugin_violation_announcements
		 SET title=$2,content=$3,pinned=$4,hidden=$5,updated_at=now() WHERE id=$1`,
		ann.ID, ann.Title, ann.Content, ann.Pinned, ann.Hidden)
	return ann.ID, err
}

// Get 按 ID 取公告；不存在返回 (nil, nil)。
func (a *Announcements) Get(ctx context.Context, id int64) (*ViolationAnnouncement, error) {
	an, err := scanAnnouncement(a.db.QueryRowContext(ctx,
		`SELECT `+annCols+` FROM plugin_violation_announcements WHERE id=$1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return an, err
}

// IncrReads 阅读量 +1。
func (a *Announcements) IncrReads(ctx context.Context, id int64) error {
	_, err := a.db.ExecContext(ctx,
		`UPDATE plugin_violation_announcements SET reads=reads+1 WHERE id=$1`, id)
	return err
}

// Delete 删除公告。
func (a *Announcements) Delete(ctx context.Context, id int64) error {
	_, err := a.db.ExecContext(ctx,
		`DELETE FROM plugin_violation_announcements WHERE id=$1`, id)
	return err
}

// ---- 用户查询（只读：添加违规时选择用户） ----

// Users 用户查询。
type Users struct{ db *sql.DB }

func NewUsers(db *sql.DB) *Users { return &Users{db: db} }

// UserBasic 用户基础信息。
type UserBasic struct {
	ID    int64
	Email string
	Name  string
}

// Get 按 ID 取用户；不存在返回 (nil, nil)。
func (u *Users) Get(ctx context.Context, id int64) (*UserBasic, error) {
	var usr UserBasic
	err := u.db.QueryRowContext(ctx,
		`SELECT id,email,coalesce(nullif(name,''),'') FROM users WHERE id=$1`, id).
		Scan(&usr.ID, &usr.Email, &usr.Name)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &usr, err
}

// Search 按邮箱/昵称/ID 模糊搜索（添加违规表单远程搜索），最多 limit 条。
func (u *Users) Search(ctx context.Context, keyword string, limit int) ([]UserBasic, error) {
	rows, err := u.db.QueryContext(ctx,
		`SELECT id,email,coalesce(nullif(name,''),'') FROM users
		 WHERE ($1='' OR email ILIKE '%'||$1||'%' OR name ILIKE '%'||$1||'%'
		        OR CAST(id AS TEXT) = $1)
		 ORDER BY id DESC LIMIT $2`, keyword, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]UserBasic, 0, limit)
	for rows.Next() {
		var usr UserBasic
		if err := rows.Scan(&usr.ID, &usr.Email, &usr.Name); err != nil {
			return nil, err
		}
		out = append(out, usr)
	}
	return out, rows.Err()
}
