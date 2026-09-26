package handler

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"math"
	"net/http"
	"strconv"
	"strings"

	"lumeidc/internal/server"
	"lumeidc/internal/service"
)

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(v)
}

// consoleErrMsg 把用户侧实例操作（控制台/续费/升级）的上游故障收敛成可展示文案。
//
// 上游 provider 的错误里带内网地址与已签名 URL：`*url.Error` 会打印完整请求 URL
// （如 `/api/xx 请求失败: Get "https://panel.example.com/api/xx": dial tcp 10.0.0.5 ...`），
// 直接 jsonFail(err.Error()) 等于把上游面板地址下发给普通用户，与本项目
// 「上游地址/令牌不下发浏览器」的设计目标（见 user_vnc.go 顶部说明）直接冲突。
//
// 判据不能只看「是不是我们写的错误类型」——本地业务错误（"商品已售罄"、"服务正在处理中"、
// "未选择操作系统"）也必须原样告诉用户，否则用户不知道该怎么处理。这里用**是否包装了底层原因**
// 区分两类错误：
//   - 带 `%w`（errors.Unwrap 非 nil）：消息里混了上游/网络/DB 细节 → 只记日志、对外收敛；
//   - 无包装：本仓自己编写的业务文案 → 原样展示（这些文案不含地址/密钥）。
//
// 新增「可展示」的错误时，优先在 service 层定义哨兵并加进下面的白名单。
func consoleErrMsg(err error) string {
	if err == nil {
		return ""
	}
	switch {
	case errors.Is(err, server.ErrNotSupported), // 该上游不支持此操作
		errors.Is(err, service.ErrNoUpstream),           // 该服务未绑定上游
		errors.Is(err, service.ErrServiceUnavailable),   // 服务不存在或不可操作
		errors.Is(err, service.ErrUpstreamPriceChanged): // 商品价格已更新，请重新确认
		return err.Error()
	}
	if errors.Unwrap(err) == nil {
		return err.Error()
	}
	log.Printf("[console] 上游操作失败: %v", err)
	return "操作失败，请稍后重试"
}

// jsonOK 输出 {ok:1, key:value} 成功响应（前端以 ok===1 判断）。
func jsonOK(w http.ResponseWriter, key string, v any) {
	writeJSON(w, map[string]any{"ok": 1, key: v})
}

// jsonFail 输出 {ok:0, msg:...} 失败响应（前端以 ok===0 判断）。
func jsonFail(w http.ResponseWriter, msg string) {
	writeJSON(w, map[string]any{"ok": 0, "msg": msg})
}

// jsonStatus 兼容“发送验证码/结构错误”这类既有 http.Error(status) 语义：
// SSR（浏览器导航）原样 http.Error；SPA（Accept: application/json）输出
// {ok: 状态<400} JSON，保留原 HTTP 状态码（如发送验证码 202）。
func jsonStatus(w http.ResponseWriter, r *http.Request, status int, msg string) {
	if !wantsJSON(r) {
		http.Error(w, msg, status)
		return
	}
	// 同上：Content-Type 必须在 WriteHeader 之前设置，否则被嗅探成 text/plain。
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	writeJSON(w, map[string]any{"ok": status < 400, "msg": msg})
}

// wantsJSON 内容协商：客户端明确请求 application/json 且不接受 text/html 时返回 true。
// SPA 的 fetch 带 Accept: application/json（详见 web/src/http），浏览器导航仍取 HTML（SSR）。
func wantsJSON(r *http.Request) bool {
	accept := r.Header.Get("Accept")
	if accept == "" {
		return false
	}
	return strings.Contains(accept, "application/json") && !strings.Contains(accept, "text/html")
}

// parsePostForm 解析表单/multipart 请求体（URL 编码解析失败时回退 multipart）。
func parsePostForm(r *http.Request) error {
	if err := r.ParseForm(); err != nil {
		if mperr := r.ParseMultipartForm(12 << 20); mperr != nil {
			return err
		}
	}
	return nil
}

// jsonFloatString JSON 数字标量转字符串：整值去掉小数点，其余保留原精度。
func jsonFloatString(f float64) string {
	if f == math.Trunc(f) {
		return strconv.FormatInt(int64(f), 10)
	}
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// bodyValuesMulti 与 bodyValues 类似，但保留多值字段（如勾选列表 import[]）。
// JSON 数组 → 多个值；表单 → PostForm 的全部值。SPA 提交勾选列表时使用。
func bodyValuesMulti(r *http.Request) (map[string][]string, error) {
	if !strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "application/json") {
		if err := parsePostForm(r); err != nil {
			return nil, err
		}
		return r.PostForm, nil
	}
	var raw map[string]any
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(&raw); err != nil {
		return nil, err
	}
	out := make(map[string][]string, len(raw))
	for k, v := range raw {
		switch t := v.(type) {
		case string:
			out[k] = []string{t}
		case bool:
			out[k] = []string{strconv.FormatBool(t)}
		case float64:
			out[k] = []string{jsonFloatString(t)}
		case []any:
			vals := make([]string, 0, len(t))
			for _, item := range t {
				switch it := item.(type) {
				case string:
					vals = append(vals, it)
				case float64:
					vals = append(vals, jsonFloatString(it))
				}
			}
			out[k] = vals
		}
	}
	return out, nil
}

// bodyValues 把请求体统一到 map[string]string，字段名与 SSR 表单契约一致：
// - application/json：解析 JSON 对象（SPA 提交），标量类型转字符串；
// - 其余（表单/multipart）：ParseForm/ParseMultipartForm 后取 PostForm 首值。
// 各 handler 调用一次，向后续逻辑传参，避免重复消费请求体。
func bodyValues(r *http.Request) (map[string]string, error) {
	if !strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "application/json") {
		if err := parsePostForm(r); err != nil {
			return nil, err
		}
		out := make(map[string]string, len(r.PostForm))
		for k, vs := range r.PostForm {
			if len(vs) > 0 {
				out[k] = vs[0]
			}
		}
		return out, nil
	}
	var raw map[string]any
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(&raw); err != nil {
		return nil, err
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		switch t := v.(type) {
		case string:
			out[k] = t
		case float64:
			out[k] = jsonFloatString(t)
		case bool:
			out[k] = strconv.FormatBool(t)
		}
	}
	return out, nil
}
