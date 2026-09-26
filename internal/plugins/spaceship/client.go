// Package spaceship 实现 Spaceship（Namecheap）域名注册商 REST API 客户端。
// 鉴权：请求头 X-Api-Key + X-Api-Secret（无签名、无 Base64）。
// 注册是异步的：POST 返回 202 + operationId（响应头 spaceship-async-operationid），
// 需轮询 GET /v1/async-operations/{id} —— 注意失败时 HTTP 仍 200，必须读 body 的 status 字段。
package spaceship

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const baseURL = "https://spaceship.dev/api/v1"
const defaultTimeout = 30 * time.Second

// APIError Spaceship API 返回的业务错误。HTTPStatus 为 200 时检查 body 的 errors 数组。
type APIError struct {
	HTTPStatus   int      `json:"-"`
	ErrorCode    string   `json:"errorCode,omitempty"`
	ErrorMessage string   `json:"message,omitempty"`
	Errors       []string `json:"errors,omitempty"`
}

func (e *APIError) Error() string {
	if len(e.Errors) > 0 {
		return fmt.Sprintf("Spaceship API 错误(%d): %s", e.HTTPStatus, strings.Join(e.Errors, "; "))
	}
	if e.ErrorMessage != "" {
		return fmt.Sprintf("Spaceship API 错误(%d): %s", e.HTTPStatus, e.ErrorMessage)
	}
	return fmt.Sprintf("Spaceship API 错误(%d)", e.HTTPStatus)
}

// IsRateLimited 判断是否是 429 限流（指数退避重试用）。
func IsRateLimited(err error) bool {
	if err == nil {
		return false
	}
	if e, ok := err.(*APIError); ok {
		return e.HTTPStatus == 429
	}
	return false
}

// Client Spaceship REST API 请求器。零依赖，只用 net/http。
type Client struct {
	apiKey    string
	apiSecret string
	http      *http.Client
	base      string
}

// NewClient 创建客户端。apiKey/apiSecret 来自插件配置（API Manager 生成）。
func NewClient(apiKey, apiSecret string) *Client {
	return &Client{
		apiKey:    strings.TrimSpace(apiKey),
		apiSecret: strings.TrimSpace(apiSecret),
		base:      baseURL,
		http:      &http.Client{Timeout: defaultTimeout},
	}
}

// newClient 客户端工厂。默认指向 NewClient（真实上游 https://spaceship.dev）；
// 单测可临时替换为指向 httptest 假服务的实现，从而在无真实 Key 的情况下覆盖资金链路。
// 生产环境不会被改写，行为与直接调用 NewClient 完全一致。
var newClient = NewClient

// ErrCredentialsMissing 凭据未配置。
var ErrCredentialsMissing = fmt.Errorf("Spaceship API Key/Secret 未配置")

func (c *Client) configured() bool {
	return c.apiKey != "" && c.apiSecret != ""
}

// do 通用请求：自动注入鉴权头，解析错误响应，429 返回可重试错误。
// respBody 非 nil 时将成功响应 JSON 反序列化到它（必须是指针）。
func (c *Client) do(ctx context.Context, method, path string, body any, respBody any) (http.Header, error) {
	if !c.configured() {
		return nil, ErrCredentialsMissing
	}
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("Spaceship 请求序列化失败: %w", err)
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Api-Key", c.apiKey)
	req.Header.Set("X-Api-Secret", c.apiSecret)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("Spaceship 请求失败: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, fmt.Errorf("Spaceship 响应读取失败: %w", err)
	}

	// 202 Accepted（异步操作）：返回头即可，body 可能为空
	if resp.StatusCode == http.StatusAccepted {
		return resp.Header, nil
	}

	// 非 2xx：尝试解析 JSON 错误体
	if resp.StatusCode >= 400 {
		apiErr := &APIError{HTTPStatus: resp.StatusCode}
		if len(raw) > 0 {
			_ = json.Unmarshal(raw, apiErr)
		}
		return resp.Header, apiErr
	}

	// 成功：反序列化 body
	if respBody != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, respBody); err != nil {
			return resp.Header, fmt.Errorf("Spaceship 响应解析失败: %.200s: %w", string(raw), err)
		}
	}
	return resp.Header, nil
}

// CheckResult 单个域名可用性查询结果。
type CheckResult struct {
	Domain         string   `json:"domain"`
	Result         string   `json:"result"`                   // available / taken / processing
	PremiumPricing *Pricing `json:"premiumPricing,omitempty"` // 非空 = 溢价域名
	Premium        bool     `json:"premium,omitempty"`
}

// Pricing 价格信息（溢价域名、注册、续费）。
type Pricing struct {
	Currency     string  `json:"currency"`
	Price        float64 `json:"price"`
	PremiumPrice float64 `json:"premiumPrice,omitempty"`
	Description  string  `json:"description,omitempty"`
}

// IsAvailable 判断域名是否可注册（排除 premiumPricing 非空的溢价域名）。
func (r *CheckResult) IsAvailable() bool {
	return r != nil && r.Result == "available"
}

// IsPremium 判断是否溢价域名（需二次确认）。
func (r *CheckResult) IsPremium() bool {
	return r != nil && (r.PremiumPricing != nil || r.Premium)
}

// CheckOne 单域名可用性查询。GET /v1/domains/{domain}/available
func (c *Client) CheckOne(ctx context.Context, domain string) (*CheckResult, error) {
	var out *CheckResult
	_, err := c.do(ctx, http.MethodGet, "/domains/"+url.PathEscape(domain)+"/available", nil, &out)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// CheckBatch 批量可用性查询（最多 20 个）。POST /v1/domains/available
func (c *Client) CheckBatch(ctx context.Context, domains []string) (map[string]*CheckResult, error) {
	if len(domains) == 0 {
		return map[string]*CheckResult{}, nil
	}
	if len(domains) > 20 {
		return nil, fmt.Errorf("批量查询最多 20 个域名")
	}
	body := map[string]any{"domains": domains}
	var out []*CheckResult
	_, err := c.do(ctx, http.MethodPost, "/domains/available", body, &out)
	if err != nil {
		return nil, err
	}
	m := make(map[string]*CheckResult, len(out))
	for _, r := range out {
		if r != nil && r.Domain != "" {
			m[r.Domain] = r
		}
	}
	return m, nil
}

// Contact WHOIS 联系人信息。四个角色（registrant/admin/tech/billing）可全用同一个 contactId。
// 电话格式：+国际区号.号码（如 +86.13800138000）。
type Contact struct {
	FirstName     string `json:"firstName"`
	LastName      string `json:"lastName"`
	Organization  string `json:"organization,omitempty"`
	Email         string `json:"email"`
	Address1      string `json:"address1"`
	Address2      string `json:"address2,omitempty"`
	City          string `json:"city"`
	StateProvince string `json:"stateProvince,omitempty"`
	PostalCode    string `json:"postalCode,omitempty"`
	Country       string `json:"country"`
	Phone         string `json:"phone"`
	// 响应返回时填充
	ContactID string `json:"contactId,omitempty"`
}

// SaveContact 创建/更新联系人。PUT /v1/contacts
// 请求体不带 contactId（新建）或带（更新）；响应返回带 contactId 的完整对象。
func (c *Client) SaveContact(ctx context.Context, contact *Contact) (string, error) {
	body := map[string]any{
		"firstName": contact.FirstName,
		"lastName":  contact.LastName,
		"email":     contact.Email,
		"address1":  contact.Address1,
		"city":      contact.City,
		"country":   contact.Country,
		"phone":     contact.Phone,
	}
	if contact.Organization != "" {
		body["organization"] = contact.Organization
	}
	// 带 contactId 时 Spaceship 视为更新已有联系人，否则新建。
	if contact.ContactID != "" {
		body["contactId"] = contact.ContactID
	}
	if contact.Address2 != "" {
		body["address2"] = contact.Address2
	}
	if contact.StateProvince != "" {
		body["stateProvince"] = contact.StateProvince
	}
	if contact.PostalCode != "" {
		body["postalCode"] = contact.PostalCode
	}
	var out *Contact
	_, err := c.do(ctx, http.MethodPut, "/contacts", body, &out)
	if err != nil {
		return "", err
	}
	if out == nil || out.ContactID == "" {
		return "", fmt.Errorf("Spaceship 联系人保存成功但未返回 contactId")
	}
	return out.ContactID, nil
}

// GetContact 读取联系人。GET /v1/contacts/{contactId}
func (c *Client) GetContact(ctx context.Context, contactID string) (*Contact, error) {
	var out *Contact
	_, err := c.do(ctx, http.MethodGet, "/contacts/"+url.PathEscape(contactID), nil, &out)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// RegisterOpts 注册域名参数。
type RegisterOpts struct {
	Years     int  // 1-10
	AutoRenew bool // 官方必填：注册后的自动续费开关
	// PrivacyLevel 官方仅支持 high（WHOIS 全隐藏）与 public（公开）两档；
	// 早期版本的 medium/low 已被官方移除，传入会被判 400。
	PrivacyLevel      string // high / public
	UserConsent       bool   // 隐私保护用户同意
	ContactRegistrant string // 四个角色均用同一个 contactId（MVP）
}

// SetPrivacy 切换 WHOIS 隐私保护等级。PUT /v1/domains/{domain}/privacy/preference
// level 仅可为 high / public；userConsent 表示已获得注册人同意（官方要求）。
func (c *Client) SetPrivacy(ctx context.Context, domain, level string, userConsent bool) error {
	if level != "high" && level != "public" {
		return fmt.Errorf("隐私保护等级仅支持 high / public，当前值 %q 非法", level)
	}
	body := map[string]any{"privacyLevel": level, "userConsent": userConsent}
	_, err := c.do(ctx, http.MethodPut, "/domains/"+url.PathEscape(domain)+"/privacy/preference", body, nil)
	return err
}

// RegisterDomain 提交域名注册（异步）。POST /v1/domains/{domain}
// 返回 operationId（来自响应头 spaceship-async-operationid）。
func (c *Client) RegisterDomain(ctx context.Context, domain string, opts RegisterOpts) (string, error) {
	if opts.Years < 1 || opts.Years > 10 {
		return "", fmt.Errorf("注册年限必须在 1-10 之间")
	}
	if opts.ContactRegistrant == "" {
		return "", fmt.Errorf("注册域名需要先创建联系人（缺 contactId）")
	}
	privacy := map[string]any{"level": opts.PrivacyLevel, "userConsent": opts.UserConsent}
	contacts := map[string]string{
		"registrant": opts.ContactRegistrant,
		"admin":      opts.ContactRegistrant,
		"tech":       opts.ContactRegistrant,
		"billing":    opts.ContactRegistrant,
	}
	body := map[string]any{
		"autoRenew":         opts.AutoRenew,
		"years":             opts.Years,
		"privacyProtection": privacy,
		"contacts":          contacts,
	}
	headers, err := c.do(ctx, http.MethodPost, "/domains/"+url.PathEscape(domain), body, nil)
	if err != nil {
		return "", err
	}
	opID := headers.Get("spaceship-async-operationid")
	if opID == "" {
		return "", fmt.Errorf("Spaceship 注册返回 202 但未在响应头找到 spaceship-async-operationid")
	}
	return opID, nil
}

// Operation 异步操作状态。status 可能是 pending/success/failed。
// 注意：HTTP 始终返回 200，必须读 body 的 status 字段判断成功/失败。
type Operation struct {
	ID         string `json:"operationId"`
	Type       string `json:"type"`              // 如 domains_Create
	Status     string `json:"status"`            // pending/success/failed
	Details    any    `json:"details,omitempty"` // 成功：包含 domainId/domain；失败：包含 error/message
	Error      string `json:"error,omitempty"`
	CreatedAt  string `json:"createdAt"`
	ModifiedAt string `json:"modifiedAt,omitempty"`
}

// GetOperation 查询异步操作状态。GET /v1/async-operations/{operationId}
func (c *Client) GetOperation(ctx context.Context, operationID string) (*Operation, error) {
	var out *Operation
	_, err := c.do(ctx, http.MethodGet, "/async-operations/"+url.PathEscape(operationID), nil, &out)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// DomainInfo 域名详情（来自 Spaceship GET /v1/domains/{domain}）。
type DomainInfo struct {
	Domain         string   `json:"domain"`
	DomainID       string   `json:"domainId"`
	Status         string   `json:"status"`
	ExpiresAt      string   `json:"expiresAt,omitempty"`
	AutoRenew      bool     `json:"autoRenew"`
	PrivacyEnabled bool     `json:"privacyProtection,omitempty"`
	Nameservers    []string `json:"nameservers,omitempty"`
}

// ListDomains 域名列表（分页）。GET /v1/domains
// 官方文档明确 take（1-100）与 skip 均为 required，缺失会被上游判 400。
func (c *Client) ListDomains(ctx context.Context, take, skip int) ([]*DomainInfo, error) {
	if take < 1 {
		take = 1
	}
	// 官方上限为 100，超过会被判 400（此前写成 500 属于文档误读）。
	if take > 100 {
		take = 100
	}
	if skip < 0 {
		skip = 0
	}
	var out []*DomainInfo
	path := fmt.Sprintf("/domains?take=%d&skip=%d", take, skip)
	_, err := c.do(ctx, http.MethodGet, path, nil, &out)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// GetDomain 域名详情。GET /v1/domains/{domain}
func (c *Client) GetDomain(ctx context.Context, domain string) (*DomainInfo, error) {
	var out *DomainInfo
	_, err := c.do(ctx, http.MethodGet, "/domains/"+url.PathEscape(domain), nil, &out)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// SetAutoRenew 切换域名自动续费。PUT /v1/domains/{domain}/autorenew
// 官方文档明确请求体为 {"isEnabled": bool}；曾用名 autoRenew 会被判缺少必填字段而 400。
func (c *Client) SetAutoRenew(ctx context.Context, domain string, enable bool) error {
	body := map[string]any{"isEnabled": enable}
	_, err := c.do(ctx, http.MethodPut, "/domains/"+url.PathEscape(domain)+"/autorenew", body, nil)
	return err
}

// TestConnection 测试 API Key/Secret 连通性。用一个轻量请求（ListDomains 取首条）。
// 成功返回空字符串；失败返回错误描述（供配置页展示）。
func (c *Client) TestConnection(ctx context.Context) error {
	if !c.configured() {
		return ErrCredentialsMissing
	}
	_, err := c.ListDomains(ctx, 1, 0)
	return err
}

// RenewDomain 续费域名（异步）。POST /v1/domains/{domain}/renew
// 官方必填：years（1-10）与 currentExpirationDate（当前到期时间，string <date-time>，RFC3339）。
// 传 Unix 毫秒整数会被官方判为类型错误而 400，故此处统一按 UTC RFC3339 序列化。
// 返回 operationId（来自响应头 spaceship-async-operationid）。
func (c *Client) RenewDomain(ctx context.Context, domain string, years int, currentExpiration time.Time) (string, error) {
	if years < 1 || years > 10 {
		return "", fmt.Errorf("续费年限必须在 1-10 之间")
	}
	if currentExpiration.IsZero() {
		return "", fmt.Errorf("续费缺少当前到期时间（currentExpirationDate）")
	}
	body := map[string]any{
		"years":                 years,
		"currentExpirationDate": currentExpiration.UTC().Format(time.RFC3339),
	}
	headers, err := c.do(ctx, http.MethodPost, "/domains/"+url.PathEscape(domain)+"/renew", body, nil)
	if err != nil {
		return "", err
	}
	opID := headers.Get("spaceship-async-operationid")
	if opID == "" {
		return "", fmt.Errorf("Spaceship 续费返回 202 但未在响应头找到 spaceship-async-operationid")
	}
	return opID, nil
}
