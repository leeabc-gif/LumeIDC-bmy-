package spaceship

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"lumeidc/internal/plugin"
)

// fakeRepo 内存假 repo：嵌 nil *Repo 以满足 repoStore 全量方法（轮询只触达
// ListPending/MarkSuccess/MarkFailed/UpdateDomain，其余不会被调用；
// WithTx 经嵌入提升且方法体不解引用 receiver，nil 亦安全）。
type fakeRepo struct {
	*Repo
	pending   []*OperationRow
	listCalls int
	listLimit int
	markSucc  []markRecord
	markFail  []failRecord
	patches   []patchRecord
}

type markRecord struct {
	id     int64
	result any
}

type failRecord struct {
	id     int64
	errMsg string
	result any
}

type patchRecord struct {
	id      int64
	patches map[string]any
}

func (f *fakeRepo) ListPending(ctx context.Context, limit int) ([]*OperationRow, error) {
	f.listCalls++
	f.listLimit = limit
	return f.pending, nil
}

func (f *fakeRepo) MarkSuccess(ctx context.Context, id int64, result any) error {
	f.markSucc = append(f.markSucc, markRecord{id: id, result: result})
	return nil
}

func (f *fakeRepo) MarkFailed(ctx context.Context, id int64, errMsg string, result any) error {
	f.markFail = append(f.markFail, failRecord{id: id, errMsg: errMsg, result: result})
	return nil
}

func (f *fakeRepo) UpdateDomain(ctx context.Context, id int64, patches map[string]any) error {
	f.patches = append(f.patches, patchRecord{id: id, patches: patches})
	return nil
}

// LatestPaidOrder 假实现：没有真实库，直接返回"无订单"，
// 让失败自动退款路径在内存测试里自然空转（不触碰 nil *Repo 的 db 句柄）。
func (f *fakeRepo) LatestPaidOrder(ctx context.Context, domainID int64, kind string) (*OrderRow, error) {
	return nil, nil
}

func (f *fakeRepo) MarkOrderRefunded(ctx context.Context, id int64) error { return nil }

// refundProbeRepo 在 fakeRepo 上叠加「退款是否被调用」的探针：
// refundByDomainOperation 内部先查 LatestPaidOrder（假实现返回 nil=nil），
// 因此用计数而不是余额断言来验证调用发生。
type refundProbeRepo struct {
	*fakeRepo
	refundCalls int
}

func (f *refundProbeRepo) LatestPaidOrder(ctx context.Context, domainID int64, kind string) (*OrderRow, error) {
	f.refundCalls++
	return nil, nil
}

// fakeNotify 记录站内通知与管理员告警，便于断言"告警是否被去重/分类"。
// alerts 每条为 (alertKey, subject)；calls 仍记录普通 Notify 以兼容老用例。
type fakeNotify struct {
	calls  []string
	alerts []alertRecord
}

type alertRecord struct {
	alertKey string
	category string
	subject  string
	body     string
}

func (n *fakeNotify) NotifyTemplate(ctx context.Context, userID int64, code, title, body string, values ...map[string]string) error {
	return nil
}
func (n *fakeNotify) Notify(ctx context.Context, userID int64, title, body string) error {
	n.calls = append(n.calls, title)
	return nil
}
func (n *fakeNotify) NotifyAdminOnce(ctx context.Context, alertKey, category, subject, body string) error {
	n.alerts = append(n.alerts, alertRecord{alertKey: alertKey, category: category, subject: subject, body: body})
	return nil
}

// newPollPlugin 构造挂假 repo + 假通知的插件（vals 覆盖 settings）。
func newPollPlugin(vals map[string]string, fr repoStore) (*Plugin, *fakeNotify) {
	nt := &fakeNotify{}
	h := (&plugin.Host{Settings: &fakeSettings{vals: vals}, Notify: nt}).ForPlugin(Name)
	return &Plugin{host: h, repo: fr}, nt
}

// opServer 把 Client.base 指向 httptest 假服务；fn 按请求写响应。
// 鉴权头缺失记为测试失败（do 必须注入 X-Api-Key/X-Api-Secret）。
func opServer(t *testing.T, fn func(w http.ResponseWriter, r *http.Request)) (*Client, *httptest.Server) {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "qa-key" || r.Header.Get("X-Api-Secret") != "qa-secret" {
			t.Errorf("Spaceship 鉴权头缺失: key=%q secret=%q", r.Header.Get("X-Api-Key"), r.Header.Get("X-Api-Secret"))
		}
		fn(w, r)
	}))
	c := NewClient("qa-key", "qa-secret")
	c.base = ts.URL
	t.Cleanup(ts.Close)
	return c, ts
}

// writeOp 写 JSON 响应体（状态码 + body）。
func writeOp(w http.ResponseWriter, code int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write([]byte(body))
}

// routeOpID 从 /async-operations/{id} 路径取 operationId（假服务按 id 分流）。
func routeOpID(r *http.Request) string {
	return strings.TrimPrefix(r.URL.Path, "/async-operations/")
}

// pollOperations：未配置 Key/Secret 时直接跳过——不得查询 pending、不得写任何状态
// （QA 环境无凭证时 cron 空转的核心语义）。
func TestPollOperationsNoCredentialsSkips(t *testing.T) {
	fr := &fakeRepo{pending: []*OperationRow{{ID: 1, OperationID: "op-1"}}}
	p, _ := newPollPlugin(nil, fr)
	if err := p.pollOperations(context.Background()); err != nil {
		t.Fatalf("无凭证应安静返回: %v", err)
	}
	if fr.listCalls != 0 || len(fr.markSucc) != 0 || len(fr.markFail) != 0 {
		t.Fatal("无凭证不得触达 repo")
	}
	// testPlugin 构造的 repo 为 nil，同样不得 panic（闸门早于 repo 访问）。
	if err := testPlugin(nil).pollOperations(context.Background()); err != nil {
		t.Fatalf("nil repo 亦应安全跳过: %v", err)
	}
}

// pollBatch：每轮取 pending 上限 10，逐条轮询；单条失败/限流不阻断其他。
// （pollOperations 仅负责凭据闸门 + 建客户端，循环语义在此验证。）
func TestPollBatchIterates(t *testing.T) {
	fr := &fakeRepo{pending: []*OperationRow{
		{ID: 1, OperationID: "op-429", OpType: "domain_create", DomainID: sql.NullInt64{Int64: 7, Valid: true}, StartedAt: time.Now()},
		{ID: 2, OperationID: "op-ok", OpType: "domain_create", DomainID: sql.NullInt64{Int64: 8, Valid: true}, StartedAt: time.Now()},
	}}
	p, _ := newPollPlugin(nil, fr)
	c, _ := opServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch routeOpID(r) {
		case "op-429":
			writeOp(w, http.StatusTooManyRequests, `{"errorCode":"RATE_LIMIT"}`)
		case "op-ok":
			writeOp(w, http.StatusOK, `{"operationId":"op-ok","status":"success","details":{"domainId":"sd-8"}}`)
		default:
			writeOp(w, http.StatusNotFound, `{"errorCode":"NOT_FOUND"}`)
		}
	})
	if err := p.pollBatch(context.Background(), c); err != nil {
		t.Fatalf("单条限流不应中断整轮: %v", err)
	}
	if fr.listCalls != 1 || fr.listLimit != 10 {
		t.Fatalf("应查询一次 pending 且上限 10: calls=%d limit=%d", fr.listCalls, fr.listLimit)
	}
	if len(fr.markSucc) != 1 || fr.markSucc[0].id != 2 {
		t.Fatalf("仅 op-ok 应标记成功: %+v", fr.markSucc)
	}
	if len(fr.markFail) != 0 {
		t.Fatalf("限流不应标记失败: %+v", fr.markFail)
	}
}

// pollOne success（domain_create）：MarkSuccess + 回补域名 active/SpaceshipID/时间 + 通知管理员。
func TestPollOneSuccessDomainCreate(t *testing.T) {
	fr := &fakeRepo{}
	p, nt := newPollPlugin(nil, fr)
	c, _ := opServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeOp(w, http.StatusOK, `{"operationId":"op-1","status":"success","details":{"domainId":"sd-42","createdAt":"2026-01-02T03:04:05Z","expiresAt":"2027-01-02T03:04:05Z"}}`)
	})
	op := &OperationRow{ID: 1, OperationID: "op-1", OpType: "domain_create",
		DomainID: sql.NullInt64{Int64: 7, Valid: true}, Domain: "qa.example", Status: "pending", StartedAt: time.Now()}
	if err := p.pollOne(context.Background(), c, op); err != nil {
		t.Fatalf("success 不应报错: %v", err)
	}
	if len(fr.markSucc) != 1 || fr.markSucc[0].id != 1 {
		t.Fatalf("应 MarkSuccess(1): %+v", fr.markSucc)
	}
	details, ok := fr.markSucc[0].result.(map[string]any)
	if !ok || details["domainId"] != "sd-42" {
		t.Fatalf("result 应透传 details: %+v", fr.markSucc[0].result)
	}
	if len(fr.patches) != 1 {
		t.Fatalf("应回补域名 1 次: %+v", fr.patches)
	}
	patch := fr.patches[0]
	if patch.id != 7 || patch.patches["status"] != "active" || patch.patches["spaceship_domain_id"] != "sd-42" {
		t.Fatalf("域名回补字段错误: %+v", patch)
	}
	if _, ok := patch.patches["registered_at"].(time.Time); !ok {
		t.Fatalf("registered_at 应为 time.Time: %T", patch.patches["registered_at"])
	}
	if _, ok := patch.patches["expires_at"].(time.Time); !ok {
		t.Fatalf("expires_at 应为 time.Time: %T", patch.patches["expires_at"])
	}
	if len(nt.calls) != 0 {
		t.Fatalf("成功不应再走 Notify(userID=0) 通道（应走 NotifyAdminOnce）: %+v", nt.calls)
	}
	if len(nt.alerts) != 1 || nt.alerts[0].subject != "域名注册成功" {
		t.Fatalf("成功应告警一次且 subject 正确: %+v", nt.alerts)
	}
	if nt.alerts[0].category != "spaceship-poll" {
		t.Fatalf("告警应归类到 spaceship-poll: %+v", nt.alerts[0])
	}
	if !strings.Contains(nt.alerts[0].alertKey, "op-1") {
		t.Fatalf("alertKey 应含 operationID: %q", nt.alerts[0].alertKey)
	}
}

// pollOne success（非 domain_create，如续费）：MarkSuccess 但不得回补域名注册信息；
// 告警走 NotifyAdminOnce，alertKey 用 OpType 区分以便按操作类型聚合。
func TestPollOneSuccessNonCreate(t *testing.T) {
	fr := &fakeRepo{}
	p, nt := newPollPlugin(nil, fr)
	c, _ := opServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeOp(w, http.StatusOK, `{"operationId":"op-r","status":"success","details":{"domainId":"sd-9"}}`)
	})
	op := &OperationRow{ID: 3, OperationID: "op-r", OpType: "domain_renew",
		DomainID: sql.NullInt64{Int64: 9, Valid: true}, Status: "pending", StartedAt: time.Now()}
	if err := p.pollOne(context.Background(), c, op); err != nil {
		t.Fatalf("success 不应报错: %v", err)
	}
	if len(fr.markSucc) != 1 || len(fr.patches) != 0 {
		t.Fatalf("非注册操作不得回补域名: succ=%+v patches=%+v", fr.markSucc, fr.patches)
	}
	if len(nt.alerts) != 1 || !strings.Contains(nt.alerts[0].alertKey, "domain_renew") {
		t.Fatalf("续费告警应走 NotifyAdminOnce 且 alertKey 含 OpType: %+v", nt.alerts)
	}
}

// pollOne failed：errMsg 回退链 o.Error → details.error → details.message → 固定兜底；
// 通知管理员（标题含失败原因）。
func TestPollOneFailedErrMsgChain(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"顶层 error 优先", `{"status":"failed","error":"top-level","details":{"error":"inner"}}`, "top-level"},
		{"details.error 回退", `{"status":"failed","details":{"error":"detail-err"}}`, "detail-err"},
		{"details.message 回退", `{"status":"failed","details":{"message":"detail-msg"}}`, "detail-msg"},
		{"空错误兜底文案", `{"status":"failed"}`, "Spaceship 返回失败"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fr := &fakeRepo{}
			p, nt := newPollPlugin(nil, fr)
			cl, _ := opServer(t, func(w http.ResponseWriter, r *http.Request) {
				writeOp(w, http.StatusOK, c.body)
			})
			op := &OperationRow{ID: 5, OperationID: "op-f", OpType: "domain_create",
				DomainID: sql.NullInt64{Int64: 7, Valid: true}, Domain: "qa.example", Status: "pending", StartedAt: time.Now()}
			if err := p.pollOne(context.Background(), cl, op); err != nil {
				t.Fatalf("failed 分支本身不应报错: %v", err)
			}
			if len(fr.markFail) != 1 || fr.markFail[0].errMsg != c.want {
				t.Fatalf("errMsg 回退链错误: want %q got %+v", c.want, fr.markFail)
			}
			if len(fr.markSucc) != 0 || len(fr.patches) != 0 {
				t.Fatal("failed 不得标记成功或回补域名")
			}
			if len(nt.calls) != 0 {
				t.Fatalf("失败不应再走 Notify(userID=0) 通道（应走 NotifyAdminOnce）: %+v", nt.calls)
			}
			if len(nt.alerts) != 1 || nt.alerts[0].subject != "域名注册失败" {
				t.Fatalf("失败应告警一次且 subject 正确: %+v", nt.alerts)
			}
			if !strings.Contains(nt.alerts[0].alertKey, c.want) && !strings.Contains(nt.alerts[0].body, c.want) {
				// 兜底：errMsg 可能只体现在 body 里（subject 是固定的"域名注册失败"）。
				t.Fatalf("告警 body 应含 errMsg=%q: %+v", c.want, nt.alerts[0])
			}
		})
	}
}

// pollOne pending：超 defStuckAfter 标记"需管理员对账"并告警；未超时不动状态。
func TestPollOnePendingTimeout(t *testing.T) {
	t.Run("超时标记失败", func(t *testing.T) {
		fr := &fakeRepo{}
		p, nt := newPollPlugin(nil, fr)
		c, _ := opServer(t, func(w http.ResponseWriter, r *http.Request) {
			writeOp(w, http.StatusOK, `{"operationId":"op-p","status":"pending"}`)
		})
		op := &OperationRow{ID: 6, OperationID: "op-p", OpType: "domain_create",
			DomainID: sql.NullInt64{Int64: 7, Valid: true}, Status: "pending",
			StartedAt: time.Now().Add(-16 * time.Minute)}
		if err := p.pollOne(context.Background(), c, op); err != nil {
			t.Fatalf("pending 不应报错: %v", err)
		}
		if len(fr.markFail) != 1 || fr.markFail[0].errMsg != "轮询超时（>15min），需管理员对账" {
			t.Fatalf("超时应标失败: %+v", fr.markFail)
		}
		if fr.markFail[0].result != nil {
			t.Fatalf("超时标记 result 应为 nil: %+v", fr.markFail[0].result)
		}
		if len(nt.alerts) != 1 || nt.alerts[0].subject != "异步操作超时" {
			t.Fatalf("超时应告警一次且 subject 正确: %+v", nt.alerts)
		}
		if !strings.Contains(nt.alerts[0].alertKey, "spaceship.poll.timeout") {
			t.Fatalf("超时 alertKey 应含 spaceship.poll.timeout: %q", nt.alerts[0].alertKey)
		}
	})
	t.Run("未超时不动", func(t *testing.T) {
		fr := &fakeRepo{}
		p, nt := newPollPlugin(nil, fr)
		c, _ := opServer(t, func(w http.ResponseWriter, r *http.Request) {
			writeOp(w, http.StatusOK, `{"operationId":"op-p","status":"pending"}`)
		})
		op := &OperationRow{ID: 7, OperationID: "op-p", OpType: "domain_create",
			DomainID: sql.NullInt64{Int64: 7, Valid: true}, Status: "pending",
			StartedAt: time.Now().Add(-14 * time.Minute)}
		if err := p.pollOne(context.Background(), c, op); err != nil {
			t.Fatalf("pending 不应报错: %v", err)
		}
		if len(fr.markFail) != 0 || len(fr.markSucc) != 0 {
			t.Fatal("未超时 pending 不得改状态")
		}
		if len(nt.alerts) != 0 {
			t.Fatalf("未超时 pending 不应告警: %+v", nt.alerts)
		}
	})
}

// pollOne pending 超时必须走退款路径（与 failed 同规则）：否则钱会长期悬挂在
// 「已扣款、状态不明」状态。fakeRepo 的 LatestPaidOrder 返回 nil（空转），
// 这里用计数断言 refundByDomainOperation 确实被调用。
func TestPollOnePendingTimeoutRefunds(t *testing.T) {
	fr := &refundProbeRepo{fakeRepo: &fakeRepo{}}
	p, nt := newPollPlugin(nil, fr)
	c, _ := opServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeOp(w, http.StatusOK, `{"operationId":"op-pr","status":"pending"}`)
	})
	op := &OperationRow{ID: 11, OperationID: "op-pr", OpType: "domain_create",
		DomainID: sql.NullInt64{Int64: 7, Valid: true}, Domain: "qa-pending.example",
		Status: "pending", StartedAt: time.Now().Add(-16 * time.Minute)}
	if err := p.pollOne(context.Background(), c, op); err != nil {
		t.Fatalf("pending 不应报错: %v", err)
	}
	if len(fr.markFail) != 1 {
		t.Fatalf("超时应标失败: %+v", fr.markFail)
	}
	if fr.refundCalls != 1 {
		t.Errorf("P1 资金悬挂：pending 超时未触发退款（refundCalls=%d）", fr.refundCalls)
	}
	if len(nt.alerts) != 1 || nt.alerts[0].subject != "异步操作超时" {
		t.Errorf("pending 超时应同时告警管理员: %+v", nt.alerts)
	}

	// refundOnFailure 关闭时不退款（人工处理）
	fr2 := &refundProbeRepo{fakeRepo: &fakeRepo{}}
	p2, nt2 := newPollPlugin(map[string]string{"plugin.spaceship.refundOnFailure": "0"}, fr2)
	c2, _ := opServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeOp(w, http.StatusOK, `{"operationId":"op-pr2","status":"pending"}`)
	})
	op2 := &OperationRow{ID: 12, OperationID: "op-pr2", OpType: "domain_create",
		DomainID: sql.NullInt64{Int64: 8, Valid: true}, Domain: "qa-pending2.example",
		Status: "pending", StartedAt: time.Now().Add(-16 * time.Minute)}
	if err := p2.pollOne(context.Background(), c2, op2); err != nil {
		t.Fatalf("pending 不应报错: %v", err)
	}
	if fr2.refundCalls != 0 {
		t.Errorf("refundOnFailure 关闭后仍退款: %d", fr2.refundCalls)
	}
	if len(fr2.markFail) != 1 {
		t.Errorf("refundOnFailure 关闭也应标记失败以便管理员对账: %+v", fr2.markFail)
	}
	// 关闭 refundOnFailure 仍要告警（管理员需要人工处理）
	if len(nt2.alerts) != 1 {
		t.Errorf("refundOnFailure 关闭后超时事件仍应告警: %+v", nt2.alerts)
	}
}

// pollOne 429 限流：跳过本轮（返回 nil），不改状态、等下一轮 cron。
func TestPollOneRateLimitedSkips(t *testing.T) {
	fr := &fakeRepo{}
	p, _ := newPollPlugin(nil, fr)
	c, _ := opServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeOp(w, http.StatusTooManyRequests, `{"errorCode":"RATE_LIMITED","message":"slow down"}`)
	})
	op := &OperationRow{ID: 8, OperationID: "op-429", OpType: "domain_create", Status: "pending", StartedAt: time.Now()}
	if err := p.pollOne(context.Background(), c, op); err != nil {
		t.Fatalf("限流应静默跳过: %v", err)
	}
	if len(fr.markSucc) != 0 || len(fr.markFail) != 0 || len(fr.patches) != 0 {
		t.Fatal("限流不得写任何状态")
	}
}

// pollOne 上游 5xx/4xx：错误向上传播（pollOperations 逐条 continue，不中断整轮）。
func TestPollOneUpstreamErrorPropagates(t *testing.T) {
	fr := &fakeRepo{}
	p, _ := newPollPlugin(nil, fr)
	c, _ := opServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeOp(w, http.StatusInternalServerError, `{"errorCode":"INTERNAL"}`)
	})
	op := &OperationRow{ID: 9, OperationID: "op-500", OpType: "domain_create", Status: "pending", StartedAt: time.Now()}
	err := p.pollOne(context.Background(), c, op)
	if err == nil {
		t.Fatal("上游 500 应传播错误")
	}
	if len(fr.markSucc) != 0 || len(fr.markFail) != 0 {
		t.Fatal("上游错误不得写状态")
	}
}

// enableNotify 关闭时：状态迁移照常，但不发任何通知/告警。
func TestPollOneNotifyDisabled(t *testing.T) {
	fr := &fakeRepo{}
	p, nt := newPollPlugin(map[string]string{"plugin.spaceship.enableNotify": "0"}, fr)
	c, _ := opServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeOp(w, http.StatusOK, `{"operationId":"op-n","status":"success","details":{"domainId":"sd-1"}}`)
	})
	op := &OperationRow{ID: 10, OperationID: "op-n", OpType: "domain_create",
		DomainID: sql.NullInt64{Int64: 7, Valid: true}, Status: "pending", StartedAt: time.Now()}
	if err := p.pollOne(context.Background(), c, op); err != nil {
		t.Fatalf("success 不应报错: %v", err)
	}
	if len(fr.markSucc) != 1 {
		t.Fatalf("关闭通知不影响状态迁移: %+v", fr.markSucc)
	}
	if len(nt.calls) != 0 || len(nt.alerts) != 0 {
		t.Fatalf("关闭通知后不得发送任何渠道: calls=%+v alerts=%+v", nt.calls, nt.alerts)
	}
}

// pollOne alertKey 设计意图：
//   - 同一 operationID 多次轮询 alertKey 一致（生产侧按 alertKey 去重）；
//   - 不同 operationID alertKey 不同（不互相覆盖）。
// 该测试在调用层断言 alertKey 与 operationID 的对应关系；真实去重由生产实现
// （admin_alert_log 表）保证，fakeNotify 自身不去重。
func TestPollOneAlertKeyUniqueness(t *testing.T) {
	fr := &fakeRepo{}
	p, nt := newPollPlugin(nil, fr)
	c, _ := opServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeOp(w, http.StatusOK, `{"operationId":"`+routeOpID(r)+`","status":"success","details":{"domainId":"sd-x"}}`)
	})
	// 同一 op 轮询两次（模拟 cron 多次抓到）
	op1 := &OperationRow{ID: 20, OperationID: "op-A", OpType: "domain_create",
		DomainID: sql.NullInt64{Int64: 1, Valid: true}, Domain: "a.example", Status: "pending", StartedAt: time.Now()}
	if err := p.pollOne(context.Background(), c, op1); err != nil {
		t.Fatalf("轮询1失败: %v", err)
	}
	if err := p.pollOne(context.Background(), c, op1); err != nil {
		t.Fatalf("轮询2失败: %v", err)
	}
	// fakeNotify 累计 2 条（自身不去重）；关键看 alertKey 一致 → 生产侧会去重
	if len(nt.alerts) != 2 {
		t.Fatalf("fakeNotify 累计调用应=2: %+v", nt.alerts)
	}
	if nt.alerts[0].alertKey != nt.alerts[1].alertKey {
		t.Fatalf("同一 op 两次轮询 alertKey 应相同，便于生产去重: %q vs %q",
			nt.alerts[0].alertKey, nt.alerts[1].alertKey)
	}
	// 不同 op → alertKey 不同
	op2 := &OperationRow{ID: 21, OperationID: "op-B", OpType: "domain_create",
		DomainID: sql.NullInt64{Int64: 2, Valid: true}, Domain: "b.example", Status: "pending", StartedAt: time.Now()}
	if err := p.pollOne(context.Background(), c, op2); err != nil {
		t.Fatalf("轮询op-B失败: %v", err)
	}
	if len(nt.alerts) != 3 {
		t.Fatalf("累计调用应=3: %+v", nt.alerts)
	}
	if nt.alerts[0].alertKey == nt.alerts[2].alertKey {
		t.Fatalf("不同 op 的 alertKey 不应相同: %q", nt.alerts[0].alertKey)
	}
	// 三条全部归到 spaceship-poll
	for i, a := range nt.alerts {
		if a.category != "spaceship-poll" {
			t.Fatalf("第 %d 条告警 category 应=spaceship-poll: %+v", i, a)
		}
	}
}
