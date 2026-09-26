<script setup lang="ts">
import { computed, h, onMounted, ref } from 'vue'
import { ElMessage, ElMessageBox, ElTag, ElButton } from 'element-plus'
import { useWindowSize } from '@vueuse/core'
import type { ColumnOption } from '@/types'
import { formatMoney } from '@/utils/format'
import PublicPageHead from '@/components/public/PublicPageHead.vue'
import ArtStatsCard from '@/components/core/cards/art-stats-card/index.vue'
import { http } from '@/http'

interface RefundRow {
  id: number
  order_id: number
  amount: string
  reason?: string
  detail?: string
  method?: string
  method_label?: string
  status: string
  status_label?: string
  handle_note?: string
  refund_id?: number
  order_amount?: string
  order_cycle?: string
  order_paid_at?: string
  handled_at?: string
  created_at?: string
  updated_at?: string
}

interface EligibleOrder {
  id: number
  product_id?: number
  amount: string
  cycle?: string
  paid_at?: string
  refundable: string
  allowed_methods?: string[]
}

interface FormOptions {
  orders: EligibleOrder[]
  methods: { value: string; label: string }[]
  reasons: string[]
  maxCents: number
  maxText: string
  windowDays: number
}

interface Stats {
  pending: number
  approved: number
  rejected: number
}

const STATUS_TAG: Record<string, 'info' | 'warning' | 'success' | 'danger'> = {
  pending: 'warning',
  approved: 'success',
  rejected: 'danger',
  withdrawn: 'info',
}

const CYCLE_LABELS: Record<string, string> = {
  monthly: '按月',
  quarterly: '按季',
  yearly: '按年',
}

const { width } = useWindowSize()
const isMobile = computed(() => width.value < 768)

const loading = ref(false)
const list = ref<RefundRow[]>([])
const total = ref(0)
const page = ref(1)
const limit = ref(10)
const stats = ref<Stats>({ pending: 0, approved: 0, rejected: 0 })

// 前台父容器无确定高度，ArtTable 默认 height:100% 会塌缩裁掉数据行，按行数算高
const fitH = (n: number) => Math.min(460, Math.max(120, n * 44 + 46))

const drawerVisible = ref(false)
const optionsLoading = ref(false)
const options = ref<FormOptions>({ orders: [], methods: [], reasons: [], maxCents: 0, maxText: '0.00', windowDays: 0 })
const form = ref({ order_id: undefined as number | undefined, amount: undefined as number | undefined, reason: '', detail: '', method: 'balance' })
const submitting = ref(false)

const detailDialog = ref(false)
const current = ref<RefundRow | null>(null)

const selectedOrder = computed(() => options.value.orders.find((o) => o.id === form.value.order_id))
const maxRefundable = computed(() => Number(selectedOrder.value?.refundable || 0))

// 当前订单允许的退款方式（按商品规则过滤，无规则回退全局）。
const currentMethods = computed(() => {
  const allowed = selectedOrder.value?.allowed_methods
  if (!allowed || !allowed.length) return options.value.methods
  return options.value.methods.filter((m) => allowed.includes(m.value))
})

const columns = ref<ColumnOption<RefundRow>[]>([
  { prop: 'order_id', label: '订单', width: 90, formatter: (row) => `#${row.order_id}` },
  {
    prop: 'amount',
    label: '退款金额',
    width: 110,
    formatter: (row) => h('span', { style: 'color:var(--art-gray-900);font-weight:600' }, `￥${formatMoney(row.amount)}`),
  },
  { prop: 'method', label: '方式', width: 100, formatter: (row) => row.method_label || row.method || '—' },
  { prop: 'reason', label: '原因', minWidth: 120, formatter: (row) => row.reason || '—' },
  {
    prop: 'status',
    label: '状态',
    width: 100,
    formatter: (row) =>
      h(ElTag, { type: STATUS_TAG[row.status] || 'info', effect: 'light' }, row.status_label || row.status),
  },
  { prop: 'created_at', label: '申请时间', width: 160 },
  { prop: 'handle_note', label: '处理备注', minWidth: 120, formatter: (row) => row.handle_note || '—' },
  {
    prop: 'actions',
    label: '操作',
    width: 130,
    fixed: 'right',
    formatter: (row) =>
      h('div', { class: 'row-actions' }, [
        h(ElButton, { size: 'small', link: true, type: 'primary', onClick: () => openDetail(row) }, '详情'),
        row.status === 'pending'
          ? h(ElButton, { size: 'small', link: true, type: 'danger', onClick: () => withdraw(row) }, '撤回')
          : null,
      ]),
  },
])

function orderLabel(o: EligibleOrder) {
  return `订单 #${o.id} · ￥${formatMoney(o.amount)} · 可退 ￥${formatMoney(o.refundable)}`
}

async function loadStats() {
  try {
    const res = await http.get<{ ok: number; stats?: Stats }>('/plugin/refund/my-stats')
    if (res.stats) stats.value = res.stats
  } catch {
    /* 统计失败不阻塞列表 */
  }
}

async function load() {
  loading.value = true
  try {
    const res = await http.get<{ ok: number; list?: RefundRow[]; total?: number }>('/plugin/refund/my', {
      page: page.value,
      limit: limit.value,
    })
    list.value = res.list || []
    total.value = res.total || 0
  } catch (err: unknown) {
    ElMessage.error((err as Error).message || '读取失败')
  } finally {
    loading.value = false
  }
}

function handlePageChange(p: number) {
  page.value = p
  load()
}

async function openApply() {
  drawerVisible.value = true
  optionsLoading.value = true
  try {
    const res = await http.get<{
      ok: number
      orders?: EligibleOrder[]
      methods?: { value: string; label: string }[]
      reasons?: string[]
      maxCents?: number
      maxText?: string
      windowDays?: number
    }>('/plugin/refund/eligible-orders')
    options.value = {
      orders: res.orders || [],
      methods: res.methods || [],
      reasons: res.reasons || [],
      maxCents: res.maxCents || 0,
      maxText: res.maxText || '0.00',
      windowDays: res.windowDays || 0,
    }
    form.value = {
      order_id: undefined,
      amount: undefined,
      reason: '',
      detail: '',
      method: options.value.methods[0]?.value || 'balance',
    }
  } catch (err: unknown) {
    ElMessage.error((err as Error).message || '读取可退订单失败')
  } finally {
    optionsLoading.value = false
  }
}

/** 选中订单后默认填满可退余额，用户可再改小。 */
function onOrderChange() {
  form.value.amount = maxRefundable.value > 0 ? Number(maxRefundable.value.toFixed(2)) : undefined
  // 若当前退款方式不在该订单允许列表中，重置为第一个允许的方式。
  const allowed = currentMethods.value
  if (allowed.length && !allowed.some((m) => m.value === form.value.method)) {
    form.value.method = allowed[0].value
  }
}

let lastAmountWarnAt = 0
/** 超出可退余额时显式提示并钳回：不可用 el-input-number 的 :max，
 *  它在用户输入时静默钳制（填 6.00 会无声变 2.99），用户会误以为按自己填的金额提交。
 *  这里在 change 时对原始输入做判断，超额则提示并显式改回上限。 */
function onAmountChange(val: number | null | undefined) {
  const max = maxRefundable.value
  if (max <= 0 || val === null || val === undefined || !Number.isFinite(val)) return
  if (val > max) {
    form.value.amount = Number(max.toFixed(2))
    // input-number 的 input 与 blur 均会触发 change，短时间内的重复提示做节流
    const now = Date.now()
    if (now - lastAmountWarnAt > 1500) {
      lastAmountWarnAt = now
      ElMessage.warning(`退款金额不能超过可退余额 ￥${formatMoney(max)}，已自动调整为 ￥${formatMoney(max)}`)
    }
  }
}

async function submit() {
  if (!form.value.order_id) {
    ElMessage.warning('请选择要退款的订单')
    return
  }
  if (!form.value.amount || form.value.amount <= 0) {
    ElMessage.warning('请输入退款金额')
    return
  }
  if (maxRefundable.value > 0 && form.value.amount > maxRefundable.value) {
    ElMessage.warning(`退款金额不能超过可退余额 ￥${formatMoney(maxRefundable.value)}`)
    return
  }
  if (!form.value.reason.trim()) {
    ElMessage.warning('请选择或填写退款原因')
    return
  }
  submitting.value = true
  try {
    const res = await http.post<{ ok: number; status?: string; message?: string; msg?: string }>(
      '/plugin/refund/create',
      {
        order_id: form.value.order_id,
        amount: form.value.amount,
        reason: form.value.reason.trim(),
        detail: form.value.detail.trim(),
        method: form.value.method,
      }
    )
    if (String(res.ok) !== '1') {
      ElMessage.error(res.msg || '提交失败')
      return
    }
    ElMessage.success(res.message || '已提交')
    drawerVisible.value = false
    page.value = 1
    loadStats()
    load()
  } catch (err: unknown) {
    ElMessage.error((err as Error).message || '提交失败')
  } finally {
    submitting.value = false
  }
}

async function withdraw(row: RefundRow) {
  const ok = await ElMessageBox.confirm(
    `确认撤回订单 #${row.order_id} 的退款申请？撤回后可以重新提交。`,
    '撤回确认',
    { type: 'warning', confirmButtonText: '撤回', cancelButtonText: '取消' }
  ).catch(() => null)
  if (!ok) return
  try {
    const res = await http.post<{ ok: number; msg?: string }>(`/plugin/refund/${row.id}/withdraw`)
    if (String(res.ok) !== '1') {
      ElMessage.error(res.msg || '操作失败')
      return
    }
    ElMessage.success('已撤回')
    loadStats()
    load()
  } catch (err: unknown) {
    ElMessage.error((err as Error).message || '操作失败')
  }
}

function openDetail(row: RefundRow) {
  current.value = row
  detailDialog.value = true
}

onMounted(() => {
  loadStats()
  load()
})
</script>

<template>
  <div>
    <PublicPageHead title="申请退款" subtitle="在可退期限内提交退款申请，审核通过后由系统自动执行退款。">
      <template #extra>
        <el-button type="primary" @click="openApply">申请退款</el-button>
      </template>
    </PublicPageHead>

    <ElRow :gutter="20" class="stats-row">
      <ElCol :xs="24" :sm="8">
        <ArtStatsCard
          icon="ri:time-line"
          icon-style="bg-warning"
          title="待审核"
          :count="stats.pending"
          description="等待平台处理的申请"
        />
      </ElCol>
      <ElCol :xs="24" :sm="8">
        <ArtStatsCard
          icon="ri:checkbox-circle-line"
          icon-style="bg-primary"
          title="已通过"
          :count="stats.approved"
          description="已通过并完成退款"
        />
      </ElCol>
      <ElCol :xs="24" :sm="8">
        <ArtStatsCard
          icon="ri:close-circle-line"
          icon-style="bg-danger"
          title="已驳回"
          :count="stats.rejected"
          description="未通过的申请"
        />
      </ElCol>
    </ElRow>

    <section class="records">
      <h3 class="section-title">我的退款申请</h3>

      <ArtTable
        v-if="!isMobile"
        :loading="loading"
        :data="list"
        :columns="columns"
        :height="fitH(list.length)"
        empty-height="120px"
        empty-text="暂无退款申请"
      />

      <div v-else v-loading="loading" class="refund-cards">
        <div v-for="item in list" :key="`refund-${item.id}`" class="art-card refund-card">
          <div class="refund-card__head">
            <div class="refund-card__title">
              <strong>订单 #{{ item.order_id }}</strong>
              <small>{{ item.created_at }}</small>
            </div>
            <el-tag :type="STATUS_TAG[item.status] || 'info'" effect="light" size="small">
              {{ item.status_label || item.status }}
            </el-tag>
          </div>
          <p class="refund-card__amount">￥{{ formatMoney(item.amount) }}</p>
          <div class="refund-card__meta">
            <span>{{ item.method_label || item.method }}</span>
            <span>{{ item.reason || '—' }}</span>
          </div>
          <p v-if="item.handle_note" class="refund-card__note">处理备注：{{ item.handle_note }}</p>
          <div class="refund-card__actions">
            <el-button size="small" link type="primary" @click="openDetail(item)">详情</el-button>
            <el-button v-if="item.status === 'pending'" size="small" link type="danger" @click="withdraw(item)">
              撤回
            </el-button>
          </div>
        </div>
        <el-empty v-if="!loading && !list.length" description="暂无退款申请" />
      </div>

      <div v-if="total > limit" class="pager">
        <el-pagination
          background
          layout="total, prev, pager, next"
          :total="total"
          :page-size="limit"
          :current-page="page"
          @current-change="handlePageChange"
        />
      </div>
    </section>

    <el-drawer v-model="drawerVisible" title="申请退款" size="560px">
      <div v-loading="optionsLoading" class="apply-form">
        <el-alert
          v-if="options.windowDays > 0"
          :title="`支持支付后 ${options.windowDays} 天内申请退款，超期订单不可退。`"
          type="info"
          :closable="false"
          show-icon
          class="apply-tip"
        />
        <el-alert
          v-if="options.maxCents > 0"
          :title="`单笔退款金额上限 ￥${options.maxText}。`"
          type="info"
          :closable="false"
          show-icon
          class="apply-tip"
        />

        <el-empty v-if="!optionsLoading && !options.orders.length" description="当前没有可退款的订单" />

        <el-form v-else label-width="88px">
          <el-form-item label="退款订单" required>
            <el-select v-model="form.order_id" placeholder="请选择订单" class="w-full" @change="onOrderChange">
              <el-option v-for="o in options.orders" :key="o.id" :value="o.id" :label="orderLabel(o)" />
            </el-select>
          </el-form-item>
          <el-form-item label="退款金额" required>
            <el-input-number
              v-model="form.amount"
              :min="0.01"
              :precision="2"
              :step="1"
              style="width: 100%"
              @change="onAmountChange"
            />
            <div class="form-tip">最多可退 ￥{{ formatMoney(maxRefundable) }}</div>
          </el-form-item>
          <el-form-item label="退款方式" required>
            <el-radio-group v-model="form.method">
              <el-radio v-for="m in currentMethods" :key="m.value" :value="m.value">{{ m.label }}</el-radio>
            </el-radio-group>
          </el-form-item>
          <el-form-item label="退款原因" required>
            <el-select v-if="options.reasons.length" v-model="form.reason" placeholder="请选择退款原因" class="w-full">
              <el-option v-for="r in options.reasons" :key="r" :value="r" :label="r" />
            </el-select>
            <el-input v-else v-model="form.reason" placeholder="请简要说明退款原因" maxlength="50" />
          </el-form-item>
          <el-form-item label="问题描述">
            <el-input
              v-model="form.detail"
              type="textarea"
              :rows="4"
              maxlength="500"
              show-word-limit
              placeholder="补充说明遇到的问题（选填，500 字以内）"
            />
          </el-form-item>
        </el-form>

        <div class="form-actions">
          <el-button
            type="primary"
            :loading="submitting"
            :disabled="!options.orders.length"
            @click="submit"
          >
            提交申请
          </el-button>
          <el-button @click="drawerVisible = false">取消</el-button>
        </div>
      </div>
    </el-drawer>

    <el-dialog v-model="detailDialog" title="退款申请详情" width="600px">
      <el-descriptions v-if="current" :column="1" border>
        <el-descriptions-item label="申请 ID">#{{ current.id }}</el-descriptions-item>
        <el-descriptions-item label="订单">
          订单 #{{ current.order_id }} · 订单金额 ￥{{ formatMoney(current.order_amount) }} ·
          {{ CYCLE_LABELS[current.order_cycle || ''] || current.order_cycle || '—' }} ·
          支付于 {{ current.order_paid_at || '—' }}
        </el-descriptions-item>
        <el-descriptions-item label="退款金额">￥{{ formatMoney(current.amount) }}</el-descriptions-item>
        <el-descriptions-item label="退款方式">{{ current.method_label || current.method }}</el-descriptions-item>
        <el-descriptions-item label="退款原因">{{ current.reason || '—' }}</el-descriptions-item>
        <el-descriptions-item label="问题描述">{{ current.detail || '—' }}</el-descriptions-item>
        <el-descriptions-item label="申请时间">{{ current.created_at || '—' }}</el-descriptions-item>
        <el-descriptions-item label="状态">
          <el-tag :type="STATUS_TAG[current.status] || 'info'" effect="light" size="small">
            {{ current.status_label || current.status }}
          </el-tag>
        </el-descriptions-item>
        <el-descriptions-item v-if="current.handled_at" label="处理时间">
          {{ current.handled_at }}
        </el-descriptions-item>
        <el-descriptions-item v-if="current.refund_id" label="关联退款记录">
          #{{ current.refund_id }}
        </el-descriptions-item>
        <el-descriptions-item v-if="current.handle_note" label="处理备注">
          {{ current.handle_note }}
        </el-descriptions-item>
      </el-descriptions>
      <template #footer>
        <el-button @click="detailDialog = false">关闭</el-button>
      </template>
    </el-dialog>
  </div>
</template>

<style scoped>
.stats-row {
  margin-bottom: 18px;
}

.stats-row .el-col {
  margin-bottom: 12px;
}

.section-title {
  margin: 0 0 12px;
  color: var(--art-gray-800);
  font-size: 15px;
  font-weight: 600;
}

.refund-cards {
  display: flex;
  flex-direction: column;
  gap: 12px;
}

.refund-card {
  padding: 16px;
  border-radius: var(--radius-lg);
}

.refund-card__head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 10px;
}

.refund-card__title {
  display: flex;
  min-width: 0;
  flex-direction: column;
}

.refund-card__title strong {
  overflow: hidden;
  color: var(--art-gray-800);
  font-size: 14px;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.refund-card__title small {
  color: var(--art-gray-500);
  font-size: 11px;
}

.refund-card__amount {
  margin: 10px 0 6px;
  color: var(--art-gray-900);
  font-size: 20px;
  font-weight: 600;
}

.refund-card__meta {
  display: flex;
  flex-wrap: wrap;
  gap: 6px 14px;
  color: var(--art-gray-500);
  font-size: 12px;
}

.refund-card__note {
  margin: 8px 0 0;
  padding: 8px 10px;
  border-radius: var(--radius-md);
  background: var(--art-gray-100, rgba(0, 0, 0, 0.03));
  color: var(--art-gray-600);
  font-size: 12px;
  line-height: 1.6;
}

.refund-card__actions {
  display: flex;
  gap: 12px;
  margin-top: 8px;
}

.row-actions {
  display: flex;
  flex-wrap: wrap;
  gap: 4px;
}

.apply-tip {
  margin-bottom: 12px;
}

.form-tip {
  margin-top: 4px;
  color: var(--art-gray-500);
  font-size: 12px;
  line-height: 1.5;
}

.form-actions {
  display: flex;
  gap: 12px;
  margin-top: 20px;
}

.pager {
  display: flex;
  justify-content: center;
  margin-top: 18px;
}

@media (max-width: 640px) {
  :deep(.el-dialog) {
    width: calc(100% - 24px) !important;
    min-width: 0;
  }
}

@media (max-width: 600px) {
  :deep(.el-drawer) {
    width: 100% !important;
  }
}
</style>
