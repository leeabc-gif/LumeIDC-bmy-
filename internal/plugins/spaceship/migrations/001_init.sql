-- Spaceship 域名注册插件数据库表
-- 插件命名空间：plugin_spaceship_*

-- 联系人（WHOIS 信息，可多域名复用）
CREATE TABLE IF NOT EXISTS plugin_spaceship_contacts (
  id              bigserial PRIMARY KEY,
  contact_id      text NOT NULL UNIQUE,          -- Spaceship 返回的 contactId
  user_id         bigint,                         -- 所属用户（NULL = 管理员创建的共享模板）
  label           text,                           -- 备注名（如"默认联系人"、"张三私人"）
  first_name      text NOT NULL,
  last_name       text NOT NULL,
  organization    text,
  email           text NOT NULL,
  address1        text NOT NULL,
  address2        text,
  city            text NOT NULL,
  state_province  text,
  postal_code     text,
  country         text NOT NULL,                  -- ISO 2 位
  phone           text NOT NULL,                  -- +国际区号.号码
  is_default      boolean DEFAULT false,
  created_at      timestamptz DEFAULT now(),
  updated_at      timestamptz DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_spaceship_contacts_user  ON plugin_spaceship_contacts (user_id);
CREATE INDEX IF NOT EXISTS idx_spaceship_contacts_def   ON plugin_spaceship_contacts (is_default);

-- 域名（注册/续费/隐私开关/自动续费）
CREATE TABLE IF NOT EXISTS plugin_spaceship_domains (
  id                    bigserial PRIMARY KEY,
  user_id               bigint NOT NULL,
  domain                text NOT NULL UNIQUE,
  whois_id              bigint,                           -- 本插件 contacts 表 id
  contact_id            text,                             -- Spaceship contactId
  years                 int NOT NULL DEFAULT 1,
  paid_amount_cents     bigint NOT NULL,                  -- 实付金额（分）
  status                text NOT NULL DEFAULT 'pending',  -- pending/active/expired/transfer_locked/deleted
  privacy_level         text DEFAULT 'high',
  auto_renew            boolean DEFAULT false,
  spaceship_domain_id   text,
  registered_at         timestamptz,
  expires_at            timestamptz,
  deleted_at            timestamptz,
  created_at            timestamptz DEFAULT now(),
  updated_at            timestamptz DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_spaceship_domains_user    ON plugin_spaceship_domains (user_id);
CREATE INDEX IF NOT EXISTS idx_spaceship_domains_status  ON plugin_spaceship_domains (status);

-- 异步操作（Spaceship 注册是异步的，cron 轮询驱动状态机）
CREATE TABLE IF NOT EXISTS plugin_spaceship_operations (
  id              bigserial PRIMARY KEY,
  operation_id    text NOT NULL UNIQUE,             -- Spaceship 返回的 operationId
  domain_id       bigint,                           -- 关联域名（注册成功后填入）
  domain          text NOT NULL,                    -- 操作的域名
  op_type         text NOT NULL,                    -- domain_create/domain_renew/contact_create
  status          text NOT NULL DEFAULT 'pending',  -- pending/success/failed
  error_msg       text,                              -- 失败时的错误描述
  result          jsonb,                            -- Spaceship 完整返回
  started_at      timestamptz DEFAULT now(),
  finished_at     timestamptz,
  FOREIGN KEY (domain_id) REFERENCES plugin_spaceship_domains(id) ON DELETE SET NULL
);
CREATE INDEX IF NOT EXISTS idx_spaceship_ops_status   ON plugin_spaceship_operations (status);
CREATE INDEX IF NOT EXISTS idx_spaceship_ops_started  ON plugin_spaceship_operations (started_at);
