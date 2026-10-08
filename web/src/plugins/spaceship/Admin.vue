<script setup lang="ts">
import { computed, h, onMounted, reactive, ref } from 'vue'
import { ElMessage, ElMessageBox, ElButton, ElTag } from 'element-plus'
import type { ColumnOption } from '@/types'
import ArtStatsCard from '@/components/core/cards/art-stats-card/index.vue'
import { http } from '@/http'
import { useAdminRequest } from '@/admin/useAdminTable'

interface ContactRow {
  id: number
  contactId: string
  label?: string
  name: string
  email: string
  country: string
  phone: string
  isDefault: boolean
  shared?: boolean // user_id 为空：管理员共享模板，对所有用户可见
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
  id: number
  contactId: string
  label: string
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
  isDefault: boolean
}

interface DomainRow {
  id: number
  userId: number
  domain: string
  years: number
  paidAmountCents: number
  status: string
  privacyLevel: string
  autoRenew: boolean
  spaceshipDomainId?: string
  registeredAt?: string
  expiresAt?: string
}

interface OperationRow {
  id: number
  operationId: string
  domain: string
  opType: string
  status: string
  error?: string
  startedAt?: string
}

interface PriceRow {
  id: number
  tld: string
  registerCents: number
  renewCents: number
  currency: string
  enabled: boolean
  minYears: number
  maxYears: number
  registerPrice: string
  renewPrice: string
}

interface CheckItem {
  domain: string
  result: string
  available: boolean
  isPremium: boolean
  needConfirm: boolean
  premiumPrice?: number
  currency?: string
  listPriceCents?: number
  listPrice?: string
  tld?: string
}

const STATUS_TAG: Record<string, 'info' | 'warning' | 'success' | 'danger'> = {
  pending: 'warning',
  active: 'success',
  failed: 'danger',
  stuck: 'danger',
  deleted: 'info',
}

const OP_STATUS_TAG: Record<string, 'info' | 'warning' | 'success' | 'danger'> = {
  pending: 'warning',
  success: 'success',
  failed: 'danger',
}

const OP_TYPE_LABELS: Record<string, string> = {
  domain_create: '域名注册',
  domain_renew: '域名续费',
  nameservers_update: 'NS 修改',
  autorenew_update: '自动续费',
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
    contactId: '',
    label: '',
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
    isDefault: false,
  }
}

function newPriceForm() {
  return { tld: '', registerPrice: '', renewPrice: '', currency: 'CNY', enabled: true, minYears: 1, maxYears: 10 }
}

const activeTab = ref<'domains' | 'contacts' | 'operations' | 'prices'>('domains')

// ---- 域名管理 ----
const startDomainsRequest = useAdminRequest()
const domainsLoading = ref(false)
const domains = ref<DomainRow[]>([])

const domainStats = computed(() => ({
  total: domains.value.length,
  active: domains.value.filter((d) => d.status === 'active').length,
  pending: domains.value.filter((d) => d.status === 'pending').length,
  failed: domains.value.filter((d) => d.status === 'failed' || d.status === 'stuck').length,
}))

const registerDialogVisible = ref(false)
const registerSubmitting = ref(false)
const registerChecking = ref(false)
const registerForm = reactive({
  domain: '',
  years: 1,
  userId: undefined as number | undefined,
  contactId: 0,
  allowPremium: false,
})
// 服务端实时报价（金额不再由后台手填，一律由价目表决定）
const registerQuote = ref<{ amount: string; amountCents: number; premium: boolean; premiumPrice?: number; currency?: string; available: boolean; result: string } | null>(null)

// 按当前域名向服务端询价（后台价目表 + SpaceShip 溢价标记）
async function quoteRegister() {
  const domain = registerForm.domain.trim().toLowerCase()
  if (!domain) {
    registerQuote.value = null
    return
  }
  registerChecking.value = true
  try {
    const res = await http.post<{ ok: number; msg?: string; item?: CheckItem }>('/plugin/spaceship/check', {
      domain,
    })
    if (String(res.ok) !== '1' || !res.item) {
      registerQuote.value = null
      ElMessage.warning(res.msg || '询价失败，请检查 Spaceship API 配置')
      return
    }
    registerQuote.value = {
      amount: res.item.listPrice || '',
      amountCents: res.item.listPriceCents || 0,
      premium: !!res.item.isPremium,
      premiumPrice: res.item.premiumPrice,
      currency: res.item.currency,
      available: !!res.item.available,
      result: res.item.result || '',
    }
    if (res.item.isPremium) {
      registerForm.allowPremium = false
    }
  } catch (err: unknown) {
    registerQuote.value = null
    // 询价失败不能静默：否则管理员以为域名可用，直接提交会被后端拒绝
    ElMessage.warning((err as Error).message || '询价失败，请检查 Spaceship API 配置')
  } finally {
    registerChecking.value = false
  }
}

async function loadDomains() {
  const isCurrent = startDomainsRequest()
  if (!isCurrent) return
  domainsLoading.value = true
  try {
    const res = await http.get<{ ok: number; list?: DomainRow[] }>('/plugin/spaceship/domains')
    if (!isCurrent()) return
    domains.value = res.list || []
  } catch (err: unknown) {
    if (isCurrent()) ElMessage.error((err as Error).message || '读取域名列表失败')
  } finally {
    if (isCurrent()) domainsLoading.value = false
  }
}

function openRegisterDialog() {
  registerForm.domain = ''
  registerForm.years = 1
  registerForm.userId = undefined
  registerForm.contactId = contacts.value[0]?.id ?? 0
  registerForm.allowPremium = false
  registerQuote.value = null
  registerDialogVisible.value = true
}

async function submitRegister() {
  if (!registerForm.domain.trim()) {
    ElMessage.warning('请输入域名')
    return
  }
  if (!registerForm.userId || registerForm.userId <= 0) {
    ElMessage.warning('请输入归属用户 ID')
    return
  }
  if (!registerForm.contactId) {
    ElMessage.warning('请先创建联系人')
    return
  }
  if (registerQuote.value?.premium && !registerForm.allowPremium) {
    ElMessage.warning('该域名为溢价域名，请先确认溢价价格后再代注册')
    return
  }
  registerSubmitting.value = true
  try {
    const res = await http.post<{ ok: number; operationId?: string; msg?: string }>(
      '/plugin/spaceship/domains/register',
      {
        domain: registerForm.domain.trim(),
        years: registerForm.years,
        userId: registerForm.userId,
        contactId: registerForm.contactId,
        allowPremium: registerForm.allowPremium,
      }
    )
    if (String(res.ok) !== '1') {
      ElMessage.error(res.msg || '注册失败')
      return
    }
    ElMessage.success('注册请求已提交，等待 Spaceship 异步处理')
    registerDialogVisible.value = false
    loadDomains()
    loadOperations()
  } catch (err: unknown) {
    ElMessage.error((err as Error).message || '注册失败')
  } finally {
    registerSubmitting.value = false
  }
}

// ---- 后台代续费 ----
const renewDialogVisible = ref(false)
const renewSubmitting = ref(false)
const renewTarget = ref<DomainRow | null>(null)
const renewForm = reactive({ years: 1 })

function openRenewDialog(row: DomainRow) {
  renewTarget.value = row
  renewForm.years = 1
  renewDialogVisible.value = true
}

async function submitRenew() {
  if (!renewTarget.value) return
  renewSubmitting.value = true
  try {
    const res = await http.post<{ ok: number; msg?: string }>(
      `/plugin/spaceship/domains/${renewTarget.value.id}/renew`,
      { years: renewForm.years }
    )
    if (String(res.ok) !== '1') {
      ElMessage.error(res.msg || '续费失败')
      return
    }
    ElMessage.success('续费请求已提交')
    renewDialogVisible.value = false
    loadDomains()
    loadOperations()
  } catch (err: unknown) {
    ElMessage.error((err as Error).message || '续费失败')
  } finally {
    renewSubmitting.value = false
  }
}

async function toggleAutoRenew(row: DomainRow, val: boolean) {
  try {
    const res = await http.post<{ ok: number; msg?: string }>(
      `/plugin/spaceship/domains/${row.id}/autorenew`,
      { enable: val }
    )
    if (String(res.ok) !== '1') {
      ElMessage.error(res.msg || '设置失败')
      return
    }
    ElMessage.success(val ? '已开启自动续费' : '已关闭自动续费')
  } catch (err: unknown) {
    ElMessage.error((err as Error).message || '设置失败')
  } finally {
    loadDomains()
  }
}

async function deleteDomain(row: DomainRow) {
  const ok = await ElMessageBox.confirm(
    `确认删除域名「${row.domain}」？仅在本地标记删除，不会取消 Spaceship 侧注册。`,
    '删除确认',
    { type: 'warning', confirmButtonText: '删除', cancelButtonText: '取消' }
  ).catch(() => null)
  if (!ok) return
  try {
    const res = await http.post<{ ok: number; msg?: string }>(`/plugin/spaceship/domains/${row.id}/delete`)
    if (String(res.ok) !== '1') {
      ElMessage.error(res.msg || '删除失败')
      return
    }
    ElMessage.success('已标记删除')
    loadDomains()
  } catch (err: unknown) {
    ElMessage.error((err as Error).message || '删除失败')
  }
}

const domainColumns = ref<ColumnOption<DomainRow>[]>([
  { prop: 'id', label: 'ID', width: 70, sortable: true },
  { prop: 'domain', label: '域名', minWidth: 180, sortable: true },
  {
    prop: 'userId',
    label: '用户',
    width: 90,
    formatter: (row) => `#${row.userId}`,
  },
  { prop: 'years', label: '年限', width: 70, formatter: (row) => `${row.years} 年` },
  {
    prop: 'paidAmountCents',
    label: '实付',
    width: 100,
    formatter: (row) => `¥${(row.paidAmountCents / 100).toFixed(2)}`,
  },
  {
    prop: 'status',
    label: '状态',
    width: 100,
    formatter: (row) => h(ElTag, { type: STATUS_TAG[row.status] || 'info', effect: 'light' }, row.status),
  },
  {
    prop: 'autoRenew',
    label: '自动续费',
    width: 100,
    formatter: (row) =>
      h(ElTag, { type: row.autoRenew ? 'success' : 'info', effect: 'light' }, row.autoRenew ? '已开启' : '未开启'),
  },
  {
    prop: 'registeredAt',
    label: '注册时间',
    width: 160,
    formatter: (row) => (row.registeredAt ? row.registeredAt.slice(0, 10) : '—'),
  },
  {
    prop: 'expiresAt',
    label: '到期时间',
    width: 160,
    formatter: (row) => (row.expiresAt ? row.expiresAt.slice(0, 10) : '—'),
  },
  {
    prop: 'actions',
    label: '操作',
    width: 250,
    fixed: 'right',
    formatter: (row) =>
      h('div', { class: 'row-actions' }, [
        h(
          ElButton,
          {
            size: 'small',
            link: true,
            type: 'primary',
            disabled: row.status !== 'active',
            onClick: () => openRenewDialog(row),
          },
          '续费'
        ),
        h(
          ElButton,
          {
            size: 'small',
            link: true,
            type: row.autoRenew ? 'warning' : 'success',
            disabled: row.status !== 'active',
            onClick: () => toggleAutoRenew(row, !row.autoRenew),
          },
          row.autoRenew ? '关自动续费' : '开自动续费'
        ),
        h(
          ElButton,
          { size: 'small', link: true, type: 'danger', onClick: () => deleteDomain(row) },
          '删除'
        ),
      ]),
  },
])

// ---- 联系人管理 ----
const startContactsRequest = useAdminRequest()
const contactsLoading = ref(false)
const contacts = ref<ContactRow[]>([])
const contactDialogVisible = ref(false)
const contactSaving = ref(false)
const contactForm = ref<ContactForm>(newContactForm())

async function loadContacts() {
  const isCurrent = startContactsRequest()
  if (!isCurrent) return
  contactsLoading.value = true
  try {
    const res = await http.get<{ ok: number; list?: ContactRow[] }>('/plugin/spaceship/contacts')
    if (!isCurrent()) return
    contacts.value = res.list || []
  } catch (err: unknown) {
    if (isCurrent()) ElMessage.error((err as Error).message || '读取联系人失败')
  } finally {
    if (isCurrent()) contactsLoading.value = false
  }
}

function openContactDialog(row?: ContactRow) {
  if (row) {
    // 编辑：带上本地 id 让后端复用上游 contactId，其余字段用列表返回的原值回填
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
    const res = await http.post<{ ok: number; id?: number; msg?: string }>('/plugin/spaceship/contacts', {
      existingId: f.id > 0 ? f.id : 0, // 传入本地 id 表示更新原联系人（复用上游 contactId）
      label: f.label.trim() || `${f.firstName} ${f.lastName}`,
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
      isDefault: f.isDefault,
    })
    if (String(res.ok) !== '1') {
      ElMessage.error(res.msg || '保存失败')
      return
    }
    ElMessage.success('已保存到 Spaceship')
    contactDialogVisible.value = false
    loadContacts()
  } catch (err: unknown) {
    ElMessage.error((err as Error).message || '保存失败')
  } finally {
    contactSaving.value = false
  }
}

async function setDefaultContact(row: ContactRow) {
  try {
    const res = await http.post<{ ok: number; msg?: string }>(`/plugin/spaceship/contacts/${row.id}/default`)
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

async function deleteContact(row: ContactRow) {
  const ok = await ElMessageBox.confirm(`确认删除联系人「${row.name}」？`, '删除确认', {
    type: 'warning',
    confirmButtonText: '删除',
    cancelButtonText: '取消',
  }).catch(() => null)
  if (!ok) return
  try {
    const res = await http.delete<{ ok: number; msg?: string }>(`/plugin/spaceship/contacts/${row.id}`)
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

const contactColumns = ref<ColumnOption<ContactRow>[]>([
  { prop: 'id', label: 'ID', width: 70, sortable: true },
  { prop: 'name', label: '姓名', minWidth: 140 },
  { prop: 'email', label: '邮箱', minWidth: 200 },
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
        ? h(ElTag, { type: 'warning', effect: 'light', size: 'small' }, '共享模板')
        : h(ElTag, { type: 'info', effect: 'light', size: 'small' }, '用户自建'),
  },
  {
    prop: 'contactId',
    label: 'Spaceship ID',
    minWidth: 160,
    formatter: (row) => h('span', { class: 'text-g-500 text-xs' }, row.contactId),
  },
  {
    prop: 'actions',
    label: '操作',
    width: 220,
    fixed: 'right',
    formatter: (row) =>
      h('div', { class: 'row-actions' }, [
        h(
          ElButton,
          { size: 'small', link: true, type: 'primary', onClick: () => openContactDialog(row) },
          '编辑'
        ),
        h(
          ElButton,
          { size: 'small', link: true, type: 'primary', onClick: () => setDefaultContact(row) },
          '设默认'
        ),
      ]),
  },
])

// ---- 价目表（商业运营定价） ----
const startPricesRequest = useAdminRequest()
const pricesLoading = ref(false)
const prices = ref<PriceRow[]>([])
const priceDialogVisible = ref(false)
const priceSaving = ref(false)
const priceEditing = ref(false)
const priceForm = ref(newPriceForm())

async function loadPrices() {
  const isCurrent = startPricesRequest()
  if (!isCurrent) return
  pricesLoading.value = true
  try {
    const res = await http.get<{ ok: number; list?: PriceRow[] }>('/plugin/spaceship/prices')
    if (!isCurrent()) return
    prices.value = res.list || []
  } catch (err: unknown) {
    if (isCurrent()) ElMessage.error((err as Error).message || '读取价目表失败')
  } finally {
    if (isCurrent()) pricesLoading.value = false
  }
}

function openPriceDialog(row?: PriceRow) {
  priceEditing.value = !!row
  if (row) {
    priceForm.value = {
      tld: row.tld,
      registerPrice: row.registerPrice,
      renewPrice: row.renewPrice,
      currency: row.currency,
      enabled: row.enabled,
      minYears: row.minYears,
      maxYears: row.maxYears,
    }
  } else {
    priceForm.value = newPriceForm()
  }
  priceDialogVisible.value = true
}

async function submitPrice() {
  const f = priceForm.value
  if (!f.tld.trim()) {
    ElMessage.warning('请输入后缀（如 com）')
    return
  }
  if (!f.registerPrice.trim() || !f.renewPrice.trim()) {
    ElMessage.warning('请输入注册价与续费价')
    return
  }
  priceSaving.value = true
  try {
    const res = await http.post<{ ok: number; msg?: string }>('/plugin/spaceship/prices', {
      tld: f.tld.trim().replace(/^\./, '').toLowerCase(),
      registerPrice: f.registerPrice.trim(),
      renewPrice: f.renewPrice.trim(),
      currency: f.currency,
      enabled: f.enabled,
      minYears: f.minYears,
      maxYears: f.maxYears,
    })
    if (String(res.ok) !== '1') {
      ElMessage.error(res.msg || '保存失败')
      return
    }
    ElMessage.success('价目已保存，前台立即生效')
    priceDialogVisible.value = false
    loadPrices()
  } catch (err: unknown) {
    ElMessage.error((err as Error).message || '保存失败')
  } finally {
    priceSaving.value = false
  }
}

async function deletePrice(row: PriceRow) {
  const ok = await ElMessageBox.confirm(
    `确认删除后缀「.${row.tld}」的价目？删除后该后缀不可自助注册。`,
    '删除确认',
    { type: 'warning', confirmButtonText: '删除', cancelButtonText: '取消' }
  ).catch(() => null)
  if (!ok) return
  try {
    const res = await http.delete<{ ok: number; msg?: string }>(`/plugin/spaceship/prices/${row.tld}`)
    if (String(res.ok) !== '1') {
      ElMessage.error(res.msg || '删除失败')
      return
    }
    ElMessage.success('已删除')
    loadPrices()
  } catch (err: unknown) {
    ElMessage.error((err as Error).message || '删除失败')
  }
}

const priceColumns = ref<ColumnOption<PriceRow>[]>([
  { prop: 'tld', label: '后缀', width: 110, formatter: (row) => `.${row.tld}` },
  { prop: 'registerPrice', label: '注册价', width: 120, formatter: (row) => `¥${row.registerPrice}` },
  { prop: 'renewPrice', label: '续费价', width: 120, formatter: (row) => `¥${row.renewPrice}` },
  { prop: 'currency', label: '币种', width: 80 },
  { prop: 'minYears', label: '最少年限', width: 100, formatter: (row) => `${row.minYears} 年` },
  { prop: 'maxYears', label: '最多年限', width: 100, formatter: (row) => `${row.maxYears} 年` },
  {
    prop: 'enabled',
    label: '上架',
    width: 90,
    formatter: (row) =>
      h(ElTag, { type: row.enabled ? 'success' : 'info', effect: 'light', size: 'small' }, row.enabled ? '在售' : '下架'),
  },
  {
    prop: 'actions',
    label: '操作',
    width: 160,
    fixed: 'right',
    formatter: (row) =>
      h('div', { class: 'row-actions' }, [
        h(
          ElButton,
          { size: 'small', link: true, type: 'primary', onClick: () => openPriceDialog(row) },
          '编辑'
        ),
        h(
          ElButton,
          { size: 'small', link: true, type: 'danger', onClick: () => deletePrice(row) },
          '删除'
        ),
      ]),
  },
])

// ---- 操作日志 ----
const startOpsRequest = useAdminRequest()
const opsLoading = ref(false)
const operations = ref<OperationRow[]>([])

async function loadOperations() {
  const isCurrent = startOpsRequest()
  if (!isCurrent) return
  opsLoading.value = true
  try {
    const res = await http.get<{ ok: number; items?: OperationRow[]; total?: number }>(
      '/plugin/spaceship/operations',
      { limit: 50 }
    )
    if (!isCurrent()) return
    operations.value = res.items || []
  } catch (err: unknown) {
    if (isCurrent()) ElMessage.error((err as Error).message || '读取操作日志失败')
  } finally {
    if (isCurrent()) opsLoading.value = false
  }
}

async function retryOperation(row: OperationRow) {
  try {
    const res = await http.post<{ ok: number; msg?: string }>(`/plugin/spaceship/operations/${row.id}/retry`)
    if (String(res.ok) !== '1') {
      ElMessage.error(res.msg || '重试失败')
      return
    }
    ElMessage.success('已触发轮询')
    loadOperations()
    loadDomains()
  } catch (err: unknown) {
    ElMessage.error((err as Error).message || '重试失败')
  }
}

const opColumns = ref<ColumnOption<OperationRow>[]>([
  { prop: 'id', label: 'ID', width: 70, sortable: true },
  { prop: 'operationId', label: 'Spaceship 操作 ID', minWidth: 220 },
  { prop: 'domain', label: '域名', minWidth: 160 },
  {
    prop: 'opType',
    label: '类型',
    width: 110,
    formatter: (row) => OP_TYPE_LABELS[row.opType] || row.opType,
  },
  {
    prop: 'status',
    label: '状态',
    width: 100,
    formatter: (row) =>
      h(ElTag, { type: OP_STATUS_TAG[row.status] || 'info', effect: 'light' }, row.status),
  },
  {
    prop: 'error',
    label: '结果说明',
    minWidth: 200,
    formatter: (row) => (row.error ? h('span', { class: 'text-danger' }, row.error) : '—'),
  },
  { prop: 'startedAt', label: '提交时间', width: 170, sortable: true },
  {
    prop: 'actions',
    label: '操作',
    width: 110,
    fixed: 'right',
    formatter: (row) =>
      h(
        ElButton,
        { size: 'small', link: true, type: 'primary', onClick: () => retryOperation(row) },
        '重新轮询'
      ),
  },
])

// ---- 连接测试 ----
const testing = ref(false)

async function testConnection() {
  testing.value = true
  try {
    const res = await http.post<{ ok: number; msg?: string }>('/plugin/spaceship/client/test')
    if (String(res.ok) !== '1') {
      ElMessage.error(res.msg || '连接失败')
      return
    }
    ElMessage.success(res.msg || '连接正常')
  } catch (err: unknown) {
    ElMessage.error((err as Error).message || '连接失败')
  } finally {
    testing.value = false
  }
}

function onTabChange(tab: string | number) {
  activeTab.value = tab as 'domains' | 'contacts' | 'operations' | 'prices'
  if (tab === 'contacts' && contacts.value.length === 0) loadContacts()
  if (tab === 'operations' && operations.value.length === 0) loadOperations()
  if (tab === 'prices' && prices.value.length === 0) loadPrices()
}

onMounted(() => {
  loadDomains()
  loadContacts()
})
</script>

<template>
  <div class="art-full-height">
    <ElCard class="art-card">
      <template #header>
        <div class="art-card-header">
          <div class="title">
            <h4>域名注册</h4>
            <p>管理 Spaceship 域名：注册新域名、维护 WHOIS 联系人、查看异步注册结果。</p>
          </div>
          <el-button link type="primary" :loading="testing" @click="testConnection">测试 Spaceship 连接</el-button>
        </div>
      </template>

      <div class="spaceship-tabs">
        <button
          class="spaceship-tab"
          :class="{ active: activeTab === 'domains' }"
          @click="activeTab = 'domains'"
        >域名管理</button>
        <button
          class="spaceship-tab"
          :class="{ active: activeTab === 'contacts' }"
          @click="onTabChange('contacts')"
        >联系人</button>
        <button
          class="spaceship-tab"
          :class="{ active: activeTab === 'operations' }"
          @click="onTabChange('operations')"
        >操作日志</button>
        <button
          class="spaceship-tab"
          :class="{ active: activeTab === 'prices' }"
          @click="onTabChange('prices')"
        >价目表</button>
      </div>

      <!-- 域名管理 -->
      <div v-show="activeTab === 'domains'">
        <ElRow :gutter="20" class="mb-5">
          <ElCol :xs="24" :sm="6">
            <ArtStatsCard
              icon="ri:global-line"
              icon-style="bg-primary"
              title="域名总数"
              :count="domainStats.total"
              description="本地登记的域名"
            />
          </ElCol>
          <ElCol :xs="24" :sm="6">
            <ArtStatsCard
              icon="ri:checkbox-circle-line"
              icon-style="bg-success"
              title="注册成功"
              :count="domainStats.active"
              description="Spaceship 侧已生效"
            />
          </ElCol>
          <ElCol :xs="24" :sm="6">
            <ArtStatsCard
              icon="ri:time-line"
              icon-style="bg-warning"
              title="处理中"
              :count="domainStats.pending"
              description="等待异步注册结果"
            />
          </ElCol>
          <ElCol :xs="24" :sm="6">
            <ArtStatsCard
              icon="ri:close-circle-line"
              icon-style="bg-danger"
              title="失败"
              :count="domainStats.failed"
              description="需到操作日志排查"
            />
          </ElCol>
        </ElRow>

        <div class="toolbar">
          <p class="toolbar-tip">
            注册为异步操作：提交后由定时任务轮询 Spaceship 结果（通常 1-5 分钟），请保持 API Key 有效。
          </p>
          <el-button type="primary" @click="openRegisterDialog">注册域名</el-button>
        </div>

        <ElCard class="art-table-card" :style="{ marginTop: '12px' }">
          <ArtTableHeader
            :loading="domainsLoading"
            @refresh="loadDomains"
          />
          <ArtTable
            :loading="domainsLoading"
            :data="domains"
            :columns="domainColumns"
            empty-text="暂无域名，点击「注册域名」开始"
          />
        </ElCard>
      </div>

      <!-- 联系人 -->
      <div v-show="activeTab === 'contacts'">
        <div class="toolbar">
          <p class="toolbar-tip">
            WHOIS 联系人由插件托管：创建后会同步保存到 Spaceship（SaveContact upsert），注册域名时选用。
          </p>
          <el-button type="primary" @click="openContactDialog()">新增联系人</el-button>
        </div>
        <ElCard class="art-table-card" :style="{ marginTop: '12px' }">
          <ArtTableHeader
            :loading="contactsLoading"
            @refresh="loadContacts"
          />
          <ArtTable
            :loading="contactsLoading"
            :data="contacts"
            :columns="contactColumns"
            empty-text="暂无联系人"
          />
        </ElCard>
      </div>

      <!-- 操作日志 -->
      <div v-show="activeTab === 'operations'">
        <ElCard class="art-table-card">
          <ArtTableHeader
            :loading="opsLoading"
            @refresh="loadOperations"
          />
          <ArtTable
            :loading="opsLoading"
            :data="operations"
            :columns="opColumns"
            empty-text="暂无操作记录"
          />
        </ElCard>
      </div>

      <!-- 价目表 -->
      <div v-show="activeTab === 'prices'">
        <div class="toolbar">
          <p class="toolbar-tip">
            价格只在这里维护：前台注册/续费一律按此价目表从用户余额扣款，客户端传价一律忽略。未上架或未配置的后缀不可注册。
          </p>
          <el-button type="primary" @click="openPriceDialog()">新增价目</el-button>
        </div>
        <ElCard class="art-table-card" :style="{ marginTop: '12px' }">
          <ArtTableHeader
            :loading="pricesLoading"
            @refresh="loadPrices"
          />
          <ArtTable
            :loading="pricesLoading"
            :data="prices"
            :columns="priceColumns"
            empty-text="暂未配置价目，新增后前台才可注册"
          />
        </ElCard>
      </div>
    </ElCard>

    <!-- 注册域名弹窗 -->
    <el-dialog v-model="registerDialogVisible" title="注册域名" width="520px">
      <el-form label-width="110px" label-position="right">
        <el-form-item label="域名" required>
          <el-input
            v-model="registerForm.domain"
            placeholder="example.com"
            maxlength="253"
            @blur="quoteRegister"
          />
        </el-form-item>
        <el-form-item label="服务端报价">
          <div v-if="registerChecking" class="text-g-500 text-xs">正在询价…</div>
          <div v-else-if="registerQuote">
            <span class="quote-price">¥{{ registerQuote.amount }}</span>
            <span class="text-g-500 ml-2 text-xs">
              {{ registerQuote.available ? '可注册' : `不可注册（${registerQuote.result}）` }}
            </span>
          </div>
          <div v-else class="text-g-500 text-xs">输入域名后自动按价目表报价</div>
        </el-form-item>
        <el-form-item v-if="registerQuote?.premium" label="溢价确认">
          <el-alert
            type="warning"
            :closable="false"
            show-icon
            :title="`该域名为溢价域名，上游报价 ${registerQuote.premiumPrice ?? '—'} ${registerQuote.currency || ''}`"
            description="溢价价不受本地价目表控制。确认后按本地价目表扣款，溢价差价请线下结算。"
          />
          <el-checkbox v-model="registerForm.allowPremium" class="mt-2">
            我已确认溢价价格，同意代客户注册
          </el-checkbox>
        </el-form-item>
        <el-form-item label="注册年限" required>
          <el-input-number v-model="registerForm.years" :min="1" :max="10" style="width: 140px" />
          <span class="text-g-500 ml-2">Spaceship 注册为异步操作，提交后需轮询结果</span>
        </el-form-item>
        <el-form-item label="归属用户" required>
          <el-input-number
            v-model="registerForm.userId"
            :min="1"
            :precision="0"
            placeholder="用户 ID"
            style="width: 100%"
          />
          <div class="form-tip">按上方服务端报价从该用户余额扣款（金额不可手填）</div>
        </el-form-item>
        <el-form-item label="WHOIS 联系人" required>
          <el-select v-model="registerForm.contactId" placeholder="选择联系人" style="width: 100%">
            <el-option v-for="c in contacts" :key="c.id" :value="c.id" :label="`${c.name} · ${c.email}`" />
          </el-select>
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="registerDialogVisible = false">取消</el-button>
        <el-button type="primary" :loading="registerSubmitting" @click="submitRegister">提交注册</el-button>
      </template>
    </el-dialog>

    <!-- 续费弹窗 -->
    <el-dialog v-model="renewDialogVisible" title="代客户续费" width="420px">
      <el-form label-width="110px" label-position="right">
        <el-form-item label="域名">
          <span>{{ renewTarget?.domain }}</span>
        </el-form-item>
        <el-form-item label="续费年限" required>
          <el-input-number v-model="renewForm.years" :min="1" :max="10" style="width: 140px" />
        </el-form-item>
      </el-form>
      <div class="form-tip">按价目表续费价从域名归属用户（#{{ renewTarget?.userId }}）余额扣款。</div>
      <template #footer>
        <el-button @click="renewDialogVisible = false">取消</el-button>
        <el-button type="primary" :loading="renewSubmitting" @click="submitRenew">提交续费</el-button>
      </template>
    </el-dialog>

    <!-- 价目弹窗 -->
    <el-dialog
      v-model="priceDialogVisible"
      :title="priceForm.tld ? `编辑 .${priceForm.tld} 价目` : '新增价目'"
      width="460px"
    >
      <el-form :model="priceForm" label-width="110px" label-position="right">
        <el-form-item label="后缀" required>
          <el-input v-model="priceForm.tld" placeholder="com" maxlength="20" :disabled="priceEditing" />
        </el-form-item>
        <el-form-item label="注册价" required>
          <el-input v-model="priceForm.registerPrice" placeholder="如 88.00" maxlength="20">
            <template #prepend>¥</template>
          </el-input>
        </el-form-item>
        <el-form-item label="续费价" required>
          <el-input v-model="priceForm.renewPrice" placeholder="如 88.00" maxlength="20">
            <template #prepend>¥</template>
          </el-input>
        </el-form-item>
        <el-form-item label="币种">
          <el-input v-model="priceForm.currency" placeholder="CNY" maxlength="8" />
        </el-form-item>
        <el-form-item label="年限区间">
          <el-input-number v-model="priceForm.minYears" :min="1" :max="10" size="small" />
          <span class="mx-2">~</span>
          <el-input-number v-model="priceForm.maxYears" :min="1" :max="10" size="small" />
        </el-form-item>
        <el-form-item label="上架销售">
          <el-switch v-model="priceForm.enabled" />
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="priceDialogVisible = false">取消</el-button>
        <el-button type="primary" :loading="priceSaving" @click="submitPrice">保存</el-button>
      </template>
    </el-dialog>

    <!-- 联系人弹窗 -->
    <el-dialog
      v-model="contactDialogVisible"
      :title="contactForm.id ? '编辑联系人' : '新增联系人'"
      width="560px"
    >
      <el-form :model="contactForm" label-width="110px" label-position="right">
        <el-form-item label="备注名">
          <el-input v-model="contactForm.label" placeholder="如「默认联系人」" maxlength="50" />
        </el-form-item>
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

.toolbar {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 16px;
}

.toolbar-tip {
  margin: 0;
  font-size: 13px;
  color: var(--art-gray-500, #909399);
  line-height: 1.6;
}

.quote-price {
  font-size: 18px;
  font-weight: 600;
  color: var(--art-primary, #409eff);
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

.text-danger {
  color: var(--el-color-danger);
}

@media (max-width: 600px) {
  :deep(.el-dialog) {
    width: calc(100% - 24px) !important;
    min-width: 0;
  }
}
</style>
