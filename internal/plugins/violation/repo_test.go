package violation

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

// setupViolationDB 执行幂等迁移建表并插入测试用户；无 TEST_DATABASE_DSN 跳过。
// 清理仅删本用例产生的数据（user_id/created_by 均指向测试用户）。
func setupViolationDB(t *testing.T) (*sql.DB, int64) {
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
	email := fmt.Sprintf("violation_test_%d@example.com", time.Now().UnixNano())
	var uid int64
	if err := d.QueryRowContext(ctx,
		`INSERT INTO users(email,password_hash,name) VALUES($1,'x','测试用户') RETURNING id`,
		email).Scan(&uid); err != nil {
		d.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c := context.Background()
		d.ExecContext(c, `DELETE FROM plugin_violation_records WHERE user_id=$1`, uid)
		d.ExecContext(c, `DELETE FROM plugin_violation_announcements WHERE created_by=$1`, uid)
		d.ExecContext(c, `DELETE FROM users WHERE id=$1`, uid)
		d.Close()
	})
	return d, uid
}

func TestRecordsCRUD(t *testing.T) {
	d, uid := setupViolationDB(t)
	ctx := context.Background()
	recs := NewRecords(d)

	expires := time.Now().Add(24 * time.Hour)
	id, err := recs.Create(ctx, &Record{
		UserID: uid, Type: "垃圾邮件", Level: "medium", Description: "发送广告",
		Action: "警告", ExpiresAt: sql.NullTime{Time: expires, Valid: true}, Public: true, HandledBy: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := recs.Get(ctx, id)
	if err != nil || got == nil {
		t.Fatalf("取记录失败: %+v err=%v", got, err)
	}
	if got.Level != "medium" || !got.Public || got.UserID != uid {
		t.Fatalf("记录字段不符: %+v", got)
	}

	got.Type = "滥用资源"
	got.Level = "severe"
	got.Public = false
	if err := recs.Update(ctx, got); err != nil {
		t.Fatal(err)
	}
	again, err := recs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if again.Type != "滥用资源" || again.Level != "severe" || again.Public {
		t.Fatalf("更新未生效: %+v", again)
	}

	deleted, err := recs.Delete(ctx, id)
	if err != nil || !deleted {
		t.Fatalf("删除失败: deleted=%v err=%v", deleted, err)
	}
	if gone, err := recs.Get(ctx, id); err != nil || gone != nil {
		t.Fatalf("删除后不应存在: %+v err=%v", gone, err)
	}
	// 再删一次应报告未删除（不存在的 ID 不得视为成功）。
	if again, err := recs.Delete(ctx, id); err != nil || again {
		t.Fatalf("重复删除应 deleted=false: again=%v err=%v", again, err)
	}
}

func TestRecordsAdminListFilter(t *testing.T) {
	d, uid := setupViolationDB(t)
	ctx := context.Background()
	recs := NewRecords(d)

	// 两条：medium/垃圾邮件（关键词「广告」）、severe/欺诈行为。
	id1, err := recs.Create(ctx, &Record{UserID: uid, Type: "垃圾邮件", Level: "medium", Description: "群发广告邮件", HandledBy: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := recs.Create(ctx, &Record{UserID: uid, Type: "欺诈行为", Level: "severe", Description: "虚假宣传", HandledBy: 1}); err != nil {
		t.Fatal(err)
	}

	rows, total, err := recs.AdminList(ctx, RecordFilter{Page: 1, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 || len(rows) != 2 {
		t.Fatalf("全量列表应为 2 条: total=%d len=%d", total, len(rows))
	}
	if rows[0].ID <= rows[1].ID {
		t.Fatal("应按 id 倒序")
	}
	if rows[0].UserEmail == "" {
		t.Fatal("后台列表应带回邮箱")
	}

	rows, total, err = recs.AdminList(ctx, RecordFilter{Level: "severe", Page: 1, Limit: 10})
	if err != nil || total != 1 || len(rows) != 1 || rows[0].Level != "severe" {
		t.Fatalf("等级筛选错误: total=%d rows=%+v err=%v", total, rows, err)
	}

	rows, total, err = recs.AdminList(ctx, RecordFilter{Type: "垃圾邮件", Page: 1, Limit: 10})
	if err != nil || total != 1 || rows[0].ID != id1 {
		t.Fatalf("类型筛选错误: total=%d rows=%+v err=%v", total, rows, err)
	}

	rows, _, err = recs.AdminList(ctx, RecordFilter{Keyword: "广告", Page: 1, Limit: 10})
	if err != nil || len(rows) != 1 || rows[0].ID != id1 {
		t.Fatalf("关键词（描述）筛选错误: rows=%+v err=%v", rows, err)
	}

	// 分页：limit=1 两页。
	page1, total, err := recs.AdminList(ctx, RecordFilter{Page: 1, Limit: 1})
	if err != nil || total != 2 || len(page1) != 1 {
		t.Fatalf("第一页错误: total=%d len=%d err=%v", total, len(page1), err)
	}
	page2, _, err := recs.AdminList(ctx, RecordFilter{Page: 2, Limit: 1})
	if err != nil || len(page2) != 1 {
		t.Fatalf("第二页错误: len=%d err=%v", len(page2), err)
	}
	if page1[0].ID == page2[0].ID {
		t.Fatal("分页重复")
	}
}

func TestRecordsStats(t *testing.T) {
	d, uid := setupViolationDB(t)
	ctx := context.Background()
	recs := NewRecords(d)

	// 四条：公示有效 / 不公示 / 已过期 / 未到生效期。
	if _, err := recs.Create(ctx, &Record{UserID: uid, Level: "light", Description: "公示有效", Public: true, HandledBy: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := recs.Create(ctx, &Record{UserID: uid, Level: "light", Description: "不公示", Public: false, HandledBy: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := recs.Create(ctx, &Record{
		UserID: uid, Level: "light", Description: "已过期", Public: true, HandledBy: 1,
		ExpiresAt: sql.NullTime{Time: time.Now().Add(-time.Hour), Valid: true},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := recs.Create(ctx, &Record{
		UserID: uid, Level: "light", Description: "未生效", Public: true, HandledBy: 1,
		StartsAt: sql.NullTime{Time: time.Now().Add(time.Hour), Valid: true},
	}); err != nil {
		t.Fatal(err)
	}

	total, active, public, err := recs.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if total != 4 || active != 2 || public != 1 {
		t.Fatalf("统计应为 total=4 active=2 public=1，实得 total=%d active=%d public=%d", total, active, public)
	}
}

func TestRecordsPublicVisibility(t *testing.T) {
	d, uid := setupViolationDB(t)
	ctx := context.Background()
	recs := NewRecords(d)

	// 四条：公示有效 / 不公示 / 已过期 / 未到生效期。
	valid, err := recs.Create(ctx, &Record{UserID: uid, Level: "light", Description: "公示有效", Public: true, HandledBy: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := recs.Create(ctx, &Record{UserID: uid, Level: "light", Description: "不公示", Public: false, HandledBy: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := recs.Create(ctx, &Record{
		UserID: uid, Level: "light", Description: "已过期", Public: true, HandledBy: 1,
		ExpiresAt: sql.NullTime{Time: time.Now().Add(-time.Hour), Valid: true},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := recs.Create(ctx, &Record{
		UserID: uid, Level: "light", Description: "未生效", Public: true, HandledBy: 1,
		StartsAt: sql.NullTime{Time: time.Now().Add(time.Hour), Valid: true},
	}); err != nil {
		t.Fatal(err)
	}

	rows, err := recs.PublicList(ctx, 1, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != valid {
		t.Fatalf("前台公示应只含 1 条有效记录: %+v", rows)
	}
	if rows[0].UserEmail != "" {
		t.Fatal("前台公示不应外泄邮箱")
	}
	if rows[0].UserName != "测试用户" {
		t.Fatalf("前台应带回昵称供脱敏展示: %q", rows[0].UserName)
	}

	count, err := recs.PublicCount(ctx)
	if err != nil || count != 1 {
		t.Fatalf("前台计数应为 1: %d err=%v", count, err)
	}

	got, err := recs.PublicGet(ctx, valid)
	if err != nil || got == nil {
		t.Fatalf("公示详情取不到: %+v err=%v", got, err)
	}
}

func TestAnnouncementsFlow(t *testing.T) {
	d, uid := setupViolationDB(t)
	ctx := context.Background()
	anns := NewAnnouncements(d)

	id, err := anns.Save(ctx, &ViolationAnnouncement{Title: "整顿公告", Content: "严禁滥用", CreatedBy: uid})
	if err != nil {
		t.Fatal(err)
	}
	got, err := anns.Get(ctx, id)
	if err != nil || got == nil || got.Title != "整顿公告" || got.Hidden {
		t.Fatalf("公告保存/读取失败: %+v err=%v", got, err)
	}

	pub, err := anns.PublicList(ctx)
	if err != nil || len(pub) != 1 {
		t.Fatalf("前台应可见 1 条: %d err=%v", len(pub), err)
	}

	// 置顶 + 隐藏。
	if _, err := anns.Save(ctx, &ViolationAnnouncement{ID: id, Title: "整顿公告", Content: "严禁滥用", Pinned: true, Hidden: true, CreatedBy: uid}); err != nil {
		t.Fatal(err)
	}
	pub, err = anns.PublicList(ctx)
	if err != nil || len(pub) != 0 {
		t.Fatalf("隐藏后前台不应可见: %d err=%v", len(pub), err)
	}
	admin, err := anns.AdminList(ctx)
	if err != nil || len(admin) != 1 || !admin[0].Pinned || !admin[0].Hidden {
		t.Fatalf("后台应仍可见且带置顶/隐藏标记: %+v err=%v", admin, err)
	}

	if err := anns.IncrReads(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := anns.IncrReads(ctx, id); err != nil {
		t.Fatal(err)
	}
	got, err = anns.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Reads != 2 {
		t.Fatalf("阅读量应累计为 2: %d", got.Reads)
	}

	if err := anns.Delete(ctx, id); err != nil {
		t.Fatal(err)
	}
	if gone, err := anns.Get(ctx, id); err != nil || gone != nil {
		t.Fatalf("删除后不应存在: %+v err=%v", gone, err)
	}
}

func TestUsersSearch(t *testing.T) {
	d, uid := setupViolationDB(t)
	ctx := context.Background()
	users := NewUsers(d)

	got, err := users.Get(ctx, uid)
	if err != nil || got == nil || got.Email == "" {
		t.Fatalf("取用户失败: %+v err=%v", got, err)
	}

	byMail, err := users.Search(ctx, got.Email, 20)
	if err != nil || len(byMail) != 1 || byMail[0].ID != uid {
		t.Fatalf("按邮箱搜索失败: %+v err=%v", byMail, err)
	}
	byName, err := users.Search(ctx, "测试用户", 20)
	if err != nil || len(byName) != 1 || byName[0].ID != uid {
		t.Fatalf("按昵称搜索失败: %+v err=%v", byName, err)
	}
	byID, err := users.Search(ctx, fmt.Sprint(uid), 20)
	if err != nil || len(byID) != 1 || byID[0].ID != uid {
		t.Fatalf("按 ID 搜索失败: %+v err=%v", byID, err)
	}
	if miss, err := users.Search(ctx, "绝不存在的关键词", 20); err != nil || len(miss) != 0 {
		t.Fatalf("无匹配应返回空: %+v err=%v", miss, err)
	}
}
