package handler

import (
	"database/sql"
	"net/http"
	"strconv"
	"time"

	"lumeidc/internal/middleware"
	"lumeidc/internal/repo"
	"lumeidc/internal/server"
)

func adminRequire(w http.ResponseWriter, r *http.Request) bool {
	sess := middleware.FromSession(r.Context())
	if sess != nil && sess.IsAdmin {
		return true
	}
	// 浏览器导航跳后台登录页；SPA（Accept: application/json）返回 401 JSON。
	if wantsJSON(r) {
		// 先设 Content-Type 再写状态码：WriteHeader 之后 header 即提交，
		// 此时再 Set("Content-Type") 无效，Go 会按正文嗅探成 text/plain，
		// 前端按 Content-Type 解析就会把 JSON 体当纯文本处理。
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		writeJSON(w, map[string]any{"ok": 0, "msg": "登录已过期，请重新登录"})
		return false
	}
	http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
	return false
}

const requestTimeout = 15 * time.Second

func serverConfig(sv *repo.Server) server.Config {
	return server.Config{APIURL: sv.APIURL, APIUsername: sv.APIUsername, APIKey: sv.APIKey, CredentialRevision: sv.CredentialRevision}
}

func sqlNull(id int64) sql.NullInt64 {
	return sql.NullInt64{Int64: id, Valid: id > 0}
}

func money(v float64) string {
	return strconv.FormatFloat(v, 'f', 2, 64)
}
