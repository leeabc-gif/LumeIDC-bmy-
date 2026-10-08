-- 同一域名同一操作类型同时只允许一条 pending 异步操作（部分唯一索引）。
-- 防止同域并发续费时，失败方按 LatestPaidOrder（domain+kind 取最新 paid 单）
-- 认领到成功那笔的订单并退错金额；硬保证，配合 renewInternal 的预检友好拒绝。

CREATE UNIQUE INDEX IF NOT EXISTS uq_spaceship_ops_pending
  ON plugin_spaceship_operations (domain_id, op_type)
  WHERE status = 'pending' AND domain_id IS NOT NULL;
