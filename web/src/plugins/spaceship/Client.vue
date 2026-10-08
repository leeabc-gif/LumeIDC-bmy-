<script setup lang="ts">
import { computed, h, onMounted, reactive, ref } from 'vue'
import { ElMessage, ElMessageBox, ElTag, ElButton } from 'element-plus'
import { useWindowSize } from '@vueuse/core'
import type { ColumnOption } from '@/types'
import PublicPageHead from '@/components/public/PublicPageHead.vue'
import ArtStatsCard from '@/components/core/cards/art-stats-card/index.vue'
import { http } from '@/http'

interface CheckItem {
  domain: string
  result: string
  ok: boolean
  premium: boolean
  price: number
  listPrice?: string
  listPriceCents?: number
  sellable?: boolean
  // 询价时的年限快照：listPriceCents 是「单价×该年限」的总价，
  // 用户询价后改年限再点注册时，金额换算必须用这个分母而非当前 searchYears
  quoteYears?: number
}

interface PriceRow {
  tld: string
  registerCents: number
  renewCents: number
  currency: string
  minYears: number
  maxYears: number
  enabled: boolean
  registerPrice: string
  renewPrice: string
}

interface MyDomain {
  id: number
  domain: string
  years: number
  status: string
  privacyLevel: string
  autoRenew: boolean
  paidAmountCents: number
  paidAmountText?: string
  registeredAt?: string
  expiresAt?: string
}

interface MyContact {
  id: number
  contactId: string
  label?: string
  name: string
  email: string
  country: string
  phone: string
  isDefault: boolean
  shared?: boolean // 管理员共享模板：只读，不可编辑/删除/设默认
  firstName?: string
  lastName?: string
  organization?: string
  address1?: string
  address2?: string
  city?: string
  stateProvince?: string
  postalCode?: string
}

interface ContactForm {
  id: number // 0=新建，>0=编辑既有联系人（后端据此复用上游 contactId）
  firstName: string
  lastName: string
  organization: string
  email: string
  address1: string
  address2: string
  city: string
  stateProvince: string
  postalCode: string
  country: string
  phone: string
  label: string
  isDefault: boolean
}

const STATUS_TAG: Record<string, 'info' | 'warning' | 'success' | 'danger'> = {
  pending: 'warning',
  active: 'success',
  failed: 'danger',
  stuck: 'danger',
  deleted: 'info',
}

const STATUS_LABELS: Record<string, string> = {
  pending: '处理中',
  active: '生效中',
  failed: '注册失败',
  stuck: '处理超时',
  deleted: '已删除',
}

const COUNTRY_OPTIONS = [
  { value: 'CN', label: '中国 (CN)' },
  { value: 'US', label: '美国 (US)' },
  { value: 'HK', label: '香港 (HK)' },
  { value: 'TW', label: '台湾 (TW)' },
  { value: 'SG', label: '新加坡 (SG)' },
  { value: 'JP', label: '日本 (JP)' },
]

function newContactForm(): ContactForm {
  return {
    id: 0,
    firstName: '',
    lastName: '',
    organization: '',
    email: '',
    address1: '',
    address2: '',
    city: '',
    stateProvince: '',
    postalCode: '',
    country: 'CN',
    phone: '',
    label: '',
    isDefault: false,
  }
}

const { width } = useWindowSize()
const isMobile = computed(() => width.value < 768)

// 前台父容器无确定高度， ArtTable 默认 height:100% 会塌缩裁掉数据行，按行数算高
const fitH = (n: number) => Math.min(460, Math.max(120, n * 44 + 46))

const activeTab = ref<'search' | 'domains' | 'contacts' | 'prices'>('search')

// ---- 域名查询与注册 ----
const searchInput = ref('')
const searchYears = ref(1)
const checking = ref(false)
const checkResults = ref<CheckItem[]>([])
const registerDialogVisible = ref(false)
const registerSubmitting = ref(false)
const registerForm = reactive({
  domain: '',
  years: 1,
  contactId: 0,
})
const registerQuote = ref<{
  amount: string
  amountCents: number
  unitCents: number // 单年价（分）：报价按查询时年限给出，弹窗内改年限需按单价重算
  premium: boolean
  premiumPrice?: number
  currency: string
  sellable: boolean
} | null>(null)

// 弹窗内年限可调，应付金额按单价 × 当前年限实时换算（后端提交时仍会重算扣款）
const registerAmountText = computed(() => {
  const q = registerQuote.value
  if (!q || !q.unitCents) return ''
  return ((q.unitCents * registerForm.years) / 100).toFixed(2)
})

async function doCheck() {
  const parts = searchInput.value
    .split(/[\s,，;；]+/)
    .map((s) => s.trim().toLowerCase())
    .filter((s) => s.includes('.'))
  if (!parts.length) {
    ElMessage.warning('请输入要查询的域名，多个用空格或逗号分隔')
    return
  }
  if (parts.length > 20) {
    ElMessage.warning('单次最多查询 20 个域名')
    return
  }
  checking.value = true
  try {
    const res = await http.post<{ ok: number; list?: CheckItem[]; msg?: string }>('/plugin/spaceship/check', {
      domains: parts,
      years: searchYears.value,
    })
    if (String(res.ok) !== '1') {
      ElMessage.error(res.msg || '查询失败')
      return
    }
    // 快照询价年限：后续金额换算的分母以询价时点为准
    const years = searchYears.value
    checkResults.value = (res.list || []).map((x) => ({ ...x, quoteYears: years }))
    if (!checkResults.value.length) ElMessage.info('未查询到结果')
  } catch (err: unknown) {
    ElMessage.error((err as Error).message || '查询失败')
  } finally {
    checking.value = false
  }
}

function resultLabel(item: CheckItem): { text: string; type: 'info' | 'warning' | 'success' | 'danger' } {
  if (!item.ok && item.result === 'processing') return { text: '查询中', type: 'warning' }
  if (!item.ok && item.result === 'unknown') return { text: '未知', type: 'info' }
  if (!item.ok) return { text: '已注册', type: 'info' }
  if (item.premium) return { text: `溢价 ￥${item.price || '—'}`, type: 'warning' }
  return { text: '可注册', type: 'success' }
}

async function openRegister(item: CheckItem) {
  registerForm.domain = item.domain
  // 年限与单价分母都以询价时点的快照为准，避免用户询价后改 searchYears 导致金额错位
  const quoteYears = item.quoteYears || searchYears.value
  registerForm.years = quoteYears
  registerForm.contactId = contacts.value.find((c) => c.isDefault)?.id ?? contacts.value[0]?.id ?? 0
  registerQuote.value = {
    amount: item.listPrice || '',
    amountCents: item.listPriceCents || 0,
    unitCents: quoteYears > 0 ? Math.round((item.listPriceCents || 0) / quoteYears) : item.listPriceCents || 0,
    premium: !!item.premium,
    premiumPrice: item.price,
    currency: '',
    sellable: !!item.sellable,
  }
  registerDialogVisible.value = true
  if (!contacts.value.length) await loadContacts()
}

async function submitRegister() {
  if (registerQuote.value?.premium) {
    ElMessage.warning('该域名为溢价域名，请联系管理员确认价格后代注册')
    return
  }
  if (!registerQuote.value?.sellable) {
    ElMessage.warning('该后缀未上架或未配置价格，暂不支持自助注册')
    return
  }
  registerSubmitting.value = true
  try {
    const res = await http.post<{
      ok: number
      domainId?: number
      operationId?: string
      status?: string
      msg?: string
    }>('/plugin/spaceship/register', {
      domain: registerForm.domain,
      years: registerForm.years,
      contactId: registerForm.contactId || undefined,
    })
    if (String(res.ok) !== '1') {
      ElMessage.error(res.msg || '注册失败')
      return
    }
    ElMessage.success(res.msg || '注册请求已提交')
    registerDialogVisible.value = false
    loadDomains()
  } catch (err: unknown) {
    ElMessage.error((err as Error).message || '注册失败')
  } finally {
    registerSubmitting.value = false
  }
}

// ---- 我的域名 ----
const domainsLoading = ref(false)
const domains = ref<MyDomain[]>([])

const domainStats = computed(() => ({
  total: domains.value.length,
  active: domains.value.filter((d) => d.status === 'active').length,
  pending: domains.value.filter((d) => d.status === 'pending').length,
}))

async function loadDomains() {
  domainsLoading.value = true
  try {
    const res = await http.get<{ ok: number; list?: MyDomain[] }>('/plugin/spaceship/my')
    domains.value = res.list || []
  } catch (err: unknown) {
    ElMessage.error((err as Error).message || '读取我的域名失败')
  } finally {
    domainsLoading.value = false
  }
}

// ---- 自助续费 ----
const renewDialogVisible = ref(false)
const renewSubmitting = ref(false)
const renewTarget = ref<MyDomain | null>(null)
const renewForm = reactive({ years: 1 })

function openRenew(row: MyDomain) {
  renewTarget.value = row
  renewForm.years = 1
  renewDialogVisible.value = true
}

async function submitRenew() {
  const row = renewTarget.value
  if (!row) return
  renewSubmitting.value = true
  try {
    const res = await http.post<{ ok: number; msg?: string }>(
      `/plugin/spaceship/my/${row.id}/renew`,
      { years: renewForm.years }
    )
    if (String(res.ok) !== '1') {
      ElMessage.error(res.msg || '续费失败')
      return
    }
    ElMessage.success(res.msg || '续费请求已提交')
    renewDialogVisible.value = false
    loadDomains()
  } catch (err: unknown) {
    ElMessage.error((err as Error).message || '续费失败')
  } finally {
    renewSubmitting.value = false
  }
}

async function toggleAutoRenew(row: MyDomain) {
  try {
    const res = await http.post<{ ok: number; msg?: string }>(
      `/plugin/spaceship/my/${row.id}/autorenew`,
      { enable: !row.autoRenew }
    )
    if (String(res.ok) !== '1') {
      ElMessage.error(res.msg || '设置失败')
      return
    }
    ElMessage.success(row.autoRenew ? '已关闭自动续费' : '已开启自动续费')
  } catch (err: unknown) {
    ElMessage.error((err as Error).message || '设置失败')
  } finally {
    loadDomains()
  }
}

function paidText(row: MyDomain): string {
  if (row.paidAmountText) return row.paidAmountText
  return (row.paidAmountCents / 100).toFixed(2)
}

const domainColumns = ref<ColumnOption<MyDomain>[]>([
  { prop: 'domain', label: '域名', minWidth: 180, sortable: true },
  { prop: 'years', label: '年限', width: 80, formatter: (row) => `${row.years} 年` },
  {
    prop: 'paidAmountText',
    label: '实付',
    width: 100,
    formatter: (row) => `￥${paidText(row)}`,
  },
  {
    prop: 'status',
    label: '状态',
    width: 100,
    formatter: (row) =>
      h(ElTag, { type: STATUS_TAG[row.status] || 'info', effect: 'light' }, STATUS_LABELS[row.status] || row.status),
  },
  {
    prop: 'autoRenew',
    label: '自动续费',
    width: 110,
    formatter: (row) =>
      h(
        ElTag,
        { type: row.autoRenew ? 'success' : 'info', effect: 'light' },
        row.autoRenew ? '已开启' : '未开启'
      ),
  },
  {
    prop: 'expiresAt',
    label: '到期时间',
    width: 120,
    formatter: (row) => row.expiresAt || '—',
  },
  {
    prop: 'actions',
    label: '操作',
    width: 180,
    fixed: 'right',
    formatter: (row) =>
      h('div', { class: 'row-actions' }, [
        row.status === 'active'
          ? h(ElButton, { size: 'small', link: true, type: 'primary', onClick: () => openRenew(row) }, '续费')
          : null,
        row.status === 'active'
          ? h(
              ElButton,
              {
                size: 'small',
                link: true,
                type: row.autoRenew ? 'danger' : 'primary',
                onClick: () => toggleAutoRenew(row),
              },
              row.autoRenew ? '关闭续费' : '开启续费'
            )
          : null,
      ]),
  },
])

// ---- 我的联系人 ----
const contactsLoading = ref(false)
const contacts = ref<MyContact[]>([])
const contactDialogVisible = ref(false)
const contactSaving = ref(false)
const contactForm = ref<ContactForm>(newContactForm())

async function loadContacts() {
  contactsLoading.value = true
  try {
    const res = await http.get<{ ok: number; list?: MyContact[] }>('/plugin/spaceship/my/contacts')
    contacts.value = res.list || []
  } catch (err: unknown) {
    ElMessage.error((err as Error).message || '读取联系人失败')
  } finally {
    contactsLoading.value = false
  }
}

function openContactDialog(row?: MyContact) {
  if (row) {
    // 编辑：带上本地 id 让后端复用上游 contactId（共享模板不允许编辑，入口不会渲染）
    contactForm.value = {
      ...newContactForm(),
      id: row.id,
      label: row.label || '',
      email: row.email,
      country: row.country,
      phone: row.phone,
      isDefault: row.isDefault,
      firstName: row.firstName || '',
      lastName: row.lastName || '',
      organization: row.organization || '',
      address1: row.address1 || '',
      address2: row.address2 || '',
      city: row.city || '',
      stateProvince: row.stateProvince || '',
      postalCode: row.postalCode || '',
    }
  } else {
    contactForm.value = newContactForm()
  }
  contactDialogVisible.value = true
}

async function submitContact() {
  const f = contactForm.value
  if (!f.firstName.trim() || !f.lastName.trim() || !f.email.trim()) {
    ElMessage.warning('请填写姓名与邮箱')
    return
  }
  if (!f.address1.trim() || !f.city.trim() || !f.phone.trim()) {
    ElMessage.warning('请填写地址、城市与电话')
    return
  }
  contactSaving.value = true
  try {
    const res = await http.post<{ ok: number; msg?: string }>('/plugin/spaceship/my/contacts', {
      existingId: f.id > 0 ? f.id : 0, // 传入本地 id 表示更新原联系人（复用上游 contactId）
      firstName: f.firstName.trim(),
      lastName: f.lastName.trim(),
      organization: f.organization.trim(),
      email: f.email.trim(),
      address1: f.address1.trim(),
      address2: f.address2.trim(),
      city: f.city.trim(),
      stateProvince: f.stateProvince.trim(),
      postalCode: f.postalCode.trim(),
      country: f.country,
      phone: f.phone.trim(),
      label: f.label.trim() || `${f.firstName} ${f.lastName}`,
      isDefault: f.isDefault,
    })
    if (String(res.ok) !== '1') {
      ElMessage.error(res.msg || '保存失败')
      return
    }
    ElMessage.success('联系人已保存')
    contactDialogVisible.value = false
    loadContacts()
  } catch (err: unknown) {
    ElMessage.error((err as Error).message || '保存失败')
  } finally {
    contactSaving.value = false
  }
}

async function setDefaultContact(row: MyContact) {
  try {
    const res = await http.post<{ ok: number; msg?: string }>(
      `/plugin/spaceship/my/contacts/${row.id}/default`
    )
    if (String(res.ok) !== '1') {
      ElMessage.error(res.msg || '设置失败')
      return
    }
    ElMessage.success('已设为默认')
    loadContacts()
  } catch (err: unknown) {
    ElMessage.error((err as Error).message || '设置失败')
  }
}

async function deleteContact(row: MyContact) {
  const ok = await ElMessageBox.confirm(`确认删除联系人「${row.name}」？`, '删除确认', {
    type: 'warning',
    confirmButtonText: '删除',
    cancelButtonText: '取消',
  }).catch(() => null)
  if (!ok) return
  try {
    const res = await http.post<{ ok: number; msg?: string }>(
      `/plugin/spaceship/my/contacts/${row.id}/delete`
    )
    if (String(res.ok) !== '1') {
      ElMessage.error(res.msg || '删除失败')
      return
    }
    ElMessage.success('已删除')
    loadContacts()
  } catch (err: unknown) {
    ElMessage.error((err as Error).message || '删除失败')
  }
}

const contactColumns = ref<ColumnOption<MyContact>[]>([
  { prop: 'name', label: '姓名', minWidth: 120 },
  { prop: 'email', label: '邮箱', minWidth: 180 },
  { prop: 'country', label: '国家', width: 90 },
  { prop: 'phone', label: '电话', width: 150 },
  {
    prop: 'isDefault',
    label: '默认',
    width: 80,
    formatter: (row) =>
      row.isDefault ? h(ElTag, { type: 'success', effect: 'light', size: 'small' }, '默认') : null,
  },
  {
    prop: 'shared',
    label: '归属',
    width: 100,
    formatter: (row) =>
      row.shared
        ? h(ElTag, { type: 'warning', effect: 'light', size: 'small' }, '管理员共享')
        : h(ElTag, { type: 'info', effect: 'light', size: 'small' }, '我的'),
  },
  {
    prop: 'actions',
    label: '操作',
    width: 220,
    fixed: 'right',
    formatter: (row) =>
      // 共享模板只读：不给编辑/删除/设默认入口（后端也会 403 兜底）
      row.shared
        ? h('span', { class: 'text-g-500 text-xs' }, '只读')
        : h('div', { class: 'row-actions' }, [
            h(
              ElButton,
              { size: 'small', link: true, type: 'primary', onClick: () => openContactDialog(row) },
              '编辑'
            ),
            row.isDefault
              ? null
              : h(
                  ElButton,
                  {
                    size: 'small',
                    link: true,
                    type: 'primary',
                    onClick: () => setDefaultContact(row),
                  },
                  '设默认'
                ),
            h(
              ElButton,
              { size: 'small', link: true, type: 'danger', onClick: () => deleteContact(row) },
              '删除'
            ),
          ]),
  },
])

// ---- 价目表（公开只读） ----
const pricesLoading = ref(false)
const prices = ref<PriceRow[]>([])

async function loadPrices() {
  pricesLoading.value = true
  try {
    const res = await http.get<{ ok: number; list?: PriceRow[] }>('/plugin/spaceship/prices')
    prices.value = res.list || []
  } catch (err: unknown) {
    ElMessage.error((err as Error).message || '读取价目表失败')
  } finally {
    pricesLoading.value = false
  }
}

const priceColumns = ref<ColumnOption<PriceRow>[]>([
  {
    prop: 'tld',
    label: '后缀',
    minWidth: 110,
    formatter: (row) => `.${row.tld}`,
  },
  { prop: 'registerPrice', label: '注册价/年', width: 120, formatter: (row) => `￥${row.registerPrice}` },
  { prop: 'renewPrice', label: '续费价/年', width: 120, formatter: (row) => `￥${row.renewPrice}` },
  {
    prop: 'maxYears',
    label: '可注册年限',
    width: 130,
    formatter: (row) => `${row.minYears} ~ ${row.maxYears} 年`,
  },
])

function onTabChange(tab: string | number) {
  activeTab.value = tab as 'search' | 'domains' | 'contacts' | 'prices'
  if (tab === 'domains' && !domains.value.length) loadDomains()
  if (tab === 'contacts' && !contacts.value.length) loadContacts()
  if (tab === 'prices' && !prices.value.length) loadPrices()
}

onMounted(() => {
  loadDomains()
  loadContacts()
})
</script>

<template>
  <div>
    <PublicPageHead title="我的域名" subtitle="查询并注册新域名，管理已注册域名与 WHOIS 联系人。">
      <template #extra>
        <el-button type="primary" @click="activeTab = 'search'">注册新域名</el-button>
      </template>
    </PublicPageHead>

    <div class="spaceship-tabs">
      <button
        class="spaceship-tab"
        :class="{ active: activeTab === 'search' }"
        @click="onTabChange('search')"
      >查询注册</button>
      <button
        class="spaceship-tab"
        :class="{ active: activeTab === 'domains' }"
        @click="onTabChange('domains')"
      >我的域名</button>
      <button
        class="spaceship-tab"
        :class="{ active: activeTab === 'contacts' }"
        @click="onTabChange('contacts')"
      >我的联系人</button>
      <button
        class="spaceship-tab"
        :class="{ active: activeTab === 'prices' }"
        @click="onTabChange('prices')"
      >价目表</button>
    </div>

    <!-- 查询注册 -->
    <div v-show="activeTab === 'search'">
      <ElCard class="art-card">
        <el-input
          v-model="searchInput"
          type="textarea"
          :rows="3"
          placeholder="输入域名，多个用空格/逗号/换行分隔，如：example.com my-site.net"
          maxlength="1000"
          @keyup.enter="doCheck"
        />
        <div class="search-actions">
          <el-button type="primary" :loading="checking" @click="doCheck">查询可用性</el-button>
          <span class="search-tip">注册为异步操作，提交后由平台定时向 Spaceship 确认结果（通常 1-5 分钟）</span>
        </div>
        <div class="search-actions">
          <span class="search-tip">注册年限</span>
          <el-input-number v-model="searchYears" :min="1" :max="10" size="small" style="width: 120px" />
          <span class="search-tip">报价按此年限实时计算，提交时按服务端报价从余额扣款</span>
        </div>
        <div v-if="checkResults.length" class="check-results">
          <div v-for="item in checkResults" :key="item.domain" class="check-item art-card">
            <div class="check-item__info">
              <strong>{{ item.domain }}</strong>
              <el-tag :type="resultLabel(item).type" effect="light" size="small">
                {{ resultLabel(item).text }}
              </el-tag>
              <span v-if="item.listPrice" class="check-item__price">
                ￥{{ item.listPrice }} / {{ item.quoteYears }} 年
              </span>
              <span v-else class="check-item__tip">该后缀暂未上架</span>
            </div>
            <el-button
              v-if="item.ok && !item.premium && item.sellable"
              size="small"
              type="primary"
              @click="openRegister(item)"
            >
              注册
            </el-button>
            <span v-else-if="item.premium" class="check-item__tip">溢价域名请联系管理员</span>
          </div>
        </div>
        <el-empty v-else-if="!checking" description="输入域名后点击「查询可用性」" />
      </ElCard>
    </div>

    <!-- 我的域名 -->
    <div v-show="activeTab === 'domains'">
      <ElRow :gutter="20" class="stats-row">
        <ElCol :xs="24" :sm="8">
          <ArtStatsCard
            icon="ri:global-line"
            icon-style="bg-primary"
            title="域名总数"
            :count="domainStats.total"
            description="我注册的域名"
          />
        </ElCol>
        <ElCol :xs="24" :sm="8">
          <ArtStatsCard
            icon="ri:checkbox-circle-line"
            icon-style="bg-success"
            title="生效中"
            :count="domainStats.active"
            description="已注册成功"
          />
        </ElCol>
        <ElCol :xs="24" :sm="8">
          <ArtStatsCard
            icon="ri:time-line"
            icon-style="bg-warning"
            title="处理中"
            :count="domainStats.pending"
            description="等待注册结果"
          />
        </ElCol>
      </ElRow>

      <ArtTable
        v-if="!isMobile"
        :loading="domainsLoading"
        :data="domains"
        :columns="domainColumns"
        :height="fitH(domains.length)"
        empty-height="120px"
        empty-text="暂无域名，去「查询注册」注册第一个域名"
      />

      <div v-else v-loading="domainsLoading" class="domain-cards">
        <div v-for="item in domains" :key="`domain-${item.id}`" class="art-card domain-card">
          <div class="domain-card__head">
            <div class="domain-card__title">
              <strong>{{ item.domain }}</strong>
              <small>{{ item.years }} 年 · 实付 ￥{{ paidText(item) }}</small>
            </div>
            <el-tag :type="STATUS_TAG[item.status] || 'info'" effect="light" size="small">
              {{ STATUS_LABELS[item.status] || item.status }}
            </el-tag>
          </div>
          <div class="domain-card__meta">
            <span>到期：{{ item.expiresAt || '—' }}</span>
            <span>自动续费：{{ item.autoRenew ? '已开启' : '未开启' }}</span>
          </div>
          <div v-if="item.status === 'active'" class="domain-card__actions">
            <el-button size="small" link type="primary" @click="openRenew(item)">续费</el-button>
            <el-button size="small" link :type="item.autoRenew ? 'danger' : 'primary'" @click="toggleAutoRenew(item)">
              {{ item.autoRenew ? '关闭续费' : '开启续费' }}
            </el-button>
          </div>
        </div>
        <el-empty v-if="!domainsLoading && !domains.length" description="暂无域名" />
      </div>
    </div>

    <!-- 我的联系人 -->
    <div v-show="activeTab === 'contacts'">
      <div class="toolbar">
        <p class="toolbar-tip">注册域名时需要一个 WHOIS 联系人，创建后会同步保存到 Spaceship。</p>
        <el-button type="primary" @click="openContactDialog()">新增联系人</el-button>
      </div>

      <ArtTable
        v-if="!isMobile"
        :loading="contactsLoading"
        :data="contacts"
        :columns="contactColumns"
        :height="fitH(contacts.length)"
        empty-height="120px"
        empty-text="暂无联系人"
      />

      <div v-else v-loading="contactsLoading" class="contact-cards">
        <div v-for="item in contacts" :key="`contact-${item.id}`" class="art-card contact-card">
          <div class="contact-card__head">
            <div class="contact-card__title">
              <strong>{{ item.name }}</strong>
              <small>{{ item.email }}</small>
            </div>
            <el-tag v-if="item.isDefault" type="success" effect="light" size="small">默认</el-tag>
          </div>
          <div class="contact-card__meta">
            <span>{{ item.country }}</span>
            <span>{{ item.phone }}</span>
          </div>
          <div class="contact-card__actions">
            <el-button v-if="!item.isDefault" size="small" link type="primary" @click="setDefaultContact(item)">
              设默认
            </el-button>
            <el-button size="small" link type="danger" @click="deleteContact(item)">删除</el-button>
          </div>
        </div>
        <el-empty v-if="!contactsLoading && !contacts.length" description="暂无联系人" />
      </div>
    </div>

    <!-- 价目表 -->
    <div v-show="activeTab === 'prices'">
      <div class="toolbar">
        <p class="toolbar-tip">注册/续费均按此价目表从余额扣款，价格由平台维护，下单时不可自行填写。</p>
      </div>
      <ArtTable
        v-if="!isMobile"
        :loading="pricesLoading"
        :data="prices"
        :columns="priceColumns"
        :height="fitH(prices.length)"
        empty-height="120px"
        empty-text="暂未配置价目"
      />
      <div v-else v-loading="pricesLoading" class="contact-cards">
        <div v-for="item in prices" :key="`price-${item.tld}`" class="art-card contact-card">
          <div class="contact-card__head">
            <div class="contact-card__title">
              <strong>.{{ item.tld }}</strong>
              <small>注册 ￥{{ item.registerPrice }} / 年 · 续费 ￥{{ item.renewPrice }} / 年</small>
            </div>
          </div>
          <div class="contact-card__meta">
            <span>可注册年限：{{ item.minYears }} ~ {{ item.maxYears }} 年</span>
            <span>币种：{{ item.currency }}</span>
          </div>
        </div>
        <el-empty v-if="!pricesLoading && !prices.length" description="暂未配置价目" />
      </div>
    </div>

    <!-- 注册域名弹窗 -->
    <el-dialog v-model="registerDialogVisible" title="注册域名" width="520px">
      <el-form label-width="96px" label-position="right">
        <el-form-item label="域名">
          <el-input :model-value="registerForm.domain" disabled />
        </el-form-item>
        <el-form-item label="注册年限" required>
          <el-input-number v-model="registerForm.years" :min="1" :max="10" style="width: 140px" />
        </el-form-item>
        <el-form-item label="应付金额" required>
          <div v-if="registerAmountText">
            <span class="quote-price">￥{{ registerAmountText }}</span>
            <span class="search-tip">（{{ registerForm.years }} 年，按价目表计算，提交后从账户余额扣减）</span>
          </div>
          <div v-else class="search-tip">该后缀暂未上架，无法自助注册</div>
        </el-form-item>
        <el-form-item v-if="registerQuote?.premium" label="溢价提示">
          <el-alert
            type="warning"
            :closable="false"
            show-icon
            title="该域名为溢价域名，前台无法自助注册"
            description="请联系管理员确认溢价价格后由后台代注册。"
          />
        </el-form-item>
        <el-form-item label="WHOIS 联系人">
          <el-select v-model="registerForm.contactId" placeholder="默认联系人" clearable style="width: 100%">
            <el-option v-for="c in contacts" :key="c.id" :value="c.id" :label="`${c.name} · ${c.email}`" />
          </el-select>
          <div class="form-tip">需先在「我的联系人」中创建</div>
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="registerDialogVisible = false">取消</el-button>
        <el-button type="primary" :loading="registerSubmitting" @click="submitRegister">提交注册</el-button>
      </template>
    </el-dialog>

    <!-- 续费弹窗 -->
    <el-dialog v-model="renewDialogVisible" title="续费域名" width="420px">
      <el-form label-width="96px" label-position="right">
        <el-form-item label="域名">
          <span>{{ renewTarget?.domain }}</span>
        </el-form-item>
        <el-form-item label="到期时间">
          <span>{{ renewTarget?.expiresAt || '—' }}</span>
        </el-form-item>
        <el-form-item label="续费年限" required>
          <el-input-number v-model="renewForm.years" :min="1" :max="10" style="width: 140px" />
        </el-form-item>
      </el-form>
      <div class="form-tip">按价目表续费价计算，提交后从账户余额扣减。</div>
      <template #footer>
        <el-button @click="renewDialogVisible = false">取消</el-button>
        <el-button type="primary" :loading="renewSubmitting" @click="submitRenew">提交续费</el-button>
      </template>
    </el-dialog>

    <!-- 联系人弹窗 -->
    <el-dialog
      v-model="contactDialogVisible"
      :title="contactForm.id ? '编辑联系人' : '新增联系人'"
      width="560px"
    >
      <el-form :model="contactForm" label-width="96px" label-position="right">
        <el-form-item label="名" required>
          <el-input v-model="contactForm.firstName" maxlength="50" />
        </el-form-item>
        <el-form-item label="姓" required>
          <el-input v-model="contactForm.lastName" maxlength="50" />
        </el-form-item>
        <el-form-item label="组织">
          <el-input v-model="contactForm.organization" maxlength="100" />
        </el-form-item>
        <el-form-item label="邮箱" required>
          <el-input v-model="contactForm.email" maxlength="100" />
        </el-form-item>
        <el-form-item label="地址" required>
          <el-input v-model="contactForm.address1" maxlength="200" />
        </el-form-item>
        <el-form-item label="地址 2">
          <el-input v-model="contactForm.address2" maxlength="200" />
        </el-form-item>
        <el-form-item label="城市" required>
          <el-input v-model="contactForm.city" maxlength="100" />
        </el-form-item>
        <el-form-item label="省/州">
          <el-input v-model="contactForm.stateProvince" maxlength="100" />
        </el-form-item>
        <el-form-item label="邮编">
          <el-input v-model="contactForm.postalCode" maxlength="20" />
        </el-form-item>
        <el-form-item label="国家" required>
          <el-select v-model="contactForm.country" style="width: 100%">
            <el-option v-for="c in COUNTRY_OPTIONS" :key="c.value" :value="c.value" :label="c.label" />
          </el-select>
        </el-form-item>
        <el-form-item label="电话" required>
          <el-input v-model="contactForm.phone" placeholder="+86.13800138000" maxlength="30">
            <template #prepend>格式</template>
          </el-input>
          <div class="form-tip">Spaceship 要求 +国际区号.号码，中间是英文句点</div>
        </el-form-item>
        <el-form-item label="设为默认">
          <el-switch v-model="contactForm.isDefault" />
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="contactDialogVisible = false">取消</el-button>
        <el-button type="primary" :loading="contactSaving" @click="submitContact">保存</el-button>
      </template>
    </el-dialog>
  </div>
</template>

<style scoped>
.spaceship-tabs {
  margin-bottom: 16px;
  display: flex;
  gap: 4px;
  border-bottom: 1px solid var(--art-gray-200, #e4e7ed);
}

.spaceship-tab {
  padding: 10px 20px;
  background: none;
  border: none;
  border-bottom: 2px solid transparent;
  cursor: pointer;
  font-size: 14px;
  color: var(--art-gray-600, #606266);
  transition: all 0.2s;
  margin-bottom: -1px;
}

.spaceship-tab:hover {
  color: var(--art-primary, #409eff);
}

.spaceship-tab.active {
  color: var(--art-primary, #409eff);
  border-bottom-color: var(--art-primary, #409eff);
  font-weight: 600;
}

.search-actions {
  display: flex;
  align-items: center;
  gap: 12px;
  margin-top: 12px;
}

.search-tip,
.toolbar-tip {
  margin: 0;
  font-size: 12px;
  color: var(--art-gray-500, #909399);
  line-height: 1.6;
}

.check-results {
  display: flex;
  flex-direction: column;
  gap: 10px;
  margin-top: 16px;
}

.check-item {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  padding: 12px 16px;
}

.check-item__info {
  display: flex;
  align-items: center;
  gap: 10px;
  min-width: 0;
}

.check-item__info strong {
  overflow: hidden;
  color: var(--art-gray-800);
  font-size: 14px;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.check-item__tip {
  font-size: 12px;
  color: var(--art-gray-500, #909399);
}

.check-item__price {
  font-size: 13px;
  font-weight: 600;
  color: var(--art-primary, #409eff);
}

.quote-price {
  font-size: 18px;
  font-weight: 600;
  color: var(--art-primary, #409eff);
}

.stats-row {
  margin-bottom: 18px;
}

.stats-row .el-col {
  margin-bottom: 12px;
}

.toolbar {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 16px;
  margin-bottom: 12px;
}

.domain-cards,
.contact-cards {
  display: flex;
  flex-direction: column;
  gap: 12px;
}

.domain-card,
.contact-card {
  padding: 16px;
  border-radius: var(--radius-lg);
}

.domain-card__head,
.contact-card__head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 10px;
}

.domain-card__title,
.contact-card__title {
  display: flex;
  min-width: 0;
  flex-direction: column;
}

.domain-card__title strong,
.contact-card__title strong {
  overflow: hidden;
  color: var(--art-gray-800);
  font-size: 14px;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.domain-card__title small,
.contact-card__title small {
  color: var(--art-gray-500, #909399);
  font-size: 11px;
}

.domain-card__meta,
.contact-card__meta {
  display: flex;
  flex-wrap: wrap;
  gap: 6px 14px;
  margin-top: 8px;
  color: var(--art-gray-500, #909399);
  font-size: 12px;
}

.domain-card__actions,
.contact-card__actions {
  display: flex;
  gap: 12px;
  margin-top: 8px;
}

.row-actions {
  display: flex;
  flex-wrap: wrap;
  gap: 4px;
}

.form-tip {
  margin-top: 4px;
  color: var(--art-gray-500, #909399);
  font-size: 12px;
  line-height: 1.5;
}

@media (max-width: 640px) {
  :deep(.el-dialog) {
    width: calc(100% - 24px) !important;
    min-width: 0;
  }
}
</style>
