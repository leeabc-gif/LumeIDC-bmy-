-- Spaceship 域名注册插件：服务端定价表（P0 修复）
-- 修复：此前注册金额完全由客户端 paidAmount 决定，任何人填 0.01 元即可注册域名。
-- 定价表由管理员在后台维护，注册/续费一律以服务端价格为准。

CREATE TABLE IF NOT EXISTS plugin_spaceship_prices (
  id              bigserial PRIMARY KEY,
  tld             text NOT NULL UNIQUE,          -- 小写后缀，不含点，如 com / net / cn
  register_cents  bigint NOT NULL,               -- 注册单价（分/年）
  renew_cents     bigint NOT NULL,               -- 续费单价（分/年）
  currency        text NOT NULL DEFAULT 'CNY',
  enabled         boolean NOT NULL DEFAULT true, -- 关闭后该后缀不可自助注册
  min_years       int NOT NULL DEFAULT 1,
  max_years       int NOT NULL DEFAULT 10,
  created_at      timestamptz NOT NULL DEFAULT now(),
  updated_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_spaceship_prices_enabled ON plugin_spaceship_prices (enabled);

-- 注册/续费订单：记录扣款与退款轨迹，供对账与失败自动退款
CREATE TABLE IF NOT EXISTS plugin_spaceship_orders (
  id            bigserial PRIMARY KEY,
  domain_id     bigint REFERENCES plugin_spaceship_domains(id) ON DELETE SET NULL,
  domain        text NOT NULL,
  user_id       bigint NOT NULL,
  kind          text NOT NULL,                   -- register / renew
  years         int NOT NULL DEFAULT 1,
  amount_cents  bigint NOT NULL,                 -- 实际扣款（分）
  status        text NOT NULL DEFAULT 'paid',    -- paid / refunded
  note          text,
  created_at    timestamptz NOT NULL DEFAULT now(),
  updated_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_spaceship_orders_user   ON plugin_spaceship_orders (user_id);
CREATE INDEX IF NOT EXISTS idx_spaceship_orders_domain ON plugin_spaceship_orders (domain);

-- 默认价格兜底（未维护价目表时至少不是"客户端说了算"）
INSERT INTO plugin_spaceship_prices (tld, register_cents, renew_cents)
VALUES ('com', 8800, 8800), ('net', 9800, 9800), ('org', 9800, 9800), ('cn', 3900, 3900)
ON CONFLICT (tld) DO NOTHING;
