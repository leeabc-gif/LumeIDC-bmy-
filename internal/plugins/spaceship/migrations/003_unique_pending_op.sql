-- 同一域名同一操作类型同时只允许一条 pending 异步操作（部分唯一索引）。
-- 防止同域并发续费时，失败方按 LatestPaidOrder（domain+kind 取最新 paid 单）
-- 认领到成功那笔的订单并退错金额；硬保证，配合 renewInternal 的预检友好拒绝。

-- 建索引前先去重，防止存量库的并发续费脏数据（同 domain_id+op_type 多条 pending）
-- 导致索引创建失败、插件迁移报错、整机启动中止。
-- 处置：保留最新一条 pending 交给 cron 正常轮询收敛；较旧的重复 pending 标记 failed
-- 并留痕"需人工对账"——其对应订单仍为 paid，管理员按操作日志补退款。
UPDATE plugin_spaceship_operations o
SET status = 'failed',
    error_msg = COALESCE(NULLIF(o.error_msg, ''),
                         '迁移去重：同域名同类型存在多条 pending，本条被标记失败，对应订单款项需人工对账'),
    finished_at = now()
WHERE o.status = 'pending'
  AND o.domain_id IS NOT NULL
  AND EXISTS (
    SELECT 1 FROM plugin_spaceship_operations newer
    WHERE newer.domain_id = o.domain_id
      AND newer.op_type = o.op_type
      AND newer.status = 'pending'
      AND newer.id > o.id
  );

CREATE UNIQUE INDEX IF NOT EXISTS uq_spaceship_ops_pending
  ON plugin_spaceship_operations (domain_id, op_type)
  WHERE status = 'pending' AND domain_id IS NOT NULL;
