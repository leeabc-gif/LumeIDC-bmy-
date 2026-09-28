package handler

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"lumeidc/internal/plugin"
	"lumeidc/internal/repo"
)

// AdminPluginConfig 插件配置 schema 的统一读写 API（框架自动提供，插件零 handler）。
// 由组装根对实现 plugin.ConfigSchemaProvider 的插件挂到 /admin/plugin/{name}/ 子 mux：
//
//	GET  /config → {ok, schema, values, has}
//	POST /config ← {"values": {key: value}}
//
// 存储约定：settings 表键 plugin.{name}.{key}；password 不回显（GET 给 has_<key>，
// POST 留空沿用）；multiselect 存 JSON 数组字符串；switch 存 "1"/"0"。
type AdminPluginConfig struct {
	Settings *repo.Settings
	AdminLog *repo.AdminLog
	Name     string
	Schema   []plugin.ConfigField
}

func (h *AdminPluginConfig) settingKey(field string) string {
	return "plugin." + h.Name + "." + field
}

// Get 返回 schema（动态选项已填充）+ 当前值 + password 已配置标记。
func (h *AdminPluginConfig) Get(w http.ResponseWriter, r *http.Request) {
	if !adminRequire(w, r) {
		return
	}
	ctx := r.Context()
	values := map[string]string{}
	has := map[string]bool{}
	schema := make([]plugin.ConfigField, len(h.Schema))
	copy(schema, h.Schema)
	for i, f := range schema {
		// 动态选项引用：plugin.events → 事件目录
		if f.OptionsRef == "plugin.events" {
			for _, e := range plugin.EventCatalog() {
				schema[i].Options = append(schema[i].Options, plugin.ConfigOption{Value: e.Name, Label: e.Label})
			}
		}
		v, err := h.Settings.Get(ctx, h.settingKey(f.Key))
		if f.Type == "password" {
			has["has_"+f.Key] = err == nil && strings.TrimSpace(v) != ""
			continue
		}
		if err != nil {
			v = f.Default
		}
		values[f.Key] = v
	}
	writeJSON(w, map[string]any{"ok": 1, "schema": schema, "values": values, "has": has})
}

// Save 按 schema 校验并保存。未知键忽略；校验失败整体不落库。
func (h *AdminPluginConfig) Save(w http.ResponseWriter, r *http.Request) {
	if !adminRequire(w, r) {
		return
	}
	var req struct {
		Values map[string]json.RawMessage `json:"values"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, map[string]any{"ok": 0, "msg": "请求格式错误"})
		return
	}
	// 先全部解析校验，再统一写库（避免半保存状态）。
	pending := map[string]string{}
	for _, f := range h.Schema {
		raw, present := req.Values[f.Key]
		if !present {
			continue
		}
		v, skip, errMsg := h.validateField(f, raw)
		if errMsg != "" {
			writeJSON(w, map[string]any{"ok": 0, "msg": f.Title + "：" + errMsg})
			return
		}
		if skip {
			continue // password 留空沿用旧值
		}
		pending[h.settingKey(f.Key)] = v
	}
	ctx := r.Context()
	for k, v := range pending {
		if err := h.Settings.Set(ctx, k, v); err != nil {
			writeJSON(w, map[string]any{"ok": 0, "msg": "保存失败，请稍后重试"})
			return
		}
	}
	if len(pending) > 0 {
		keys := make([]string, 0, len(pending))
		for _, f := range h.Schema {
			if _, ok := pending[h.settingKey(f.Key)]; ok {
				keys = append(keys, f.Key)
			}
		}
		sort.Strings(keys)
		// 只记键名不记值（可能含密钥）。
		auditPluginOp(h.AdminLog, r, "plugin_config", "插件 "+h.Name+" 配置保存: "+strings.Join(keys, ","))
	}
	writeJSON(w, map[string]any{"ok": 1})
}

// validateField 按字段类型把 JSON 原始值规整为存储字符串。
// 返回 (存储值, 是否跳过写入, 校验错误文案)。
func (h *AdminPluginConfig) validateField(f plugin.ConfigField, raw json.RawMessage) (string, bool, string) {
	switch f.Type {
	case "switch":
		var b bool
		if err := json.Unmarshal(raw, &b); err != nil {
			return "", false, "必须是开关值"
		}
		if b {
			return "1", false, ""
		}
		return "0", false, ""
	case "number":
		var n float64
		if err := json.Unmarshal(raw, &n); err != nil {
			return "", false, "必须是数字"
		}
		return strconv.FormatFloat(n, 'f', -1, 64), false, ""
	case "select":
		var v string
		if err := json.Unmarshal(raw, &v); err != nil {
			return "", false, "格式错误"
		}
		if v == "" {
			return "", false, "" // 清空允许
		}
		// 与 multiselect 一致：静态选项 + OptionsRef 动态选项都合法。
		if h.optionsOf(f)[v] {
			return v, false, ""
		}
		return "", false, "选项不在允许范围内"
	case "multiselect":
		var arr []string
		if err := json.Unmarshal(raw, &arr); err != nil {
			return "", false, "格式错误"
		}
		allowed := h.optionsOf(f)
		out := make([]string, 0, len(arr))
		for _, v := range arr {
			if _, ok := allowed[v]; ok {
				out = append(out, v)
			}
		}
		b, _ := json.Marshal(out)
		return string(b), false, ""
	case "password":
		var v string
		if err := json.Unmarshal(raw, &v); err != nil {
			return "", false, "格式错误"
		}
		if strings.TrimSpace(v) == "" {
			return "", true, "" // 留空沿用
		}
		return strings.TrimSpace(v), false, ""
	default: // text/textarea
		var v string
		if err := json.Unmarshal(raw, &v); err != nil {
			return "", false, "格式错误"
		}
		return strings.TrimSpace(v), false, ""
	}
}

// optionsOf 汇总字段的可选值集合（静态 + OptionsRef 动态）。
func (h *AdminPluginConfig) optionsOf(f plugin.ConfigField) map[string]bool {
	allowed := map[string]bool{}
	for _, opt := range f.Options {
		allowed[opt.Value] = true
	}
	if f.OptionsRef == "plugin.events" {
		for _, e := range plugin.EventCatalog() {
			allowed[e.Name] = true
		}
	}
	return allowed
}
