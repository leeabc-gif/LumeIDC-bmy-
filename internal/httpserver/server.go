package httpserver

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"lumeidc/internal/captcha"
	"lumeidc/internal/config"
	"lumeidc/internal/cron"
	"lumeidc/internal/crypto"
	"lumeidc/internal/db"
	"lumeidc/internal/gateway"
	"lumeidc/internal/handler"
	"lumeidc/internal/middleware"
	"lumeidc/internal/plugin"
	_ "lumeidc/internal/plugins/all" // 扩展聚合：业务插件 + 内置供应商（init 自注册）
	"lumeidc/internal/repo"
	"lumeidc/internal/server"
	"lumeidc/internal/service"
	"lumeidc/internal/storage"
	"lumeidc/internal/update"

	robfigcron "github.com/robfig/cron/v3"
)

// App 持有所有需要在优雅关闭时释放的资源。
type App struct {
	mu         sync.Mutex
	Server     *http.Server
	DB         *sql.DB
	Cron       *robfigcron.Cron
	Notifier   *service.Notifier
	listenAddr string        // 当前实际监听地址（启动或热切换后），供同址幂等比较
	stopped    chan struct{} // 第一次关闭后关闭，供 SwitchListen 热替换后等待新服务
	stopOnce   sync.Once
}

// Shutdown 限时停止邮件领取和发送，再关闭数据库；未完成邮件保留到下次启动。
func (a *App) Shutdown(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var cronDone context.Context
	if a.Cron != nil {
		cronDone = a.Cron.Stop()
	}
	if err := a.Notifier.StopSMS(ctx); err != nil {
		log.Print("等待短信服务停止超时，未确认任务不会自动重发")
	}
	if err := a.Notifier.StopMail(ctx); err != nil {
		log.Print("等待邮件服务停止超时，未完成任务将由租约回收")
	}
	a.mu.Lock()
	srv := a.Server
	a.mu.Unlock()
	if srv != nil {
		if err := srv.Shutdown(ctx); err != nil {
			_ = srv.Close()
		}
	}
	if cronDone != nil {
		select {
		case <-cronDone.Done():
		case <-ctx.Done():
		}
	}
	if a.DB != nil {
		a.DB.Close()
	}
	a.stopOnce.Do(func() { close(a.stopped) }) // 让等待方退出（作为退出信号）
}

// Stopped 返回一个 chan，当实际响应的服务（SwitchListen 替换后的最终服务）退出时关闭。
func (a *App) Stopped() <-chan struct{} { return a.stopped }

// SwitchListen 热切换监听地址（后台修改端口用）：先绑定新端口，成功后
// 替换当前 HTTP 服务并优雅关闭旧服务；绑定失败时旧服务不受影响。
// 目标与当前监听一致时幂等直接成功（后台留空回退 config 端口时避免"自己绑自己"EADDRINUSE）。
// ponytail: 绑定+替换在锁内串行完成，消除并发切换时"两调用各自读到旧 Server、
// 先创建的实例成为孤儿（监听泄漏）"的窗口；net.Listen 耗时为微秒级，锁内可接受。
func (a *App) SwitchListen(addr string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if sameListen(a.listenAddr, addr) {
		return nil
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("绑定新端口 %s 失败: %w", addr, err)
	}
	old := a.Server
	newSrv := &http.Server{
		Handler:           old.Handler,
		ReadHeaderTimeout: old.ReadHeaderTimeout,
		ReadTimeout:       old.ReadTimeout,
		WriteTimeout:      old.WriteTimeout,
		IdleTimeout:       old.IdleTimeout,
	}
	a.Server = newSrv
	a.listenAddr = addr
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = old.Shutdown(ctx) // 等待旧端口上的存量请求完成
	}()
	go func() {
		if err := newSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("监听 %s 异常退出: %v", addr, err)
		}
		a.stopOnce.Do(func() { close(a.stopped) })
	}()
	return nil
}

// version 为构建期注入的版本号（main.version），未注入时为 dev。
func Build(cfg *config.Config, version string) (*App, error) {
	// 前端构建产物为强制依赖（SPA 已接管前台/后台页面，SSR 仅保留少量流程）。
	if !handler.WebUIBuilt() {
		return nil, errors.New("前端构建产物缺失（internal/handler/webui/dist）：请先执行 cd web && npm run build")
	}
	database, err := db.Open(cfg.DBDSN)
	if err != nil {
		return nil, err
	}
	// 启动时应用未执行的迁移，保证升级平滑
	if err := db.Migrate(context.Background(), database, db.Migrations()); err != nil {
		return nil, err
	}
	// 插件启用态入库（plugins 表由核心迁移 079 建立；读不到时静默全部启用）
	_ = plugin.InitEnabledStore(database)
	// 插件迁移：先于 Init 建表（version 键以 plugin/{name}/ 前缀隔离核心序列）
	for _, pl := range plugin.All() {
		if m, ok := pl.(plugin.Migrator); ok {
			if err := db.MigratePrefixed(context.Background(), database, m.Migrations(), "plugin/"+pl.Info().Name); err != nil {
				database.Close()
				return nil, fmt.Errorf("插件 %s 迁移失败: %w", pl.Info().Name, err)
			}
		}
	}
	store, err := middleware.NewStore(cfg)
	if err != nil {
		return nil, err
	}
	// ---- 渲染依赖（会话双 Store + 站点品牌/余额），由各 handler 匿名内嵌 ----
	deps := &handler.Deps{PageStore: store, AdminStore: store}

	// 供应商注册表：各供应商包 init() 自注册（新增见 server/all/all.go 与 docs/provider.md）。
	providers := server.DefaultRegistry
	// 实例密码加密器（services.password_crypt），密钥与 session 同源
	cryptor, cerr := crypto.New(cfg.SecretKey)
	if cerr != nil {
		database.Close()
		return nil, fmt.Errorf("初始化密码加密器失败: %w", cerr)
	}

	// ---- 仓库：每表一个实例，全图共享 ----
	users := repo.NewUsers(database)
	products := repo.NewProducts(database)
	serversRepo := repo.NewServers(database)
	balanceRepo := repo.NewBalance(database)
	coupons := repo.NewCoupons(database)
	announcements := repo.NewAnnouncements(database)
	adminLog := repo.NewAdminLog(database)
	refunds := repo.NewRefunds(database)
	statsRepo := repo.NewStats(database)
	gatewaysRepo := repo.NewGateways(database)
	loginAttempts := repo.NewLoginAttempts(database)
	invoices := repo.NewInvoices(database)
	promotionsRepo := repo.NewPromotions(database)
	provisions := repo.NewProvisionRepo(database)
	jobs := repo.NewFulfillmentJobs(database)
	periodGrants := repo.NewPeriodGrants(database)
	admins := repo.NewAdmins(database)
	identityStore := repo.NewIdentityStore(database)
	authChallenges := repo.NewAuthChallenges(database)
	settingsRepo := repo.NewSettings(database)
	cancelReqs := repo.NewCancelRequests(database)

	deps.Balance = balanceRepo
	deps.Settings = settingsRepo

	// 自定义后台访问路径（站点设置 admin_path；运行期可改，保存即生效）
	adminPathCfg := middleware.NewAdminPathConfig("")
	if raw, err := settingsRepo.Get(context.Background(), service.KeyAdminPath); err == nil {
		adminPathCfg.Set(strings.TrimSpace(raw))
	}
	// 监听端口（站点设置 listen_port；运行期可改，保存即生效）
	defaultListen := cfg.Listen
	if rawPort, err := settingsRepo.Get(context.Background(), service.KeyListenPort); err == nil {
		if p := strings.TrimSpace(rawPort); p != "" {
			if n, err := strconv.Atoi(p); err == nil && n >= 1 && n <= 65535 {
				defaultListen = ":" + strconv.Itoa(n)
			}
		}
	}
	deps.AdminPathCfg = adminPathCfg

	identityKey := cfg.PIIKey
	if identityKey == "" {
		// 兼容尚未配置独立 PII key 的旧安装；新安装由 installer 生成独立密钥。
		identityKey, err = crypto.DeriveKey(cfg.SecretKey, "identity-pii")
		if err != nil {
			database.Close()
			return nil, fmt.Errorf("派生实名资料密钥失败: %w", err)
		}
	}
	piiCryptor, err := crypto.New(identityKey)
	if err != nil {
		database.Close()
		return nil, fmt.Errorf("初始化实名资料加密器失败: %w", err)
	}
	identityFiles := &storage.PrivateFiles{Root: cfg.PrivateDataDir}

	// 通知/实名服务：Notifier 先建，供身份/验证码/Auth 共享。
	notifier := service.NewNotifier(database, settingsRepo)
	pluginHost := &plugin.Host{DB: database, Settings: settingsRepo, Notify: notifier, PrivateRoot: cfg.PrivateDataDir}
	identity := service.NewIdentity(identityStore, users, piiCryptor, identityFiles,
		notifier, identityKey, notifier, settingsRepo, cfg.BaseURL,
		service.NewConfiguredVerificationProvider(settingsRepo, ""))
	localCaptcha := captcha.New(database, settingsRepo, []byte(cfg.SecretKey))

	// ---- 服务层（单例，组合根统一注入） ----
	servicesRepo := service.NewServicesRepo(database)
	upstreamGuard := &service.UpstreamGuard{Servers: serversRepo, Products: products, Providers: providers}
	orders := service.NewOrders(database, products, coupons, identity, upstreamGuard, notifier)
	promotionSvc := service.NewPromotionService(database, promotionsRepo, coupons)
	orders.Promotion = promotionSvc
	console := service.NewConsole(database, serversRepo, products, providers, cryptor)
	lifecycle := service.NewLifecycle(database, serversRepo, products, providers, provisions, jobs)
	paymentSvc := service.NewPayment(database, lifecycle, serversRepo, products, provisions, jobs, balanceRepo, providers, periodGrants, notifier, cryptor)
	paymentSvc.Promotion = promotionSvc
	// 插件初始化（Refunder 已就绪；事件订阅在此生效；Init 失败即启动失败——编译期插件的问题应启动期暴露）
	pluginHost.Refunder = paymentSvc
	for _, pl := range plugin.All() {
		if err := pl.Init(pluginHost.ForPlugin(pl.Info().Name)); err != nil {
			database.Close()
			return nil, fmt.Errorf("插件 %s 初始化失败: %w", pl.Info().Name, err)
		}
	}
	// 支付网关：各驱动包 init() 自注册（新增见 gateway/registry.go）。
	gateways := gateway.All()
	// 支持订单查询的网关（异步通知丢失时补单）。
	orderQueriers := gateway.OrderQueriers()
	challenges := &service.AuthChallengeService{Store: authChallenges, SMS: identity.OTP, EmailCodeSend: notifier.SendEmailCode, SiteName: notifier.SiteName, Key: []byte(cfg.SecretKey)}
	auth := &handler.Auth{
		Users:        users,
		Sessions:     store,
		Settings:     settingsRepo,
		Lockout:      loginAttempts,
		Notifier:     notifier,
		Challenges:   challenges,
		Captcha:      service.NewConfiguredCaptchaProvider(settingsRepo),
		LocalCaptcha: localCaptcha,
		Deps:         deps,
	}
	pay := &handler.Pay{
		Orders:   orders,
		Payment:  paymentSvc,
		Products: products,
		Gateways: gateways,
		GwRepo:   gatewaysRepo,
		Invoices: invoices,
		Balance:  balanceRepo,
		Deps:     deps,
	}
	adminHandler := &handler.Admin{
		DB:            database,
		PrivateFiles:  identityFiles,
		Admins:        admins,
		Lockout:       loginAttempts,
		LocalCaptcha:  localCaptcha,
		Coupons:       coupons,
		Refunds:       refunds,
		AdminLog:      adminLog,
		Stats:         statsRepo,
		Promotions:    promotionsRepo,
		Notifier:      notifier,
		Updater:       &update.Client{Repo: "wmwlwmwl/LumeIDC", Version: version},
		DefaultListen: defaultListen,
		Deps:          deps,
	}
	pages := &handler.Pages{
		DB:            database,
		PrivateFiles:  identityFiles,
		Products:      products,
		Svc:           servicesRepo,
		Orders:        orders,
		UsersRepo:     users,
		ServersRepo:   serversRepo,
		Console:       console,
		Balance:       balanceRepo,
		Notifier:      notifier,
		Announcements: announcements,
		Settings:      settingsRepo,
		Invoices:      invoices,
		Promotion:     promotionSvc,
		Payment:       paymentSvc,
		CancelReqs:    cancelReqs,
		Deps:          deps,
	}

	mux := http.NewServeMux()
	auth.Register(mux)
	pages.Register(mux)
	// 后台「服务实例 → 管理」：把用户侧服务接口镜像到 /admin/services/{id}/...（见 admin_service_proxy.go）
	pages.RegisterAdminServiceProxy(mux)
	// SPA 启动端点：返回 CSRF 令牌 / 站点品牌 / 登录态（前端 mount 前调用）。
	sessionHandler := &handler.Session{Users: users, Deps: deps}
	sessionHandler.Register(mux)
	verificationHandler := &handler.VerificationHandler{Identity: identity, Users: users, Sessions: store, AdminLog: adminLog, Challenges: challenges, StepKey: []byte(cfg.SecretKey), LocalCaptcha: localCaptcha, Captcha: service.NewConfiguredCaptchaProvider(settingsRepo), Deps: deps}
	verificationHandler.Register(mux)
	pay.Register(mux)
	adminHandler.Register(mux)
	registerAdminRoutes(mux,
		&handler.AdminVerification{Identity: identity, Users: users, AdminLog: adminLog, Deps: deps},
		&handler.AdminGateway{GwRepo: gatewaysRepo, Gateways: gateways, Deps: deps},
		&handler.AdminServers{Servers: serversRepo, Providers: providers, Deps: deps},
		&handler.AdminManage{
			Products: products, Users: users, Servers: serversRepo,
			Balance:     balanceRepo,
			Svc:         servicesRepo,
			Lifecycle:   lifecycle,
			Payment:     paymentSvc,
			Providers:   providers,
			Settings:    settingsRepo,
			Identity:    identityStore,
			IdentitySvc: identity,
			AdminLog:    adminLog,
			CancelReqs:  cancelReqs,
			Notifier:    notifier,
			Deps:        deps,
		},
	)
	// 插件路由：每个插件一个子 mux（相对路径注册），框架统一挂到
	// /admin/plugin/{name}/ 与 /plugin/{name}/ 前缀并包启用态闸门（禁用即 404）；
	// 与核心同一 mux，自动获得 session/CSRF/后台路径中间件。
	for _, pl := range plugin.All() {
		name := pl.Info().Name
		var adminSub, clientSub *http.ServeMux
		if _, ok := pl.(plugin.ConfigSchemaProvider); ok {
			adminSub = http.NewServeMux()
			// 配置 schema 统一读写 API（插件零 handler 获得配置页）
			cfgH := &handler.AdminPluginConfig{Settings: settingsRepo, AdminLog: adminLog, Name: name, Schema: pl.(plugin.ConfigSchemaProvider).ConfigSchema()}
			adminSub.HandleFunc("GET /config", cfgH.Get)
			adminSub.HandleFunc("POST /config", cfgH.Save)
		}
		if rr, ok := pl.(plugin.AdminRouteRegistrar); ok {
			if adminSub == nil {
				adminSub = http.NewServeMux()
			}
			rr.RegisterAdminRoutes(adminSub)
		}
		if rr, ok := pl.(plugin.ClientRouteRegistrar); ok {
			clientSub = http.NewServeMux()
			rr.RegisterClientRoutes(clientSub)
		}
		if adminSub != nil {
			prefix := "/admin/plugin/" + name
			// 必须带方法注册：无方法的全路子树与 SPA 兜底 "GET /{path...}"
			// 构成"路径宽方法窄 vs 路径窄方法宽"的重叠，ServeMux 会 panic。
			gated := plugin.Gate(name, http.StripPrefix(prefix, adminSub))
			for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodDelete} {
				mux.Handle(method+" "+prefix+"/", gated)
			}
		}
		if clientSub != nil {
			prefix := "/plugin/" + name
			gated := plugin.Gate(name, http.StripPrefix(prefix, clientSub))
			for _, method := range []string{http.MethodGet, http.MethodPost} {
				mux.Handle(method+" "+prefix+"/", gated)
			}
		}
	}
	(&handler.AdminPlugins{AdminLog: adminLog}).Register(mux)
	registerHealthRoutes(mux, database)
	// Vite SPA 静态资源与文档兜底（GET 未命中任意 SSR/API 路由时）。
	// "GET /{path...}" 比 "/" 更精确，GET 未知路径交给 SPA；其余方法仍走渲染 404。
	handler.RegisterWebUI(mux, settingsRepo)
	// 全局 404 兜底（未匹配路由统一渲染站点 404 页）
	mux.HandleFunc("/", pages.NotFound)

	// SPA 接管已迁移路径的「浏览器导航」（返回 SPA 外壳）；API 请求与
	// 未迁移路径（服务子路径/回调等）透传，SSR 行为不变。
	// 认证（含手机验证码）与实名（含自动实名插件）页均已迁移到 SPA。
	root := handler.SPAGate(mux, settingsRepo)
	h := store.Middleware(middleware.CSRF(root))
	h = middleware.AdminPath(h, adminPathCfg)
	// 最外层兜 panic：任意中间件或 handler 崩溃都要变成结构化 500，而不是断连。
	h = middleware.Recover(h)
	addr := defaultListen
	if a := os.Getenv("LISTEN"); a != "" {
		addr = a // 环境变量覆盖配置，便于本地多实例测试
	}
	fulfillment := service.NewFulfillment(jobs, paymentSvc, lifecycle, notifier)
	// 支付成功立即异步执行履约队列（cron 每 15s 轮询仍兜底），开通不再等轮询周期
	paymentSvc.TriggerFulfillment = func() { fulfillment.TriggerDrain(context.Background(), 3) }
	cronJobs := &cron.Jobs{DB: database, Fulfillment: fulfillment, Notifier: notifier,
		Providers: providers, Servers: serversRepo, Products: products, Coupons: coupons,
		Lifecycle: lifecycle,
		Gateways:  gatewaysRepo, Payment: paymentSvc, Settings: settingsRepo,
		OrderQueriers: orderQueriers}
	notifier.StartMail()
	notifier.StartSMS()
	cronRef := cronJobs.Start()
	app := &App{
		Server: &http.Server{
			Addr:              addr,
			Handler:           h,
			ReadHeaderTimeout: 10 * time.Second,  // 防慢头攻击：读完全部请求头的超时
			WriteTimeout:      0,                 // WebSocket 隧道不能被响应写入超时断开
			ReadTimeout:       0,                 // 同上，读请求体也不设上限
			IdleTimeout:       120 * time.Second, // Keep-Alive 空闲超时
		},
		DB:       database,
		Notifier: notifier,
		Cron:     cronRef,
		stopped:  make(chan struct{}),
	}
	app.listenAddr = addr
	adminHandler.ListenSwitcher = app.SwitchListen // 后台修改监听端口后立即生效
	return app, nil
}

// registerAdminRoutes 注册后台管理路由：实名审核、支付网关、上游服务器与
// 用户/分类/产品/服务/停用审核/退款等管理操作。
func registerAdminRoutes(mux *http.ServeMux, adminVerification *handler.AdminVerification, gwHandler *handler.AdminGateway, srvHandler *handler.AdminServers, mng *handler.AdminManage) {
	mux.HandleFunc("GET /admin/verifications", adminVerification.List)
	mux.HandleFunc("GET /admin/verifications/{id}", adminVerification.Detail)
	mux.HandleFunc("POST /admin/verifications/{id}/approve", adminVerification.Approve)
	mux.HandleFunc("POST /admin/verifications/{id}/reject", adminVerification.Reject)
	mux.HandleFunc("GET /admin/verifications/{id}/photo/{side}", adminVerification.Photo)
	gwHandler.Register(mux)
	srvHandler.Register(mux)
	mux.HandleFunc("GET /admin/users/{id}/edit", mng.UserEdit)
	mux.HandleFunc("POST /admin/users/{id}/save", mng.UserSave)
	mux.HandleFunc("GET /admin/users/{id}/realname", mng.UserRealname)
	mux.HandleFunc("POST /admin/users/{id}/status", mng.UserSetStatus)
	mux.HandleFunc("POST /admin/users/{id}/recharge", mng.UserRecharge)
	mux.HandleFunc("POST /admin/users/{id}/refund", mng.UserRefund)
	mux.HandleFunc("GET /admin/types", mng.TypesList)
	mux.HandleFunc("POST /admin/types/save", mng.TypeSave)
	mux.HandleFunc("POST /admin/types/{id}/delete", mng.TypeDelete)
	mux.HandleFunc("POST /admin/types/{id}/moveproducts", mng.TypeMoveProducts)
	mux.HandleFunc("GET /admin/products", mng.ProductsList)
	mux.HandleFunc("GET /admin/products/new", mng.ProductForm)
	mux.HandleFunc("POST /admin/products/save", mng.ProductSave)
	mux.HandleFunc("GET /admin/products/{id}/edit", mng.ProductForm)
	mux.HandleFunc("POST /admin/products/{id}/save", mng.ProductSave)
	mux.HandleFunc("POST /admin/products/{id}/delete", mng.ProductDelete)
	mux.HandleFunc("GET /admin/users", mng.UsersList)
	mux.HandleFunc("GET /admin/services", mng.ServicesList)
	mux.HandleFunc("GET /admin/services/status", mng.ServicesStatusJSON)
	mux.HandleFunc("POST /admin/services/{id}/action", mng.ServiceAction)
	mux.HandleFunc("POST /admin/services/{id}/edit", mng.ServiceEdit)
	mux.HandleFunc("GET /admin/services/{id}/recovery", mng.ServiceRecovery)
	mux.HandleFunc("POST /admin/services/{id}/recovery", mng.ServiceRecovery)
	mux.HandleFunc("GET /admin/service-cancel-requests", mng.CancelRequestsList)
	mux.HandleFunc("POST /admin/service-cancel-requests/{id}/handle", mng.CancelRequestHandle)
	mux.HandleFunc("POST /admin/orders/{id}/refund", mng.OrderRefund)
	mux.HandleFunc("GET /admin/servers/{id}/catalog", mng.CatalogPage)
	mux.HandleFunc("POST /admin/servers/{id}/import", mng.ImportProducts)
	mux.HandleFunc("POST /admin/products/{id}/pull-config", mng.PullConfig)
	mux.HandleFunc("GET /admin/products/upstream-options", mng.UpstreamOptions)
	mux.HandleFunc("GET /admin/products/upstream-config", mng.UpstreamConfig)
}

// registerHealthRoutes 注册健康/就绪检查端点（不经过 CSRF）。
func registerHealthRoutes(mux *http.ServeMux, database *sql.DB) {
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if err := database.PingContext(r.Context()); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte(`{"status":"not ready","error":"database unreachable"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ready"}`))
	})
}

// sameListen 判断两个监听地址是否同一地址（端口相同且主机语义等价）。
// 主机为空、"0.0.0.0"、"::" 均视为全接口，互相等效；明确主机（如 127.0.0.1）需完全一致。
func sameListen(a, b string) bool {
	ha, pa, oka := splitListen(a)
	hb, pb, okb := splitListen(b)
	if !oka || !okb || pa != pb {
		return false
	}
	w := func(h string) bool { return h == "" || h == "0.0.0.0" || h == "::" }
	return (w(ha) && w(hb)) || ha == hb
}

func splitListen(addr string) (host string, port int, ok bool) {
	addr = strings.TrimSpace(addr)
	if !strings.Contains(addr, ":") {
		return "", 0, false
	}
	h, p, err := net.SplitHostPort(addr)
	if err != nil {
		// ":8080" 等无主机形式补全后重试
		h, p, err = net.SplitHostPort("0.0.0.0" + addr)
		if err != nil {
			return "", 0, false
		}
	}
	n, err := strconv.Atoi(p)
	if err != nil || n < 1 || n > 65535 {
		return "", 0, false
	}
	return h, n, true
}
