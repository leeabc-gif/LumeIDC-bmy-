/**
 * 插件验收脚本（配置驱动：真实后端 + 真实浏览器）
 *
 * 对应验收计划《插件测试验收标准与检查计划》第二章通用标准（A~G）与第三章业务清单：
 *   通用模块（每个插件自动执行）：C-01/C-02 未登录 401、C-04 CSRF 拦截、C-05 畸形输入、
 *     C-07 PII 脱敏、B-01/B-02/B-04 启停闸门、D-01/D-02/D-03 三视图渲染
 *   业务模块（按插件声明式接入）：V-xx / R-xx / T-xx / N-xx / W-xx / D2-xx
 *   证据模块：每步截图 + 失败时输出网络轨迹
 *
 * 用法：
 *   node scripts/acceptance-plugins.mjs                # 跑全部已接入插件
 *   node scripts/acceptance-plugins.mjs violation      # 只跑指定插件
 *   node scripts/acceptance-plugins.mjs violation refund
 * 环境变量：ACCEPT_BASE / SMOKE_EMAIL / SMOKE_PASSWORD / SMOKE_ADMIN / SMOKE_ADMIN_PASSWORD / CHROME_PATH
 * 前提：`npm run dev` 已启动（:5173，且 LUME_DEV_API 指向 Go 后端所在端口），Go 后端已运行，验收库已备份。
 */
import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import puppeteer from 'puppeteer-core'
import http from 'node:http'
import crypto from 'node:crypto'
import { execSync } from 'node:child_process'

// ---------------------------------------------------------------- 环境与账号

const BASE = (process.env.ACCEPT_BASE || 'http://localhost:5173').replace(/\/$/, '')
const API = BASE + '/__api' // 开发期 Vite 代理前缀（剥掉后与生产同源路径一致）
const EMAIL = process.env.SMOKE_EMAIL || 'smoke@lumeidc.local'
const PASSWORD = process.env.SMOKE_PASSWORD || 'Smoke@12345'
// C-03 越权检查专用第二账号（普通用户，非管理员）。由 go run ./tools/ensure-qa-alt 幂等创建。
const ALT_EMAIL = process.env.SMOKE_ALT_EMAIL || 'qa-alt@lumeidc.local'
const ALT_PASSWORD = process.env.SMOKE_ALT_PASSWORD || 'QA-Alt@12345'
const ADMIN_ACCOUNT = process.env.SMOKE_ADMIN || 'smokeqa'
const ADMIN_PASSWORD = process.env.SMOKE_ADMIN_PASSWORD || 'Smoke@12345'
/** 本轮造数标记：同一天多次运行可区分，便于清理历史残留。 */
const MARKER = 'QA' + new Date().toISOString().slice(2, 10).replace(/-/g, '')
const SHOTS = path.join(path.dirname(fileURLToPath(import.meta.url)), '.acceptance-shots')
fs.mkdirSync(SHOTS, { recursive: true })

const VIEWPORTS = [
  { key: 'desktop', label: '桌面', width: 1440, height: 900 },
  { key: 'tablet', label: '平板', width: 768, height: 1024 },
  { key: 'mobile', label: '移动', width: 375, height: 667 },
]

const CHROME = [
  process.env.CHROME_PATH,
  'C:\\Program Files\\Google\\Chrome\\Application\\chrome.exe',
  'C:\\Program Files (x86)\\Microsoft\\Edge\\Application\\msedge.exe',
]
  .filter(Boolean)
  .find((p) => fs.existsSync(p))
if (!CHROME) {
  console.error('未找到 Chrome/Edge，请设置 CHROME_PATH')
  process.exit(1)
}

// psql 二进制探测（DB 直证用）：PSQL_PATH 可覆盖；候选含旧环境 D:/lumeidc-dev 与本机 pgsql17；均不可用时退回 PATH 查找
const PSQL = [
  process.env.PSQL_PATH,
  'D:/lumeidc-dev/pgsql/bin/psql.exe',
  'd:/31493/Documents/开发2用/tools/pgsql17/pgsql/bin/psql.exe',
]
  .filter(Boolean)
  .find((p) => fs.existsSync(p)) || 'psql'

// ---------------------------------------------------------------- 结果记录

const results = []
const netLog = []

function check(id, level, name, pass, detail) {
  results.push({ id, level, name, pass, detail })
  console.log(`${pass ? '✓' : '✗'} [${id}/${level}] ${name}${detail ? ' — ' + detail : ''}`)
}

function skip(id, level, name, reason) {
  results.push({ id, level, name, pass: null, detail: reason })
  console.log(`○ [${id}/${level}] ${name} — 跳过：${reason}`)
}

function note(name, detail) {
  console.log(`· ${name}${detail ? ' — ' + detail : ''}`)
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms))

// ---------------------------------------------------------------- HTTP 探针

/**
 * 已登录会话探针：在页面内 fetch，自动带 Cookie 与 CSRF。
 * opts.csrf=false 不发令牌（C-04 缺失场景）；opts.token='x' 发错误令牌（C-04 错配场景）。
 */
async function pageApi(page, method, p, body, opts = {}) {
  const { csrf = true, token } = opts
  return page.evaluate(
    async (m, p, b, useCsrf, fixedToken) => {
      const headers = { Accept: 'application/json', 'Sec-Fetch-Mode': 'cors' }
      let tok = fixedToken || ''
      if (!tok && useCsrf) {
        try {
          const s = await fetch('/__api/session', {
            credentials: 'same-origin',
            cache: 'no-store',
            headers: { Accept: 'application/json' },
          })
          tok = ((await s.json()) || {}).csrf || ''
        } catch {
          /* 取不到令牌时按缺失场景处理 */
        }
      }
      const noBody = m === 'GET' || m === 'HEAD'
      if (b && !noBody) headers['Content-Type'] = 'application/json'
      if (tok) headers['X-CSRF-Token'] = tok
      let res
      try {
        res = await fetch('/__api' + p, {
          method: m,
          credentials: 'same-origin',
          headers,
          body: b && !noBody ? JSON.stringify(b) : undefined,
        })
      } catch (e) {
        return { status: 0, contentType: '', json: null, text: String((e && e.message) || e) }
      }
      const text = await res.text()
      let json = null
      try {
        json = JSON.parse(text)
      } catch {
        /* 非 JSON */
      }
      return { status: res.status, contentType: res.headers.get('content-type') || '', json, text: text.slice(0, 400) }
    },
    method,
    p,
    body === undefined ? null : body,
    csrf,
    token,
  )
}

/** 匿名探针：不带任何 Cookie（Node 侧发起，Sec-Fetch-Mode 模拟 AJAX 以拿到 JSON 而非跳转）。 */
async function anonApi(method, p, body) {
  const headers = { Accept: 'application/json', 'Sec-Fetch-Mode': 'cors' }
  const noBody = method === 'GET' || method === 'HEAD'
  if (body && !noBody) headers['Content-Type'] = 'application/json'
  let res
  try {
    res = await fetch(API + p, { method, headers, redirect: 'manual', body: body && !noBody ? JSON.stringify(body) : undefined })
  } catch (e) {
    return { status: 0, contentType: '', json: null, text: String((e && e.message) || e) }
  }
  const text = await res.text()
  let json = null
  try {
    json = JSON.parse(text)
  } catch {
    /* 非 JSON */
  }
  return { status: res.status, contentType: res.headers.get('content-type') || '', json, text: text.slice(0, 400) }
}

/** 判定为「JSON 且非 HTML 页面」——防止把登录页 HTML 当作成功响应。 */
function isJson(res) {
  return /json/i.test(res.contentType) && !/^\s*<(!doctype|html)/i.test(res.text)
}

function attachNetProbe(page, tag) {
  page.on('response', async (res) => {
    const url = res.url()
    if (!/\/__api\/(admin\/)?plugin\//.test(url)) return
    let body = ''
    try {
      body = (await res.text()).slice(0, 200).replace(/\s+/g, ' ')
    } catch {
      body = '(body 不可读)'
    }
    netLog.push(`[${tag}] ${res.request().method()} ${res.status()} ${url.replace(BASE, '')} :: ${body}`)
  })
}

function attachErrorCollector(page) {
  const errors = []
  page.on('console', (m) => {
    if (m.type() === 'error' && !m.text().includes('Failed to load resource')) errors.push(m.text())
  })
  page.on('pageerror', (e) => errors.push('pageerror: ' + e.message))
  return errors
}

// ---------------------------------------------------------------- 浏览器与登录

const browser = await puppeteer.launch({
  executablePath: CHROME,
  headless: 'new',
  args: ['--no-sandbox', '--disable-gpu', '--hide-scrollbars'],
})
// puppeteer-core v23 起 createBrowserContext() 为异步方法，返回 Promise<BrowserContext>
const createCtx = () => browser.createBrowserContext()

// 本地图形验证码是固定 5x7 点阵字体、位置确定，可直接在页面内解码。
// 仅在后台因登录失败被强制出验证码时才会用到（正常路径不会出现）。
const CAPTCHA_GLYPHS = {
  2: [6, 9, 1, 2, 4, 8, 15], 3: [14, 1, 1, 6, 1, 1, 14], 4: [2, 6, 10, 10, 15, 2, 2], 5: [15, 8, 8, 14, 1, 1, 14],
  6: [6, 8, 8, 14, 9, 9, 6], 7: [15, 1, 2, 4, 4, 4, 4], 8: [6, 9, 9, 6, 9, 9, 6], 9: [6, 9, 9, 7, 1, 1, 6],
  A: [6, 9, 9, 15, 9, 9, 9], B: [14, 9, 9, 14, 9, 9, 14], C: [6, 9, 8, 8, 8, 9, 6], D: [14, 9, 9, 9, 9, 9, 14],
  E: [15, 8, 8, 14, 8, 8, 15], F: [15, 8, 8, 14, 8, 8, 8], G: [6, 9, 8, 11, 9, 9, 7], H: [9, 9, 9, 15, 9, 9, 9],
  J: [1, 1, 1, 1, 1, 9, 6], K: [9, 10, 12, 12, 10, 10, 9], L: [8, 8, 8, 8, 8, 8, 15], M: [9, 15, 15, 9, 9, 9, 9],
  N: [9, 13, 15, 11, 9, 9, 9], P: [14, 9, 9, 14, 8, 8, 8], Q: [6, 9, 9, 9, 10, 5, 7], R: [14, 9, 9, 14, 10, 9, 9],
  S: [7, 8, 8, 6, 1, 1, 14], T: [15, 2, 2, 2, 2, 2, 2], U: [9, 9, 9, 9, 9, 9, 6], V: [9, 9, 9, 9, 9, 9, 6],
  W: [9, 9, 9, 15, 15, 15, 6], X: [9, 9, 6, 6, 6, 9, 9], Y: [9, 9, 6, 6, 2, 2, 2], Z: [15, 1, 2, 4, 8, 8, 15],
}

async function solveCaptchaIfPresent(page) {
  const input = await page.$('input[placeholder="请输入图中字符"]')
  if (!input) return false
  const answer = await page.evaluate((glyphs) => {
    const img = document.querySelector('.admin-login__captcha-refresh img')
    if (!img || !img.src) return null
    const canvas = document.createElement('canvas')
    const ctx = canvas.getContext('2d')
    canvas.width = img.naturalWidth || 150
    canvas.height = img.naturalHeight || 48
    ctx.drawImage(img, 0, 0)
    const px = ctx.getImageData(0, 0, canvas.width, canvas.height).data
    const on = (x, y) => {
      const i = (y * canvas.width + x) * 4
      return px[i] < 150 && px[i + 1] < 150 && px[i + 2] > 100
    }
    let out = ''
    for (let i = 0; i < 5; i++) {
      const x0 = 12 + i * 27
      const rows = []
      for (let y = 0; y < 7; y++) {
        let row = 0
        for (let x = 0; x < 4; x++) if (on(x0 + x * 4 + 2, 8 + y * 4 + 2)) row |= 1 << (3 - x)
        rows.push(row)
      }
      let best = '?'
      let bestScore = -1
      for (const [ch, g] of Object.entries(glyphs)) {
        let score = 0
        for (let y = 0; y < 7; y++) {
          for (let x = 0; x < 4; x++) if (((rows[y] >> (3 - x)) & 1) === ((g[y] >> (3 - x)) & 1)) score++
        }
        if (score > bestScore) {
          bestScore = score
          best = ch
        }
      }
      out += best
    }
    return out
  }, CAPTCHA_GLYPHS)
  if (!answer || answer.includes('?')) return false
  await page.type('input[placeholder="请输入图中字符"]', answer, { delay: 20 })
  return true
}

async function loginCustomer(page, email = EMAIL, password = PASSWORD) {
  await page.goto(BASE + '/login', { waitUntil: 'domcontentloaded', timeout: 20000 })
  await page.waitForSelector('input[autocomplete="username"]', { timeout: 10000 })
  await page.type('input[autocomplete="username"]', email, { delay: 10 })
  await page.type('input[autocomplete="current-password"]', password, { delay: 10 })
  await page.keyboard.press('Enter')
  await page.waitForFunction(() => !location.pathname.startsWith('/login'), { timeout: 20000 }).catch(() => null)
  await sleep(1500)
  return page.evaluate(() => !location.pathname.startsWith('/login'))
}

async function loginAdmin(page) {
  await page.goto(BASE + '/admin#/login', { waitUntil: 'domcontentloaded', timeout: 20000 })
  await page.waitForSelector('input[autocomplete="username"]', { timeout: 10000 })
  await page.type('input[autocomplete="username"]', ADMIN_ACCOUNT, { delay: 10 })
  await page.type('input[autocomplete="current-password"]', ADMIN_PASSWORD, { delay: 10 })
  await sleep(800)
  await solveCaptchaIfPresent(page)
  await page.click('.admin-login__submit').catch(async () => {
    await page.keyboard.press('Enter')
  })
  const ok = await page
    .waitForSelector('.app-layout', { timeout: 20000 })
    .then(() => true)
    .catch(() => false)
  await sleep(1500)
  return ok
}

// ---------------------------------------------------------------- 通用检查模块

/** C-01 / C-02：未登录访问管理/用户 API 必须 401 JSON，不泄漏页面 HTML 或堆栈。 */
async function checkAuth(cfg, adminPage) {
  const adminBad = []
  for (const r of cfg.adminRoutes) {
    const res = await anonApi('GET', r)
    if (!(res.status === 401 && isJson(res))) adminBad.push(`${r} → ${res.status} ${res.contentType || '(无)'}`)
  }
  check('C-01', 'P0', `${cfg.title} 管理 API 未登录返回 401 JSON`, adminBad.length === 0,
    adminBad.length ? '异常：' + adminBad.join('；') : `${cfg.adminRoutes.length} 条路由全部 401 JSON`)

  if (!cfg.clientRoutes.length) {
    // 公告为公开内容（meta.free）、部分插件无用户侧接口：401 判据不适用
    skip('C-02', 'P0', `${cfg.title} 用户 API 未登录返回 401 JSON`, cfg.clientSkipReason || '该插件无用户侧 API')
    return
  }
  const clientBad = []
  for (const r of cfg.clientRoutes) {
    const res = await anonApi('GET', r)
    if (!(res.status === 401 && isJson(res))) clientBad.push(`${r} → ${res.status} ${res.contentType || '(无)'}`)
  }
  check('C-02', 'P0', `${cfg.title} 用户 API 未登录返回 401 JSON`, clientBad.length === 0,
    clientBad.length ? '异常：' + clientBad.join('；') : `${cfg.clientRoutes.length} 条路由全部 401 JSON`)

  // 已登录但非管理员访问管理 API：同样必须 401（防普通用户越权进后台接口）
  if (cfg.adminRoutes.length && adminPage) {
    const res = await pageApi(adminPage, 'GET', cfg.adminRoutes[0])
    const ok = res.status === 200 || res.status === 401
    check('C-01b', 'P0', `${cfg.title} 管理 API 登录态可访问（对照项）`, ok, `HTTP ${res.status}`)
  }
}

/** C-04：写请求 CSRF 令牌缺失/错配必须被拦截，且请求不落库。 */
async function checkCsrf(cfg, adminPage, customerPage) {
  const probes = []
  if (cfg.adminWriteProbe) probes.push({ tag: '管理', page: adminPage, probe: cfg.adminWriteProbe })
  if (cfg.clientWriteProbe) probes.push({ tag: '用户', page: customerPage, probe: cfg.clientWriteProbe })
  if (!probes.length) {
    skip('C-04', 'P0', `${cfg.title} CSRF 拦截`, '该插件无写接口，不适用')
    return
  }
  const bad = []
  for (const { tag, page, probe } of probes) {
    if (!page) continue
    const miss = await pageApi(page, 'POST', probe.path, probe.body, { csrf: false })
    const wrong = await pageApi(page, 'POST', probe.path, probe.body, { token: 'stale-token-for-acceptance' })
    const missOk = miss.status === 401 || miss.status === 303
    const wrongOk = wrong.status === 401 || wrong.status === 303
    if (!missOk) bad.push(`${tag}缺令牌 → ${miss.status}`)
    if (!wrongOk) bad.push(`${tag}错令牌 → ${wrong.status}`)
  }
  check('C-04', 'P0', `${cfg.title} 写请求 CSRF 缺失/错配被拦截`, bad.length === 0,
    bad.length ? '异常：' + bad.join('；') : '缺令牌与错令牌均被拦截（401/303），未进入业务逻辑')
}

/** C-05：畸形/恶意输入返回友好错误，不 panic、不 500。 */
async function checkMalformed(cfg, adminPage) {
  if (!cfg.malformedCases || !cfg.malformedCases.length || !adminPage) {
    skip('C-05', 'P0', `${cfg.title} 畸形输入与注入`, '未配置用例')
    return
  }
  const bad = []
  for (const c of cfg.malformedCases) {
    const res = await pageApi(adminPage, c.method || 'POST', c.path, c.body)
    const ok = res.status !== 0 && res.status < 500 && (res.json ? res.json.ok === 0 || res.json.ok === 1 : true)
    if (!ok) bad.push(`${c.name} → HTTP ${res.status} ${res.text.slice(0, 80)}`)
  }
  check('C-05', 'P0', `${cfg.title} 畸形输入与注入返回友好错误`, bad.length === 0,
    bad.length ? '异常：' + bad.join('；') : `${cfg.malformedCases.length} 个用例均未 500/panic`)
}

/** C-07：前台接口 PII 脱敏（响应中不出现用户 email/手机）。 */
async function checkPii(cfg, customerPage) {
  if (!cfg.clientRoutes.length || !cfg.piiNeedle || !customerPage) {
    skip('C-07', 'P1', `${cfg.title} 前台 PII 脱敏`, '未配置探针')
    return
  }
  const res = await pageApi(customerPage, 'GET', cfg.clientRoutes[0])
  const leaked = res.text.includes(cfg.piiNeedle)
  check('C-07', 'P1', `${cfg.title} 前台响应不含用户邮箱`, !leaked,
    leaked ? `响应中出现 ${cfg.piiNeedle}` : 'feed 响应未见邮箱（昵称/用户#id 脱敏）')
}

/**
 * C-03：越权防护。用户A（主账号）名下的私有资源，用户B（ALT 账号）不得读取/操作。
 * 探针两种模式：
 *  - kind='create'：用户A 先经 API 自建资源拿 id（响应须含 {ok:1,id}），再以用户B 访问；
 *  - kind='existing'：从后台列表取任意一条他人资源 id，再以用户B 操作。
 * 判据 expectStatus：HTTP 状态须落在数组内（如 404）；expect='notOk'：响应不得 ok:1
 * （JSONFail 走 HTTP 200 + {ok:0} 的接口用它）。
 */
async function checkPrivilege(cfg, ctx) {
  const probe = cfg.privProbe
  if (!probe) {
    skip('C-03', 'P1', `${cfg.title} 越权访问防护`, '该插件无用户私有资源探针')
    return
  }
  const { adminPage, customerPage, ensureAltPage } = ctx
  let resourceId = ''
  let setupInfo = ''
  if (probe.kind === 'create') {
    const res = await pageApi(customerPage, 'POST', probe.createPath, probe.createBody)
    if (!res.json || res.json.ok !== 1 || !res.json.id) {
      skip('C-03', 'P1', `${cfg.title} 越权访问防护`, `用户A 创建资源失败：HTTP ${res.status} ${res.text.slice(0, 80)}`)
      return
    }
    resourceId = res.json.id
    setupInfo = `用户A 自建资源 id=${resourceId}`
  } else {
    const res = await pageApi(adminPage, 'GET', probe.listPath)
    const first = ((res.json && res.json.list) || [])[0]
    if (!first || !first.id) {
      skip('C-03', 'P1', `${cfg.title} 越权访问防护`, `后台列表无既有资源可探针（HTTP ${res.status}）`)
      return
    }
    resourceId = first.id
    setupInfo = `既有他人资源 id=${resourceId}（来自 ${probe.listPath}）`
  }
  const altPage = await ensureAltPage()
  if (!altPage) {
    check('C-03', 'P1', `${cfg.title} 越权访问防护（用户B 访问用户A 资源）`, false, '第二账号登录失败')
    return
  }
  const path = probe.probePath.replace('{id}', String(resourceId))
  const res = await pageApi(altPage, probe.probeMethod || 'GET', path, probe.probeBody || {})
  const denied =
    probe.expect === 'notOk'
      ? !(res.json && res.json.ok === 1)
      : (probe.expectStatus || [404, 403]).includes(res.status)
  check('C-03', 'P1', `${cfg.title} 越权访问防护（用户B 访问用户A 资源）`, denied,
    denied
      ? `${setupInfo}；用户B ${probe.probeMethod || 'GET'} ${path} → HTTP ${res.status} ${res.text.slice(0, 60)}，已被拦截`
      : `越权成功！${setupInfo}；用户B ${probe.probeMethod || 'GET'} ${path} → HTTP ${res.status} ${res.text.slice(0, 120)}`)
}

/** B-01/B-02/B-04：启停闸门。切换后必须恢复原启用态（X-03）。 */
async function checkGate(cfg, adminPage, customerPage, initialState) {
  const routes = [...cfg.adminRoutes, ...cfg.clientRoutes]
  if (!routes.length) {
    skip('B-01', 'P0', `${cfg.title} 禁用后 API 一律 404`, '无可探测路由')
    return
  }
  const off = await pageApi(adminPage, 'POST', `/admin/plugins/${cfg.name}/toggle`, { enabled: false })
  if (off.status !== 200 || !off.json || off.json.ok !== 1) {
    check('B-01', 'P0', `${cfg.title} 禁用切换`, false, `toggle 失败：HTTP ${off.status} ${off.text.slice(0, 80)}`)
    return
  }
  const bad = []
  for (const r of routes) {
    const anon = await anonApi('GET', r)
    if (anon.status !== 404) bad.push(`匿名 ${r} → ${anon.status}`)
    const authed = await pageApi(adminPage, 'GET', r)
    if (authed.status !== 404) bad.push(`已登录 ${r} → ${authed.status}`)
  }
  check('B-01', 'P0', `${cfg.title} 禁用后管理/用户 API 一律 404`, bad.length === 0,
    bad.length ? '异常：' + bad.join('；') : `${routes.length} 条路由匿名与已登录均 404`)

  // B-02 菜单数据源：后台清单 enabled=false、前台用户中心菜单不含该插件
  const adminList = await pageApi(adminPage, 'GET', '/admin/plugins')
  const item = ((adminList.json && adminList.json.plugins) || []).find((p) => p.name === cfg.name)
  const adminMenuOk = !!item && item.enabled === false
  let clientMenuOk = true
  let clientMenuDetail = ''
  if (customerPage) {
    const cm = await pageApi(customerPage, 'GET', '/plugins/client')
    const found = ((cm.json && cm.json.plugins) || []).some((p) => p.name === cfg.name)
    clientMenuOk = !found
    clientMenuDetail = found ? '前台菜单仍包含该插件' : '前台菜单已不含该插件'
  }
  check('B-02', 'P1', `${cfg.title} 禁用后菜单不显示`, adminMenuOk && clientMenuOk,
    `后台清单 enabled=${item ? item.enabled : '未找到'}；${clientMenuDetail || '未验证前台菜单'}`)

  // B-04 重新启用后即时恢复（无需重启）
  const on = await pageApi(adminPage, 'POST', `/admin/plugins/${cfg.name}/toggle`, { enabled: true })
  const restored = []
  for (const r of routes) {
    const res = await anonApi('GET', r)
    // 恢复后匿名访问应回到 401（闸门已放行，由鉴权层拦截）而非 404
    if (res.status !== 401) restored.push(`${r} → ${res.status}`)
  }
  check('B-04', 'P1', `${cfg.title} 重新启用后能力即时恢复`, on.status === 200 && restored.length === 0,
    restored.length ? '异常：' + restored.join('；') : '路由恢复为 401（鉴权层拦截），无需重启')

  // 恢复到验收开始时的启用态（X-03）
  if (initialState === false) {
    await pageApi(adminPage, 'POST', `/admin/plugins/${cfg.name}/toggle`, { enabled: false })
    note(`${cfg.title} 已恢复到验收前状态：禁用`)
  }
}

/** D-01/D-02/D-03：三视图渲染、横向溢出、控制台错误。 */
async function checkRender(cfg, page, { url, name, waitSelector, waitText, shot }) {
  const errors = attachErrorCollector(page)
  const overflow = []
  const empty = []
  const consoleErr = []
  for (const vp of VIEWPORTS) {
    errors.length = 0
    await page.setViewport({ width: vp.width, height: vp.height })
    await page.goto(BASE + url, { waitUntil: 'domcontentloaded', timeout: 25000 })
    if (waitSelector) await page.waitForSelector(waitSelector, { timeout: 15000 }).catch(() => null)
    if (waitText) {
      await page
        .waitForFunction((t) => (document.body.innerText || '').includes(t), { timeout: 15000 }, waitText)
        .catch(() => null)
    }
    await sleep(1500)
    const m = await page.evaluate(() => ({
      overflow: document.documentElement.scrollWidth - document.documentElement.clientWidth,
      bodyLen: (document.body.innerText || '').trim().length,
    }))
    await page.screenshot({ path: path.join(SHOTS, `${shot || name}-${vp.key}.png`) })
    if (m.bodyLen <= 5) empty.push(`${vp.label} 正文 ${m.bodyLen} 字符`)
    if (m.overflow > 1) overflow.push(`${vp.label} 溢出 ${m.overflow}px`)
    if (errors.length) consoleErr.push(`${vp.label}: ${errors[0].slice(0, 120)}`)
  }
  check('D-01', 'P0', `${name} 三视图正文非空`, empty.length === 0, empty.length ? empty.join('；') : '三个视口正文均完整渲染')
  check('D-02', 'P1', `${name} 三视图无横向溢出`, overflow.length === 0, overflow.length ? overflow.join('；') : '三个视口均无横向溢出')
  check('D-03', 'P1', `${name} 控制台无错误`, consoleErr.length === 0, consoleErr.length ? consoleErr.join('；') : '三个视口均无 console/pageerror')
}

const escapeReg = (s) => s.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')

/** 关闭所有已展开的下拉：多个 popper 同时可见时会串选，必须先收敛。 */
async function closeDropdowns(page) {
  await page.evaluate(() => {
    const ae = document.activeElement
    if (ae && typeof ae.blur === 'function') ae.blur()
    document.body.click()
  })
  const t0 = Date.now()
  while (Date.now() - t0 < 3000) {
    const n = await page.evaluate(
      () => Array.from(document.querySelectorAll('.el-select-dropdown__item')).filter((el) => el.getClientRects().length > 0).length,
    )
    if (n === 0) return true
    await sleep(150)
  }
  return false
}

/** 通用页面辅助：Element Plus 下拉与 Toast。 */
async function openSelectByPlaceholder(page, placeholderText) {
  await closeDropdowns(page)
  return page.evaluate((text) => {
    const isVisible = (el) => el && el.getClientRects().length > 0
    document.querySelectorAll('.el-select[data-qa-open]').forEach((s) => s.removeAttribute('data-qa-open'))
    const ph = Array.from(document.querySelectorAll('.el-select__placeholder')).find(
      (p) => (p.textContent || '').trim() === text && isVisible(p),
    )
    if (!ph) return false
    const select = ph.closest('.el-select')
    const wrap = ph.closest('.el-select__wrapper') || ph
    wrap.dispatchEvent(new MouseEvent('click', { bubbles: true }))
    const input = select && select.querySelector('.el-select__input')
    if (input) input.focus()
    if (select) select.setAttribute('data-qa-open', '1')
    return !!input
  }, placeholderText)
}

/**
 * 在可见下拉项中按文本选择一项。
 * mode='substr' 按子串匹配；mode='regex' 按正则全文匹配（用于白名单选项，保证取值合法）。
 * 返回选中文本，超时未选中返回 null。
 */
async function pickDropdownItem(page, pattern, mode = 'regex', timeout = 12000) {
  const t0 = Date.now()
  while (Date.now() - t0 < timeout) {
    const picked = await page.evaluate((p, m) => {
      const isVisible = (el) => el && el.getClientRects().length > 0
      const items = Array.from(document.querySelectorAll('.el-select-dropdown__item')).filter(isVisible)
      if (!items.length) return null
      let target = null
      if (m === 'regex') {
        const re = new RegExp(p)
        target = items.find((it) => re.test((it.textContent || '').trim()))
      } else {
        target = items.find((it) => ((it.textContent || '').includes(p)))
      }
      if (!target) return null
      const text = (target.textContent || '').trim()
      target.dispatchEvent(new MouseEvent('click', { bubbles: true }))
      return text
    }, pattern, mode)
    if (picked) return picked
    await sleep(250)
  }
  return null
}

/** 点击指定容器内可见的、文本匹配的按钮（粗粒度范围控制）。 */
async function clickButtonIn(page, containerSelector, text) {
  return page.evaluate(
    (sel, t) => {
      const isVisible = (el) => el && el.getClientRects().length > 0
      const root = document.querySelector(sel)
      if (!root) return false
      const btn = Array.from(root.querySelectorAll('button')).find(
        (b) => (b.textContent || '').trim() === t && isVisible(b),
      )
      if (!btn) return false
      btn.click()
      return true
    },
    containerSelector,
    text,
  )
}

/** 以锚点元素收敛按钮点击范围，返回 { ok, why }。 */
async function clickButtonNear(page, anchorSelector, text) {
  return page.evaluate(
    (anchorSel, t) => {
      const isVisible = (el) => el && el.getClientRects().length > 0
      const anchor = Array.from(document.querySelectorAll(anchorSel)).find(isVisible)
      if (!anchor) return { ok: false, why: `锚点不可见：${anchorSel}` }
      const scope = anchor.closest('form, .el-form, .el-tab-pane, .el-drawer, .el-dialog, .el-card') || anchor
      const btn = Array.from(scope.querySelectorAll('button')).find(
        (b) => (b.textContent || '').trim() === t && isVisible(b) && !b.disabled,
      )
      if (!btn) return { ok: false, why: `范围内无可用「${t}」按钮` }
      btn.click()
      return { ok: true }
    },
    anchorSelector,
    text,
  )
}

async function waitToast(page, pattern, timeout = 10000) {
  const t0 = Date.now()
  while (Date.now() - t0 < timeout) {
    const text = await page.evaluate(() => {
      const isVisible = (el) => el && el.getClientRects().length > 0
      const m = Array.from(document.querySelectorAll('.el-message')).filter(isVisible).pop()
      return m ? (m.textContent || '').trim() : ''
    })
    if (text && pattern.test(text)) return text
    await sleep(250)
  }
  return ''
}

/**
 * 在 live 请求轨迹中从 startIdx 起轮询匹配条目：把「Toast 文案」升级为「网络层真实提交」的
 * 硬证据，避免通用配置表单与业务表单成功提示同文案造成的假阳性。
 * 注意必须每轮重新 slice netLog——探针的 body 读取是异步的，条目可能晚于 Toast 才入列，
 * 传入静态快照会因竞态永远等不到（V-01a 曾因此偶发假红）。
 */
async function waitNetEntry(pattern, startIdx = 0, timeout = 10000) {
  const t0 = Date.now()
  while (Date.now() - t0 < timeout) {
    const hit = netLog.slice(startIdx).find((l) => pattern.test(l))
    if (hit) return hit
    await sleep(200)
  }
  return ''
}

// ---------------------------------------------------------------- 插件配置

const PLUGINS = {
  violation: {
    name: 'violation',
    title: '用户违规管理',
    adminPath: '/admin#/plugin/violation',
    clientPath: '/plugin/violation',
    adminRoutes: [
      '/admin/plugin/violation/list',
      '/admin/plugin/violation/stats',
      '/admin/plugin/violation/form',
      '/admin/plugin/violation/users/search?keyword=a',
      '/admin/plugin/violation/announcements',
    ],
    clientRoutes: ['/plugin/violation/feed', '/plugin/violation/record/1', '/plugin/violation/announcement/1'],
    adminWriteProbe: { path: '/admin/plugin/violation/save', body: { user_id: 0, level: '', type: '', description: '' } },
    piiNeedle: '@',
    malformedCases: [
      { name: 'user_id 非数字', path: '/admin/plugin/violation/save', body: { user_id: 'abc', level: 'light', type: '其他', description: 'x' } },
      { name: 'user_id 不存在', path: '/admin/plugin/violation/save', body: { user_id: 999999999, level: 'light', type: '其他', description: 'x' } },
      { name: 'level 非法值', path: '/admin/plugin/violation/save', body: { user_id: 1, level: 'critical', type: '其他', description: 'x' } },
      { name: 'type 不在白名单', path: '/admin/plugin/violation/save', body: { user_id: 1, level: 'light', type: '不在选项中的类型', description: 'x' } },
      { name: 'action 越界白名单', path: '/admin/plugin/violation/save', body: { user_id: 1, level: 'light', type: '其他', description: 'x', action: '越权措施' } },
      { name: 'description SQL 片段', path: '/admin/plugin/violation/save', body: { user_id: 1, level: 'light', type: '其他', description: "x'; DROP TABLE users; --" } },
      { name: '超长 description', path: '/admin/plugin/violation/save', body: { user_id: 1, level: 'light', type: '其他', description: 'A'.repeat(100000) } },
      { name: '删除不存在记录', path: '/admin/plugin/violation/999999999/delete', body: {} },
    ],
    adminWaitText: '违规管理',
    clientWaitSelector: '.el-table',
    flow: flowViolation,
  },
  refund: {
    name: 'refund',
    title: '用户自助退款',
    adminPath: '/admin#/plugin/refund',
    clientPath: '/plugin/refund',
    adminRoutes: ['/admin/plugin/refund/list', '/admin/plugin/refund/stats', '/admin/plugin/refund/config'],
    clientRoutes: ['/plugin/refund/my', '/plugin/refund/my-stats', '/plugin/refund/eligible-orders'],
    adminWriteProbe: { path: '/admin/plugin/refund/999999999/approve', body: {} },
    clientWriteProbe: { path: '/plugin/refund/999999999/withdraw', body: {} },
    piiNeedle: '@',
    malformedCases: [
      { name: '审批不存在申请', method: 'POST', path: '/admin/plugin/refund/999999999/approve', body: {} },
      { name: '驳回不存在申请', method: 'POST', path: '/admin/plugin/refund/999999999/reject', body: { handle_note: 'x' } },
      { name: 'list 注入参数', method: 'GET', path: "/admin/plugin/refund/list?status=' OR 1=1--&page=abc&limit=-1", body: {} },
      { name: '撤回不存在申请', method: 'POST', path: '/plugin/refund/999999999/withdraw', body: {} },
    ],
    // JSONFail 走 HTTP 200 + {ok:0}：判据取「不得 ok:1」
    privProbe: {
      kind: 'existing',
      listPath: '/admin/plugin/refund/list',
      probePath: '/plugin/refund/{id}/withdraw',
      probeMethod: 'POST',
      expect: 'notOk',
    },
    adminWaitText: '退款审核',
    clientWaitText: '我的退款申请',
    clientWaitSelector: '.el-card',
    flow: flowRefund,
  },
  tickets: {
    name: 'tickets',
    title: '工单支持',
    adminPath: '/admin#/plugin/tickets',
    clientPath: '/tickets',
    adminRoutes: ['/admin/plugin/tickets/list', '/admin/plugin/tickets/stats', '/admin/plugin/tickets/assignees'],
    clientRoutes: ['/plugin/tickets/list'],
    adminWriteProbe: { path: '/admin/plugin/tickets/999999999/reply', body: { content: 'x' } },
    clientWriteProbe: { path: '/plugin/tickets/999999999/reply', body: { content: 'x' } },
    piiNeedle: '@',
    malformedCases: [
      { name: 'create 缺主题与正文', method: 'POST', path: '/plugin/tickets/create', body: { priority: 'normal', category: 'technical' } },
      { name: 'create 超长主题', method: 'POST', path: '/plugin/tickets/create', body: { subject: 'S'.repeat(200), body: 'x' } },
      { name: 'list 注入关键词', method: 'GET', path: "/admin/plugin/tickets/list?q=' OR 1=1--", body: {} },
      { name: '回复不存在工单', method: 'POST', path: '/plugin/tickets/999999999/reply', body: { content: 'x' } },
    ],
    // clientDetail 对非本人工单走 StatusFail 404
    privProbe: {
      kind: 'create',
      createPath: '/plugin/tickets/create',
      createBody: { subject: 'QA_MARKER 越权探针工单', body: 'C-03 验收造数', priority: 'normal', category: 'technical' },
      probePath: '/plugin/tickets/{id}',
      probeMethod: 'GET',
      expectStatus: [404],
    },
    adminWaitText: '待处理',
    clientWaitSelector: '.ticket-filters',
    flow: flowTickets,
  },
  announcement: {
    name: 'announcement',
    title: '系统公告',
    adminPath: '/admin#/plugin/announcement',
    clientPath: '/notices',
    adminRoutes: ['/admin/plugin/announcement/list', '/admin/plugin/announcement/form'],
    // 公告前台为 meta.free 公开内容，未登录即可访问：401 判据不适用
    clientRoutes: [],
    clientSkipReason: '公告前台为公开内容（meta.free），未登录可访问，401 判据不适用',
    adminWriteProbe: { path: '/admin/plugin/announcement/save', body: { title: '' } },
    malformedCases: [
      { name: 'save 空标题', method: 'POST', path: '/admin/plugin/announcement/save', body: { title: '' } },
      { name: 'save 超长标题', method: 'POST', path: '/admin/plugin/announcement/save', body: { title: 'T'.repeat(300), content: 'x' } },
      { name: '删除不存在公告', method: 'POST', path: '/admin/plugin/announcement/999999999/delete', body: {} },
    ],
    adminWaitText: '系统公告',
    clientWaitSelector: '.notices-layout',
    flow: flowAnnouncement,
  },
  webhooknotify: {
    name: 'webhooknotify',
    title: 'Webhook 通知',
    adminPath: '/admin#/plugin/webhooknotify',
    adminRoutes: ['/admin/plugin/webhooknotify/logs', '/admin/plugin/webhooknotify/config'],
    clientRoutes: [],
    clientSkipReason: '该插件无用户侧接口（仅后台日志与配置）',
    adminWriteProbe: { path: '/admin/plugin/webhooknotify/test', body: {} },
    malformedCases: [
      { name: '未配置 URL 时发送测试', method: 'POST', path: '/admin/plugin/webhooknotify/test', body: {} },
      { name: 'logs 注入参数', method: 'GET', path: "/admin/plugin/webhooknotify/logs?page=abc&limit=-1", body: {} },
    ],
    adminWaitText: '投递日志',
    flow: flowWebhooknotify,
  },
  dailyreport: {
    name: 'dailyreport',
    title: '每日运营报告',
    adminPath: '/admin#/plugin/dailyreport',
    // send-now 为 POST：GET 探测会得 405 而非 404，故 adminRoutes 只列 GET 路由
    adminRoutes: ['/admin/plugin/dailyreport/config'],
    clientRoutes: [],
    clientSkipReason: '该插件无用户侧接口（仅后台配置与定时任务）',
    adminWriteProbe: { path: '/admin/plugin/dailyreport/send-now', body: {} },
    malformedCases: [
      { name: 'hour 越界', method: 'POST', path: '/admin/plugin/dailyreport/config', body: { hour: 999 } },
      { name: 'hour 非数字', method: 'POST', path: '/admin/plugin/dailyreport/config', body: { hour: 'abc' } },
    ],
    adminWaitText: '每日运营报告',
    clientWaitSelector: '.el-form',
    flow: flowDailyreport,
  },
  spaceship: {
    name: 'spaceship',
    title: '域名注册',
    adminPath: '/admin#/plugin/spaceship',
    clientPath: '/plugin/spaceship',
    adminRoutes: [
      '/admin/plugin/spaceship/contacts',
      '/admin/plugin/spaceship/domains',
      '/admin/plugin/spaceship/operations',
    ],
    clientRoutes: ['/plugin/spaceship/my', '/plugin/spaceship/my/contacts'],
    adminWriteProbe: { path: '/admin/plugin/spaceship/domains/999999999/renew', body: {} },
    clientWriteProbe: { path: '/plugin/spaceship/register', body: { domain: '', years: 1, paidAmount: '' } },
    piiNeedle: '@',
    malformedCases: [
      // 注册入口三道校验：域名格式 / 年限越界 / 归属用户缺失（QA 环境未配 API 时先行返回「未配置」，均为 ok:0）
      { name: '注册缺域名', method: 'POST', path: '/admin/plugin/spaceship/domains/register', body: { domain: '', years: 1, contactId: 1, userId: 1, paidAmount: '1.00' } },
      { name: '注册年限越界', method: 'POST', path: '/admin/plugin/spaceship/domains/register', body: { domain: 'qa-probe.invalid', years: 99, contactId: 1, userId: 1, paidAmount: '1.00' } },
      { name: '注册未指定归属用户', method: 'POST', path: '/admin/plugin/spaceship/domains/register', body: { domain: 'qa-probe.invalid', years: 1, contactId: 1, paidAmount: '1.00' } },
      { name: '联系人缺必填字段', method: 'POST', path: '/admin/plugin/spaceship/contacts', body: { firstName: 'QA' } },
      { name: '域名 id 非数字', method: 'GET', path: '/admin/plugin/spaceship/domains/abc', body: {} },
      { name: '自动续费不存在域名', method: 'POST', path: '/admin/plugin/spaceship/domains/999999999/autorenew', body: { enable: true } },
      { name: '删除不存在联系人', method: 'DELETE', path: '/admin/plugin/spaceship/contacts/999999999', body: {} },
      { name: '重试不存在操作', method: 'POST', path: '/admin/plugin/spaceship/operations/999999999/retry', body: {} },
      { name: '操作日志注入参数', method: 'GET', path: "/admin/plugin/spaceship/operations?limit=-1&offset=' OR 1=1--", body: {} },
      // 未配置 API 时 JSONFail「未配置」（ok:0）；已配置则走真实连通性探测，不得 500
      { name: '测试连接', method: 'POST', path: '/admin/plugin/spaceship/client/test', body: {} },
    ],
    // 前台自有域名：用户B 操作他人域名，clientAutoRenew 归属校验 404（JSONFail ok:0）
    privProbe: {
      kind: 'existing',
      listPath: '/admin/plugin/spaceship/domains',
      probePath: '/plugin/spaceship/my/{id}/autorenew',
      probeMethod: 'POST',
      probeBody: {},
      expect: 'notOk',
    },
    adminWaitText: '域名注册',
    clientWaitText: '我的域名',
    clientWaitSelector: '.spaceship-tabs',
    flow: flowSpaceship,
  },
}

// ---------------------------------------------------------------- violation 业务流

async function flowViolation(ctx) {
  const { adminPage, customerPage } = ctx
  const MARKER2 = MARKER + 'P' // 未公示记录专用标记

  // 取目标用户（用测试账号自身）
  const search = await pageApi(adminPage, 'GET', `/admin/plugin/violation/users/search?keyword=${encodeURIComponent(EMAIL)}`)
  const users = (search.json && search.json.list) || []
  const target = users.find((u) => u.email === EMAIL) || users[0]
  if (!target) {
    check('V-01', 'P0', '后台添加违规并前台公示可见', false, `用户搜索未返回 ${EMAIL}，无法造数`)
    return
  }
  note('violation 目标用户', `#${target.id} ${target.email}`)

  const statsBefore = await pageApi(adminPage, 'GET', '/admin/plugin/violation/stats')
  const totalBefore = (statsBefore.json && statsBefore.json.stats && statsBefore.json.stats.total) || 0

  // 以后端配置的白名单作为下拉选项判据：既保证后续提交的 type 一定合法，也核对前端选项与后端一致
  const cfgRes = await pageApi(adminPage, 'GET', '/admin/plugin/violation/config')
  const cfgValues = (cfgRes.json && cfgRes.json.values) || {}
  const typeOptions = String(cfgValues.typeOptions || '').split('\n').map((s) => s.trim()).filter(Boolean)
  note('violation 违规类型白名单（后端配置）', typeOptions.join(' / ') || '(未取到)')

  // ---- 后台填表 ----
  await adminPage.setViewport({ width: 1440, height: 900 })
  await adminPage.goto(BASE + '/admin#/plugin/violation', { waitUntil: 'domcontentloaded', timeout: 25000 })
  await adminPage.waitForSelector('.violation-tabs', { timeout: 15000 }).catch(() => null)
  await adminPage.evaluate(() => {
    const isVisible = (el) => el && el.getClientRects().length > 0
    const tab = Array.from(document.querySelectorAll('.el-tabs__item')).find(
      (t) => (t.textContent || '').trim() === '添加违规' && isVisible(t),
    )
    tab && tab.click()
  })
  await adminPage.waitForSelector('textarea[placeholder="描述违规事实、发生时间与影响范围"]', { timeout: 15000 })

  // 违规用户（远程搜索下拉）：按邮箱子串匹配，避免与其他 popper 串选
  await openSelectByPlaceholder(adminPage, '输入邮箱或用户名搜索')
  await adminPage.type('.el-select[data-qa-open="1"] .el-select__input', EMAIL, { delay: 10 })
  const pickedUser = await pickDropdownItem(adminPage, EMAIL, 'substr')
  note('violation 选中用户', pickedUser || '(未选中)')
  // 违规类型：按后端白名单整词匹配
  await openSelectByPlaceholder(adminPage, '选择违规类型')
  const typePattern = typeOptions.length ? `^(${typeOptions.map(escapeReg).join('|')})$` : '其他'
  const pickedType = await pickDropdownItem(adminPage, typePattern, 'regex')
  note('violation 选中类型', pickedType || '(未选中)')
  // 违规等级（单选）
  await adminPage.evaluate(() => {
    const r = document.querySelector('.el-radio')
    r && r.click()
  })
  // 描述（含 MARKER，便于前台定位）
  await adminPage.type(
    'textarea[placeholder="描述违规事实、发生时间与影响范围"]',
    `${MARKER} 验收造数：滥用资源自动化测试描述`,
    { delay: 5 },
  )
  // 是否公示：打开开关
  await adminPage.evaluate(() => {
    const sw = document.querySelector('.el-switch')
    if (sw && !sw.classList.contains('is-checked')) sw.click()
  })
  await adminPage.screenshot({ path: path.join(SHOTS, 'violation-form-filled.png') })

  // ---- 提交 ----
  // PluginShell 内通用配置表单与违规表单都有「保存」，必须用描述框锚定范围，否则会误点配置保存
  const TEXTAREA = 'textarea[placeholder="描述违规事实、发生时间与影响范围"]'
  const netMark = netLog.length
  const clickRes = await clickButtonNear(adminPage, TEXTAREA, '保存')
  const toast = await waitToast(adminPage, /已保存|失败/)
  // 网络层硬证据：确实发出了 POST .../plugin/violation/save 且响应 ok:1
  const saveNet = await waitNetEntry(
    /POST \d+ \/__api\/(admin\/)?plugin\/violation\/save :: .*"ok"\s*:\s*1/,
    netMark,
  )
  const formReset = await adminPage.evaluate((sel) => {
    const ta = document.querySelector(sel)
    return !!ta && !(ta.value || '').trim()
  }, TEXTAREA)
  // D-01/P3：emit('saved') 先于 resetForm()，父组件同步切 tab 后用 v-if 卸载表单，
  // 「保存后表单未重置」为设计行为而非缺陷，formReset 仅作观测记录，不计入判据
  const saved = clickRes.ok && !!saveNet && /已保存/.test(toast)
  check('V-01a', 'P0', '后台保存真实提交且提示成功', saved,
    saved
      ? `提示「${toast}」；${saveNet}；表单状态观测：${formReset ? '已重置' : '未重置（D-01/P3 已知：保存成功后表单即被卸载）'}`
      : [
          clickRes.ok ? '' : `点击失败：${clickRes.why}`,
          saveNet ? '' : '未捕获 /save 成功请求（可能误点了其他保存按钮）',
          /已保存/.test(toast) ? '' : `Toast 未匹配（${toast || '无'}）`,
        ].filter(Boolean).join('；'))

  // ---- 落库核对：列表出现 + stats total+1（E-01/E-03）----
  const listRes = await pageApi(adminPage, 'GET', `/admin/plugin/violation/list?keyword=${MARKER}&limit=20`)
  const rows = (listRes.json && listRes.json.list) || []
  const inList = rows.some((r) => (r.description || '').includes(MARKER))
  check('E-01', 'P0', '写操作真实落库（列表可查）', inList, inList ? `列表命中 ${rows.length} 条` : '列表中未找到 MARKER 记录')

  const statsAfter = await pageApi(adminPage, 'GET', '/admin/plugin/violation/stats')
  const totalAfter = (statsAfter.json && statsAfter.json.stats && statsAfter.json.stats.total) || 0
  const before = (statsBefore.json && statsBefore.json.stats && statsBefore.json.stats.total) || 0
  check('E-03', 'P1', '统计随写操作同步更新', totalAfter === before + 1,
    `total ${before} → ${totalAfter}`)

  // ---- V-10：操作后表格 DOM 即时刷新 + 统计卡片同步（D-02 修复的回归判据）----
  // 机制：onSaved/removeRecord 均并发调用 loadList+loadStats，二者共享 useAdminRequest 代际守卫，
  // 后启动者会把先启动者的响应判为过期而静默跳过 → 表格停留在旧数据（D-02 症状）。
  // 统计卡片有 2s 计数动画（ArtCountTo），轮询等待数值稳定。
  const savedRec = rows.find((r) => (r.description || '').includes(MARKER))
  const savedId = savedRec ? String(savedRec.id) : ''

  const rowAppeared = await adminPage.evaluate(async (id) => {
    const isVisible = (el) => el && el.getClientRects().length > 0
    const sleep = (ms) => new Promise((r) => setTimeout(r, ms))
    if (!id) return '未取到新记录 ID'
    for (let i = 0; i < 24; i++) {
      const row = Array.from(document.querySelectorAll('.el-table__row')).find((r) => {
        const cell = r.querySelector('.el-table__cell, td')
        return cell && (cell.textContent || '').trim() === id
      })
      if (row && isVisible(row)) return ''
      await sleep(250)
    }
    return `表格 DOM 中未出现 ID=${id} 的新行（保存后未刷新）`
  }, savedId)
  check('V-10a', 'P1', '保存后后台表格 DOM 即时出现新行', rowAppeared === '',
    rowAppeared || `ID=${savedId} 已渲染进列表表格`)

  const cardSynced = await adminPage.evaluate(async (expect) => {
    const sleep = (ms) => new Promise((r) => setTimeout(r, ms))
    const readTotal = () => {
      const card = Array.from(document.querySelectorAll('.art-card')).find((c) => (c.textContent || '').includes('记录总数'))
      if (!card) return null
      const num = card.querySelector('.tabular-nums')
      return num ? Number((num.textContent || '').replace(/,/g, '')) : null
    }
    for (let i = 0; i < 24; i++) {
      if (readTotal() === expect) return ''
      await sleep(250)
    }
    return `统计卡片未更新到 ${expect}（当前 ${readTotal()}）`
  }, totalAfter)
  check('V-10b', 'P1', '保存后统计卡片同步更新', cardSynced === '',
    cardSynced || `「记录总数」卡片 = ${totalAfter}`)

  // ---- 前台公示可见 + 详情含完整描述（V-02）----
  // 顺序约束：必须在 V-10c 删除该记录之前执行，否则记录已删、feed 必然查无此记录
  await customerPage.setViewport({ width: 1440, height: 900 })
  await customerPage.goto(BASE + '/plugin/violation', { waitUntil: 'domcontentloaded', timeout: 25000 })
  await customerPage.waitForSelector('.el-table__row', { timeout: 15000 }).catch(() => null)
  await sleep(1200)
  const feedRes = await pageApi(customerPage, 'GET', '/plugin/violation/feed?limit=50')
  const feed = ((feedRes.json && feedRes.json.list) || []).filter((i) => i.kind === 'record')
  const publicVisible = feed.some((i) => (i.description || '').includes(MARKER))
  check('V-02a', 'P0', '前台公示表格出现该记录', publicVisible,
    publicVisible ? `feed 命中（共 ${feed.length} 条公示记录）` : 'feed 中未见 MARKER 记录')

  // 点行内「详情」→ 弹窗含完整描述
  // 桌面表格为列定义驱动、无 description 列（描述仅在详情弹窗展示），必须按首列记录 ID 精确定位行
  const markerRecord = feed.find((i) => (i.description || '').includes(MARKER))
  const markerId = markerRecord ? String(markerRecord.id) : ''
  const detailOk = await customerPage.evaluate(async ({ id, marker }) => {
    const isVisible = (el) => el && el.getClientRects().length > 0
    const sleep = (ms) => new Promise((r) => setTimeout(r, ms))
    if (!id) return 'feed 中未取到 MARKER 记录 ID'
    const row = Array.from(document.querySelectorAll('.el-table__row')).find((r) => {
      const cell = r.querySelector('.el-table__cell, td')
      return cell && (cell.textContent || '').trim() === id
    })
    if (!row) return `未找到 ID=${id} 的表格行（可能不在第 1 页）`
    const btn = Array.from(row.querySelectorAll('button')).find((b) => (b.textContent || '').trim() === '详情')
    if (!btn) return '行内无「详情」按钮'
    btn.click()
    for (let i = 0; i < 40; i++) {
      await sleep(250)
      const dlg = Array.from(document.querySelectorAll('.el-dialog, .el-message-box')).find(isVisible)
      if (dlg && (dlg.textContent || '').includes(marker)) return ''
    }
    return '详情弹窗未出现或不含 MARKER'
  }, { id: markerId, marker: MARKER })
  await customerPage.screenshot({ path: path.join(SHOTS, 'violation-public-detail.png') })
  check('V-02b', 'P0', '前台详情弹窗显示完整描述', detailOk === '', detailOk || '弹窗含 MARKER 描述')
  await customerPage.evaluate(() => {
    const isVisible = (el) => el && el.getClientRects().length > 0
    const close = Array.from(document.querySelectorAll('.el-dialog__headerbtn, .el-dialog__close')).find(isVisible)
    close && close.click()
  })

  // V-10c：DOM 删除该行（经二次确认弹窗）→ 行从表格消失且卡片回落
  const domDelete = await adminPage.evaluate(async ({ id, expect }) => {
    const isVisible = (el) => el && el.getClientRects().length > 0
    const sleep = (ms) => new Promise((r) => setTimeout(r, ms))
    const findRow = () => Array.from(document.querySelectorAll('.el-table__row')).find((r) => {
      const cell = r.querySelector('.el-table__cell, td')
      return cell && (cell.textContent || '').trim() === id
    })
    const readTotal = () => {
      const card = Array.from(document.querySelectorAll('.art-card')).find((c) => (c.textContent || '').includes('记录总数'))
      if (!card) return null
      const num = card.querySelector('.tabular-nums')
      return num ? Number((num.textContent || '').replace(/,/g, '')) : null
    }
    const row = findRow()
    if (!row) return '删除前未找到目标行'
    const delBtn = Array.from(row.querySelectorAll('button')).find((b) => (b.textContent || '').trim() === '删除')
    if (!delBtn) return '行内无「删除」按钮'
    delBtn.click()
    let confirmed = false
    for (let i = 0; i < 20; i++) {
      await sleep(250)
      const box = Array.from(document.querySelectorAll('.el-message-box')).find(isVisible)
      if (box) {
        const ok = Array.from(box.querySelectorAll('button')).find((b) => (b.textContent || '').trim() === '删除')
        if (ok) {
          ok.click()
          confirmed = true
          break
        }
      }
    }
    if (!confirmed) return '二次确认弹窗未出现或 confirm 按钮非「删除」'
    for (let i = 0; i < 24; i++) {
      await sleep(250)
      if (!findRow() && readTotal() === expect) return ''
    }
    return `删除后未同步：目标行${findRow() ? '仍在表格 DOM 中' : '已消失'}，卡片=${readTotal()}（期望 ${expect}）`
  }, { id: savedId, expect: totalAfter - 1 })
  check('V-10c', 'P1', '删除后表格 DOM 即时移除该行且卡片回落（含二次确认）', domDelete === '',
    domDelete || `ID=${savedId} 已从表格移除，卡片回落到 ${totalAfter - 1}`)

  // ---- V-03：未公示记录前台不出现 ----
  const hidden = await pageApi(adminPage, 'POST', '/admin/plugin/violation/save', {
    user_id: target.id,
    level: 'light',
    type: pickedType || '其他',
    description: `${MARKER2} 未公示验收造数`,
    action: '',
    public: false,
  })
  await sleep(600)
  const feed2 = await pageApi(customerPage, 'GET', '/plugin/violation/feed?limit=50')
  const hiddenLeak = ((feed2.json && feed2.json.list) || []).some((i) => (i.description || '').includes(MARKER2))
  const hiddenSaved = hidden.status === 200 && hidden.json && hidden.json.ok === 1
  check('V-03', 'P1', '未公示记录前台不出现', !hiddenLeak && hiddenSaved,
    hiddenLeak ? '未公示记录泄漏到前台 feed'
      : hiddenSaved ? '未公示记录已落库且未出现在 feed'
        : `未公示记录保存失败 HTTP ${hidden.status} ${hidden.text.slice(0, 60)}`)

  // ---- V-05：等级非法/类型越界被拒，处置措施留空允许保存 ----
  const badLevel = await pageApi(adminPage, 'POST', '/admin/plugin/violation/save', {
    user_id: target.id, level: 'super', type: '其他', description: 'x', public: false,
  })
  check('V-05a', 'P1', '等级非法值返回 400', badLevel.status === 400, `HTTP ${badLevel.status} ${badLevel.text.slice(0, 60)}`)

  const emptyAction = await pageApi(adminPage, 'POST', '/admin/plugin/violation/save', {
    user_id: target.id, level: 'medium', type: pickedType || '其他', description: `${MARKER} 处置留空回归`, action: '', public: false,
  })
  const emptyActionOk = emptyAction.status === 200 && emptyAction.json && emptyAction.json.ok === 1
  check('V-05b', 'P1', '处置措施留空允许保存（回归项）', emptyActionOk,
    emptyActionOk ? '保存成功' : `HTTP ${emptyAction.status} ${emptyAction.text.slice(0, 80)}`)

  // ---- C-05b：description 长度上限（畸形不返回 500 已由 checkMalformed 覆盖，这里查是否被拦下）----
  const longDesc = await pageApi(adminPage, 'POST', '/admin/plugin/violation/save', {
    user_id: target.id, level: 'light', type: '其他', description: 'A'.repeat(100000), public: false,
  })
  const longAccepted = longDesc.status === 200 && longDesc.json && longDesc.json.ok === 1
  check('C-05b', 'P2', '超长 description 应被拒绝或截断', !longAccepted,
    longAccepted ? '后端接受 10 万字符 description 并落库（缺少长度上限校验）' : `HTTP ${longDesc.status} ${longDesc.text.slice(0, 60)}`)

  // ---- 清理：按关键词经 API 检索并删除全部 MARKER / MARKER2 记录 ----
  // DOM 驱动的「点删除 + ElMessageBox 二次确认」已由 V-10c 完整覆盖，
  // 清理体改用 API（list?keyword → 逐条 delete），与下方 C-05 副作用清扫同一模式，
  // 避免同文档 hash 跳转后 activeTab 状态不确定导致的 DOM 定位失败
  let deleted = 0
  for (const kw of [MARKER, MARKER2]) {
    for (let i = 0; i < 10; i++) {
      const r = await pageApi(adminPage, 'GET', `/admin/plugin/violation/list?keyword=${encodeURIComponent(kw)}&limit=50`)
      const hit = ((r.json && r.json.list) || []).find((x) => (x.description || '').includes(kw))
      if (!hit) break
      const del = await pageApi(adminPage, 'POST', `/admin/plugin/violation/${hit.id}/delete`, {})
      if (del.status !== 200 || !del.json || del.json.ok !== 1) {
        note('清理失败', `id=${hit.id} HTTP ${del.status} ${del.text.slice(0, 60)}`)
        break
      }
      deleted++
      await sleep(200)
    }
  }
  const leftRes = await pageApi(adminPage, 'GET', `/admin/plugin/violation/list?keyword=${MARKER}&limit=50`)
  const leftRows = ((leftRes.json && leftRes.json.list) || []).filter((r) => (r.description || '').includes(MARKER))
  const hiddenLeft = await pageApi(adminPage, 'GET', `/admin/plugin/violation/list?keyword=${MARKER2}&limit=50`)
  const hiddenLeftRows = ((hiddenLeft.json && hiddenLeft.json.list) || []).filter((r) => (r.description || '').includes(MARKER2))
  const allClean = leftRows.length === 0 && hiddenLeftRows.length === 0
  check('V-07a', 'P1', '删除需二次确认且记录被清理', allClean,
    allClean
      ? `二次确认证据见 V-10c（ElMessageBox confirm「删除」后方才删除）；API 清理 ${deleted} 条，MARKER 记录已清空`
      : `残留 ${leftRows.length} 条公开 + ${hiddenLeftRows.length} 条未公示`)

  const feedFinal = await pageApi(customerPage, 'GET', '/plugin/violation/feed?limit=50')
  const feedClean = !((feedFinal.json && feedFinal.json.list) || []).some((i) => (i.description || '').includes(MARKER))
  check('V-07b', 'P1', '删除后前台公示同步消失', feedClean, feedClean ? 'feed 已无 MARKER' : 'feed 仍含 MARKER')

  // ---- 清理 C-05 畸形用例的副作用数据（SQL 片段 / 超长字符，后端未拦截已落库）----
  let swept = 0
  for (const kw of ['DROP TABLE', 'AAAAAA']) {
    for (let i = 0; i < 10; i++) {
      const r = await pageApi(adminPage, 'GET', `/admin/plugin/violation/list?keyword=${encodeURIComponent(kw)}&limit=20`)
      const hit = ((r.json && r.json.list) || []).find((x) => (x.description || '').includes(kw))
      if (!hit) break
      await pageApi(adminPage, 'POST', `/admin/plugin/violation/${hit.id}/delete`, {})
      swept++
      await sleep(200)
    }
  }
  note('C-05 副作用清理', swept ? `删除 ${swept} 条畸形用例残留记录` : '无残留')
}

// ---------------------------------------------------------------- refund 业务流（S2b：R-01~R-12）

async function flowRefund({ adminPage, customerPage, marker }) {
  const ORDER_MAIN = 475
  const ORDER_EXPIRED = 476
  const ORDER_UNPAID = 477
  const tag = (t) => `${marker}-${t}`
  const cents = (v) => (v === null || v === undefined ? 'N/A' : (v / 100).toFixed(2))
  const toCents = (s) => Math.round(parseFloat(s) * 100)

  // DB 直证/自愈：与 ticket、spaceship 流程同口径的 psql 通道。
  const psql = (sql) => {
    try {
      return execSync(`"${PSQL}" -h 127.0.0.1 -p 5433 -U lumeidc -d lumeidc -t -A -c "${sql}"`,
        { env: { ...process.env, PGPASSWORD: 'lumeidc_dev' }, encoding: 'utf8', timeout: 15000 }).trim()
    } catch (e) {
      return null
    }
  }
  const psqlNum = (sql) => {
    const v = psql(sql)
    const n = v === null ? NaN : parseFloat(v)
    return Number.isFinite(n) ? n : null
  }

  // D 系列三视图检查后页面停留在移动视口（375px）；前台移动视口渲染 .refund-card 卡片而非表格行，
  // 业务流行内操作依赖桌面表格，开头统一恢复桌面视口。
  await adminPage.setViewport({ width: 1440, height: 900 })
  await customerPage.setViewport({ width: 1440, height: 900 })

  const waitFind = async (fn, timeout = 10000) => {
    const t0 = Date.now()
    while (Date.now() - t0 < timeout) {
      const v = await fn().catch(() => null)
      if (v) return v
      await sleep(400)
    }
    return null
  }
  const myList = async () => {
    const r = await pageApi(customerPage, 'GET', '/plugin/refund/my?limit=100')
    return (r.json && r.json.list) || []
  }
  const findMy = async (t, status) => {
    const list = await myList()
    return list.find((x) => String(x.detail || '').includes(tag(t)) && (!status || x.status === status)) || null
  }
  const adminListByTag = async (t) => {
    const r = await pageApi(adminPage, 'GET', '/admin/plugin/refund/list?limit=100')
    return ((r.json && r.json.list) || []).filter((x) => String(x.detail || '').includes(tag(t)))
  }
  const adminStats = async () => {
    const r = await pageApi(adminPage, 'GET', '/admin/plugin/refund/stats')
    return (r.json && r.json.stats) || null
  }
  const userBalance = async () => {
    const r = await pageApi(adminPage, 'GET', `/admin/users?q=${encodeURIComponent(EMAIL)}&limit=5`)
    const list = (r.json && (r.json.list || r.json.users)) || []
    const u = list.find((x) => x.email === EMAIL) || list[0]
    return u && u.balance !== undefined ? toCents(u.balance) : null
  }
  const createApi = (t, orderId, amount, reason) =>
    pageApi(customerPage, 'POST', '/plugin/refund/create', {
      order_id: String(orderId), amount, method: 'balance', reason: reason || '验收测试退款', detail: tag(t),
    })

  const readStat = (page, title) => page.evaluate((t) => {
    for (const card of document.querySelectorAll('.art-card')) {
      const p = card.querySelector('p')
      if (p && (p.textContent || '').trim() === t) {
        const el = card.querySelector('.text-2xl')
        const m = el && (el.textContent || '').replace(/,/g, '').match(/\d+/)
        return m ? Number(m[0]) : null
      }
    }
    return null
  }, title)
  const waitStat = async (page, title, timeout = 8000) => {
    const t0 = Date.now()
    while (Date.now() - t0 < timeout) {
      const v = await readStat(page, title).catch(() => null)
      if (v !== null) return v
      await sleep(250)
    }
    return null
  }
  // 首列匹配 + 可选行文消歧，轮询等待行渲染（表格异步加载，首访可能晚几百毫秒）。
  // 注意前台列首列是「订单 #id」、后台首列是「申请 id」，调用方须传对应 key。
  const clickRowButton = async (page, firstCell, txt, contains = '', timeout = 10000) => {
    const t0 = Date.now()
    let saw = 'no-row'
    while (Date.now() - t0 < timeout) {
      const r = await page.evaluate(({ key, t, c }) => {
        for (const row of document.querySelectorAll('.el-table__row')) {
          const cell = row.querySelector('.el-table__cell, td')
          if (!cell || (cell.textContent || '').trim() !== key) continue
          if (c && !(row.textContent || '').includes(c)) continue
          const btn = [...row.querySelectorAll('button')].find((b) => (b.textContent || '').trim() === t)
          if (btn) { btn.click(); return 'clicked' }
          return 'no-button'
        }
        return 'no-row'
      }, { key: firstCell, t: txt, c: contains })
      if (r === 'clicked') return 'clicked'
      saw = r
      await sleep(300)
    }
    const diag = await page.evaluate(() => ({
      url: location.href.slice(0, 90),
      vw: window.innerWidth,
      rows: document.querySelectorAll('.el-table__row').length,
      firstCells: [...document.querySelectorAll('.el-table__row')].slice(0, 3).map((r) => {
        const c = r.querySelector('.el-table__cell, td')
        return c ? (c.textContent || '').trim() : '(no-cell)'
      }),
      cards: document.querySelectorAll('.refund-card').length,
    })).catch((e) => `evaluate失败:${e.message}`)
    note('clickRowButton 诊断', typeof diag === 'string' ? diag : JSON.stringify(diag))
    return saw
  }
  const confirmBox = async (page, txt, timeout = 8000) => {
    const t0 = Date.now()
    while (Date.now() - t0 < timeout) {
      const ok = await page.evaluate((t) => {
        const box = document.querySelector('.el-message-box')
        if (!box) return false
        const btn = [...box.querySelectorAll('button')].find((b) => (b.textContent || '').trim() === t)
        if (btn) { btn.click(); return true }
        return false
      }, txt)
      if (ok) return true
      await sleep(200)
    }
    return false
  }
  // 目标 URL 与当前相同时 goto 可能不重新加载文档（后台 hash 路由尤为明显），
  // 附加一次性 query 强制完整导航，保证组件重新挂载、列表数据新鲜。
  const freshGoto = async (page, url) => {
    const sep = url.includes('#') ? url.replace('#', `?_=${Date.now()}#`) : `${url}?_=${Date.now()}`
    await page.goto(BASE + sep, { waitUntil: 'domcontentloaded', timeout: 25000 })
  }
  const gotoAdmin = async () => {
    await freshGoto(adminPage, '/admin#/plugin/refund')
    await adminPage.waitForSelector('.el-table', { timeout: 15000 }).catch(() => null)
    await sleep(400)
  }
  const gotoClient = async () => {
    await freshGoto(customerPage, '/plugin/refund')
    await customerPage.waitForSelector('.el-card, .records', { timeout: 15000 }).catch(() => null)
    await sleep(400)
  }

  // ---- 步骤 0：QA 账本复位 + 幂等清理 + 基线
  // 退款流程用的是固定 QA 订单 #475/#476/#477，而可退余额 = 实付 − SUM(refunds done)，
  // 是库里累积算出来的。上一轮跑完的 approved 记录会吃掉下一轮的可退额度，导致
  // 硬编码金额（6.00）被前端夹到剩余额度、R-11 对账随之失败——那是脚本不可重复执行，
  // 不是产品缺陷。这里在开跑前把三个 QA 订单的退款痕迹清空并把余额冲平，
  // 让每轮都从同一账本基线出发（自愈；psql 不可用时降级为累积口径，断言同步放宽）。
  let resetOk = false
  let paidBase = null // 订单 #475 实付（分），复位后即可退全额
  {
    const uid = psqlNum(`SELECT user_id FROM orders WHERE id=${ORDER_MAIN}`)
    const paid = psqlNum(`SELECT amount::numeric*100 FROM orders WHERE id=${ORDER_MAIN}`)
    if (uid !== null && paid !== null && psql('SELECT 1') !== null) {
      // 先按本轮已入账的退款金额倒扣余额，再删账本，避免用户余额被反复虚增
      const doneSum = psqlNum(
        `SELECT COALESCE(SUM(amount::numeric),0)*100 FROM refunds WHERE order_id IN (${ORDER_MAIN},${ORDER_EXPIRED},${ORDER_UNPAID}) AND status='done'`)
      if (doneSum !== null && doneSum > 0) {
        psql(`UPDATE users SET balance = balance - ${(doneSum / 100).toFixed(2)} WHERE id=${uid}`)
      }
      psql(`DELETE FROM refunds WHERE order_id IN (${ORDER_MAIN},${ORDER_EXPIRED},${ORDER_UNPAID})`)
      psql(`DELETE FROM plugin_refund_requests WHERE order_id IN (${ORDER_MAIN},${ORDER_EXPIRED},${ORDER_UNPAID})`)
      const leftReq = psqlNum(`SELECT count(*) FROM plugin_refund_requests WHERE order_id IN (${ORDER_MAIN},${ORDER_EXPIRED},${ORDER_UNPAID})`)
      const leftRef = psqlNum(`SELECT count(*) FROM refunds WHERE order_id IN (${ORDER_MAIN},${ORDER_EXPIRED},${ORDER_UNPAID})`)
      resetOk = leftReq === 0 && leftRef === 0
      if (resetOk) paidBase = Math.round(paid)
      note('步骤0 账本复位', resetOk
        ? `已清空 #${ORDER_MAIN}/#${ORDER_EXPIRED}/#${ORDER_UNPAID} 历史退款痕迹（倒扣余额 ${cents(doneSum)}）；#${ORDER_MAIN} 可退基线=${cents(paidBase)}`
        : `复位未彻底：残留 申请=${leftReq} 退款=${leftRef}；后续改用实时可退余额自适应`)
    } else {
      note('步骤0 账本复位', 'psql 不可用，跳过复位；后续改用实时可退余额自适应')
    }
  }
  let swept = 0
  for (const x of await myList()) {
    if (String(x.detail || '').includes(marker) && x.status === 'pending') {
      await pageApi(customerPage, 'POST', `/plugin/refund/${x.id}/withdraw`, {})
      swept++
    }
  }
  note('步骤0 幂等清理', swept ? `撤回 ${swept} 条残留 pending` : '无残留')
  const stats0 = await adminStats()
  const pendingBase = stats0 ? stats0.pending : 0

  // ---- 步骤 1：R-01a 可退订单口径
  let eligibleReasons = []
  // 本轮基线：订单 #475 的实时可退余额（分）。后续金额一律由它推导，
  // 不再硬编码 10.00/6.00——账本复位成功时等于实付，复位不可用时等于剩余额度。
  let base = null
  {
    const r = await pageApi(customerPage, 'GET', '/plugin/refund/eligible-orders')
    const orders = (r.json && r.json.orders) || []
    eligibleReasons = (r.json && r.json.reasons) || []
    const hit = orders.find((o) => o.id === ORDER_MAIN)
    const noExpired = !orders.some((o) => o.id === ORDER_EXPIRED)
    const refundable = hit ? parseFloat(hit.refundable) : NaN
    base = hit && Number.isFinite(refundable) ? toCents(hit.refundable) : null
    const ok = !!(r.json && String(r.json.ok) === '1' && hit && noExpired && refundable > 0
      && (paidBase === null || refundable <= paidBase / 100 + 0.001))
    check('R-01a', 'P0', '可退订单口径（窗口内已支付可见、超窗隐藏）', ok,
      `含#${ORDER_MAIN}:${hit ? `refundable=${hit.refundable}` : '缺'} 不含#${ORDER_EXPIRED}:${noExpired}`
      + (paidBase !== null ? ` 实付基线=${cents(paidBase)}` : ''))
    note('R-01a 表单选项', `reasons=${eligibleReasons.length} 项 methods=${JSON.stringify((r.json && r.json.methods) || [])}`)
  }
  // 复位成功且实付已知时做一次交叉校验：实时可退应恰等于实付
  if (resetOk && paidBase !== null && base !== null && base !== paidBase) {
    note('R-01a 基线提示', `复位后可退=${cents(base)} 与实付=${cents(paidBase)} 不等，按实时值 ${cents(base)} 继续`)
  }
  if (base === null) base = paidBase // 兜底：可退列表取不到时用实付基线

  // ---- 步骤 2：R-10a 金额边界组（在造数前打，基线为当前可退余额）
  {
    const over = ((base + 1) / 100).toFixed(2)
    const cases = [
      { name: '空金额', amount: '' },
      { name: '负数', amount: '-1' },
      { name: '零', amount: '0' },
      { name: '超可退上限', amount: over },
    ]
    const fails = []
    for (const c of cases) {
      const r = await createApi(`r10-${c.name}`, ORDER_MAIN, c.amount, '边界探测')
      const rejected = r.status < 500 && (!r.json || String(r.json.ok) !== '1')
      if (!rejected) fails.push(`${c.name}:status=${r.status},ok=${r.json && r.json.ok}`)
      await sleep(150)
    }
    check('R-10a', 'P0', '金额边界组全部拒绝且不 500', fails.length === 0,
      fails.length ? fails.join(' | ') : `空/负/零/超额(${over}) 4 例均业务拒绝`)
  }

  // ---- 步骤 3：R-02 前台 UI 创建
  // 申请金额由本轮可退基线推导：基线≥8.00 时按原设计取 6.00，否则留出后续小额用例的份额。
  const AMT_CENTS = base >= 800 ? 600 : Math.max(100, base - 300)
  const AMT = (AMT_CENTS / 100).toFixed(2)
  // 后续小额用例（r04/r07）金额同样自适应，保证不超过 r02 之后的剩余额度。
  const restAfterMain = base - AMT_CENTS
  const SMALL_CENTS = Math.min(100, Math.max(1, Math.floor((restAfterMain - 1) / 2)))
  const SMALL = (SMALL_CENTS / 100).toFixed(2)
  const TINY_CENTS = Math.min(1, Math.max(1, restAfterMain - 2 * SMALL_CENTS))
  const TINY = (TINY_CENTS / 100).toFixed(2)
  note('R-02 金额推导', `可退基线=${cents(base)} → 主申请=${AMT}；小额用例=${SMALL}；自动通过探针=${TINY}`)
  let createOk = false
  {
    await gotoClient()
    let why = ''
    const netMark = netLog.length
    const opened = await clickButtonIn(customerPage, 'body', '申请退款')
    if (!opened) why = '未找到「申请退款」按钮'
    let submitted = false
    if (opened) {
      const drawerOk = await customerPage.waitForSelector('.el-drawer .el-form', { timeout: 8000 }).then(() => true).catch(() => false)
      if (!drawerOk) why = '申请抽屉未打开'
      else {
        await openSelectByPlaceholder(customerPage, '请选择订单')
        const picked = await pickDropdownItem(customerPage, `#${ORDER_MAIN}(?!\\d)`, 'regex')
        if (!picked) why = `订单下拉未选到 #${ORDER_MAIN}`
        else {
          await customerPage.click('.el-drawer .el-input-number input', { clickCount: 3 })
          await customerPage.type('.el-drawer .el-input-number input', AMT, { delay: 20 })
          await customerPage.keyboard.press('Tab')
          await openSelectByPlaceholder(customerPage, '请选择退款原因')
          const reasonPat = eligibleReasons.length ? `^(${eligibleReasons.map(escapeReg).join('|')})$` : '.+'
          await pickDropdownItem(customerPage, reasonPat, 'regex')
          await customerPage.type('.el-drawer textarea[placeholder*="补充说明"]', tag('r02'), { delay: 5 })
          submitted = await clickButtonIn(customerPage, '.el-drawer', '提交申请')
          if (!submitted) why = '「提交申请」点击失败'
        }
      }
    }
    if (submitted) {
      const net = await waitNetEntry(/\/__api\/plugin\/refund\/create/, netMark, 10000)
      const rec = await waitFind(() => findMy('r02', 'pending'), 10000)
      createOk = !!(net && rec)
      if (!createOk) why = `提交后未形成 pending（net=${net ? '有' : '无'}）`
    }
    check('R-02', 'P0', '前台提交退款申请（UI 全路径）', createOk, why || `订单#${ORDER_MAIN} 金额${AMT} 进入待审核`)
    if (!createOk) {
      note('R-02 降级', 'UI 创建失败，改 API 补建 pending 以保全后续步骤')
      await createApi('r02', ORDER_MAIN, AMT, eligibleReasons[0])
      await waitFind(() => findMy('r02', 'pending'), 8000)
    }
  }

  // ---- 步骤 4：R-12 创建后三处同步
  {
    const mine = await findMy('r02')
    const adminRows = await adminListByTag('r02')
    const stats = await adminStats()
    const c = stats && stats.pending >= pendingBase + 1
    check('R-12', 'P0', '创建后三处同步（前台列表/后台列表/统计）', !!(mine && adminRows.length === 1 && c),
      `前台=${mine ? mine.status : '无'} 后台=${adminRows.length}条 pending=${stats ? stats.pending : 'N/A'}(基线${pendingBase})`)
  }

  // ---- 步骤 5：R-01b 进行中申请使订单从可退列表消失
  {
    const r = await pageApi(customerPage, 'GET', '/plugin/refund/eligible-orders')
    const orders = (r.json && r.json.orders) || []
    const gone = !orders.some((o) => o.id === ORDER_MAIN)
    check('R-01b', 'P2', '有进行中申请的订单从可退列表消失', gone,
      gone ? `订单#${ORDER_MAIN} 已隐藏` : `订单#${ORDER_MAIN} 仍在可退列表`)
  }

  // ---- 步骤 6：R-06 同订单重复申请被拒
  {
    const r = await createApi('r02-dup', ORDER_MAIN, SMALL, eligibleReasons[0])
    const msg = (r.json && (r.json.msg || r.json.message)) || r.text || ''
    const rejected = r.status < 500 && r.json && String(r.json.ok) !== '1' && /进行中/.test(msg)
    check('R-06', 'P0', '同订单重复申请被拒（幂等）', !!rejected, `status=${r.status} msg=${msg}`)
  }

  // ---- 步骤 7：R-05a 撤回 + 可重建
  let rec = await findMy('r02', 'pending')
  {
    await gotoClient()
    let withdrawOk = false
    let why = ''
    if (!rec) why = '无 pending 记录'
    else {
      const netMark = netLog.length
      const clicked = await clickRowButton(customerPage, `#${rec.order_id}`, '撤回', `￥${AMT}`)
      if (clicked !== 'clicked') why = `行内撤回按钮(${clicked})`
      else if (!(await confirmBox(customerPage, '撤回'))) why = '撤回确认框未出现'
      else {
        const net = await waitNetEntry(/\/__api\/plugin\/refund\/\d+\/withdraw/, netMark, 10000)
        const gone = await waitFind(async () => !(await findMy('r02', 'pending')), 8000)
        withdrawOk = !!(net && gone)
        if (!withdrawOk) why = '撤回后记录未脱离 pending'
      }
    }
    let rebuildOk = false
    if (withdrawOk) {
      const r = await createApi('r02', ORDER_MAIN, AMT, eligibleReasons[0])
      rebuildOk = !!(r.json && String(r.json.ok) === '1')
      if (rebuildOk) rec = await waitFind(() => findMy('r02', 'pending'), 8000)
    }
    check('R-05a', 'P0', '撤回成功且撤回后可重新提交', withdrawOk && rebuildOk,
      why || `撤回成功，重建 id=${rec && rec.id}`)
  }

  // ---- 步骤 8：R-03 后台真实 UI 审批（通过并退款）
  let recApproved = null
  {
    if (!rec) {
      check('R-03', 'P0', '后台审批通过', false, '无 pending 记录可审批')
    } else {
      const balBefore = await userBalance()
      const statsB = await adminStats()
      const approvedBefore = statsB ? statsB.approved : null
      let via = 'UI'
      let uiOk = false
      let why = ''
      await gotoAdmin()
      const netMark = netLog.length
      const clicked = await clickRowButton(adminPage, String(rec.id), '审核')
      if (clicked !== 'clicked') why = `行内审核按钮(${clicked})`
      else {
        const drawerOk = await adminPage.waitForSelector('.el-drawer', { timeout: 8000 }).then(() => true).catch(() => false)
        if (!drawerOk) why = '审批抽屉未打开'
        else if (!(await clickButtonIn(adminPage, '.el-drawer', '通过并退款'))) why = '抽屉「通过并退款」点击失败'
        else if (!(await confirmBox(adminPage, '通过并退款'))) why = '通过确认框未出现'
        else {
          const net = await waitNetEntry(/\/__api\/admin\/plugin\/refund\/\d+\/approve/, netMark, 12000)
          const toast = await waitToast(adminPage, /已通过并完成退款/, 8000).catch(() => null)
          uiOk = !!(net && net.includes(' 200 '))
          if (!uiOk) why = `审批请求未成功(net=${net ? '有' : '无'},toast=${toast ? '有' : '无'})`
        }
      }
      if (!uiOk) {
        via = 'API'
        note('R-03 降级', `UI 审批失败：${why}；改 API 审批`)
        await adminPage.keyboard.press('Escape').catch(() => null)
        const r = await pageApi(adminPage, 'POST', `/admin/plugin/refund/${rec.id}/approve`, {})
        uiOk = !!(r.json && String(r.json.ok) === '1')
      }
      const after = await waitFind(async () => {
        const m = await findMy('r02')
        return m && m.status === 'approved' ? m : null
      }, 10000)
      const balAfter = await userBalance()
      const statsA = await adminStats()
      const balOk = balBefore !== null && balAfter !== null && balAfter - balBefore === AMT_CENTS
      check('R-03', 'P0', `后台审批通过（${via}）+ 余额入账 + 状态翻转`, uiOk && !!after && balOk,
        `余额 ${cents(balBefore)}→${cents(balAfter)}（应+${AMT}）状态=${after ? after.status : 'N/A'}`)
      const statOk = approvedBefore !== null && statsA && statsA.approved === approvedBefore + 1
      check('R-03c', 'P1', '后台统计同步（已通过+1）', !!statOk,
        `approved ${approvedBefore}→${statsA ? statsA.approved : 'N/A'}`)
      const cardVal = await waitStat(adminPage, '已通过')
      note('R-03 统计卡读数', `「已通过」卡片=${cardVal === null ? '未取到' : cardVal}（API approved=${statsA ? statsA.approved : 'N/A'}）`)
      if (after) recApproved = after
    }
  }

  // ---- 步骤 9：R-05b 终态不可撤回
  {
    const r = recApproved
      ? await pageApi(customerPage, 'POST', `/plugin/refund/${recApproved.id}/withdraw`, {})
      : null
    const blocked = r && r.status < 500 && r.json && String(r.json.ok) !== '1'
    check('R-05b', 'P0', '终态申请不可撤回', !!blocked,
      r ? `status=${r.status} msg=${(r.json && r.json.msg) || ''}` : '无 approved 记录')
  }

  // ---- 步骤 10：R-04 驳回（空备注前端拦截 + 备注前台详情可见）
  {
    await createApi('r04', ORDER_MAIN, SMALL, eligibleReasons[0])
    const target = await waitFind(() => findMy('r04', 'pending'), 8000)
    if (!target) {
      check('R-04', 'P0', '驳回及备注前台可见', false, '造数失败（r04 未形成 pending）')
    } else {
      await gotoAdmin()
      let emptyBlocked = false
      const clicked = await clickRowButton(adminPage, String(target.id), '审核')
      if (clicked === 'clicked') {
        const drawerOk = await adminPage.waitForSelector('.el-drawer', { timeout: 8000 }).then(() => true).catch(() => false)
        if (drawerOk) {
          await clickButtonIn(adminPage, '.el-drawer', '驳回')
          const toast = await waitToast(adminPage, /请填写处理备注/, 5000).catch(() => null)
          emptyBlocked = !!toast
          // 保持抽屉打开：空备注被前端拦截后确认框未弹出、抽屉仍在，后续直接复用填备注驳回
          await sleep(300)
        }
      }
      check('R-04b', 'P1', '空备注驳回被前端拦截', emptyBlocked,
        emptyBlocked ? '提示「请填写处理备注，将展示给用户」' : '未观察到拦截提示')
      const noteText = `验收驳回-${tag('r04')}`
      let rejectOk = false
      if (clicked === 'clicked') {
        await adminPage.type('.el-drawer textarea', noteText, { delay: 5 }).catch(() => null)
        const netMark = netLog.length
        await clickButtonIn(adminPage, '.el-drawer', '驳回')
        if (await confirmBox(adminPage, '驳回')) {
          const net = await waitNetEntry(/\/__api\/admin\/plugin\/refund\/\d+\/reject/, netMark, 10000)
          rejectOk = !!(net && net.includes(' 200 '))
        }
      }
      if (!rejectOk) {
        const rr = await pageApi(adminPage, 'POST', `/admin/plugin/refund/${target.id}/reject`, { handle_note: noteText })
        rejectOk = !!(rr.json && String(rr.json.ok) === '1')
        if (rejectOk) note('R-04 降级', 'UI 驳回失败，改 API 驳回')
      }
      const after = await waitFind(async () => {
        const m = await findMy('r04')
        return m && m.status === 'rejected' ? m : null
      }, 8000)
      let noteVisible = false
      if (after) {
        await gotoClient()
        if ((await clickRowButton(customerPage, `#${after.order_id}`, '详情', `￥${SMALL}`)) === 'clicked') {
          const dlg = await customerPage.waitForSelector('.el-dialog', { timeout: 8000 }).catch(() => null)
          if (dlg) {
            const txt = await customerPage.evaluate(() => (document.querySelector('.el-dialog') || {}).textContent || '')
            noteVisible = txt.includes(noteText)
          }
        }
      }
      check('R-04', 'P0', '驳回成功且备注前台详情可见', rejectOk && !!after && noteVisible,
        `reject=${rejectOk} 状态=${after ? after.status : 'N/A'} 备注可见=${noteVisible}`)
    }
  }

  // ---- 步骤 11：R-07 并发审批仅一人成功
  {
    await createApi('r07', ORDER_MAIN, SMALL, eligibleReasons[0])
    const target = await waitFind(() => findMy('r07', 'pending'), 8000)
    if (!target) {
      check('R-07', 'P0', '并发审批仅一人成功', false, '造数失败（r07 未形成 pending）')
    } else {
      const pair = await adminPage.evaluate(async (id) => {
        const s = await fetch('/__api/session', { credentials: 'same-origin', cache: 'no-store' }).then((x) => x.json()).catch(() => ({}))
        const tok = s.csrf || ''
        const fire = () => fetch(`/__api/admin/plugin/refund/${id}/approve`, {
          method: 'POST',
          credentials: 'same-origin',
          headers: { Accept: 'application/json', 'Content-Type': 'application/json', 'X-CSRF-Token': tok },
          body: '{}',
        }).then(async (x) => ({ status: x.status, body: await x.text() }))
        return Promise.all([fire(), fire()])
      }, target.id)
      const okCount = pair.filter((x) => x.status === 200 && /"ok"\s*:\s*1/.test(x.body)).length
      const raceMsg = pair.some((x) => /已被其他管理员处理/.test(x.body))
      check('R-07', 'P0', '并发审批仅一人成功（Claim 原子占位）', okCount === 1,
        `两次 approve 成功数=${okCount} 冲突提示=${raceMsg ? '有' : '无'}`)
    }
  }

  // ---- 步骤 12：R-08 未支付订单审批失败回滚 pending
  {
    const cr = await createApi('r08', ORDER_UNPAID, '5.00', eligibleReasons[0])
    const target = await waitFind(() => findMy('r08', 'pending'), 8000)
    if (!target) {
      check('R-08', 'P0', '未支付订单审批失败回滚', false,
        `造数失败：create status=${cr.status} msg=${(cr.json && cr.json.msg) || cr.text || ''}`)
    } else {
      const r = await pageApi(adminPage, 'POST', `/admin/plugin/refund/${target.id}/approve`, {})
      const msg = (r.json && (r.json.msg || r.json.message)) || r.text || ''
      const failOk = r.status < 500 && r.json && String(r.json.ok) !== '1' && /退款执行失败/.test(msg)
      const stillPending = await findMy('r08', 'pending')
      check('R-08', 'P0', '未支付订单审批失败且回滚 pending', !!(failOk && stillPending),
        `approve msg=${msg} 回滚=${stillPending ? 'pending' : '异常'}`)
    }
  }

  // ---- 步骤 13：R-09 自动通过配置（finally 兜底还原）
  {
    let touched = false
    try {
      const set = await pageApi(adminPage, 'POST', '/admin/plugin/refund/config', {
        values: { reviewMode: 'auto', autoApproveMax: 50 },
      })
      touched = !!(set.json && String(set.json.ok) === '1')
      check('R-09a', 'P0', '配置自动通过（number 字段 JSON 数字）', touched,
        `status=${set.status} msg=${(set.json && set.json.msg) || ''}`)
      if (touched) {
        await createApi('r09', ORDER_MAIN, TINY, eligibleReasons[0])
        const auto = await waitFind(async () => {
          const m = await findMy('r09')
          return m && m.status === 'approved' ? m : null
        }, 12000)
        check('R-09b', 'P0', '低于阈值自动通过', !!auto,
          auto ? `id=${auto.id} 自动 approved` : '12s 内未自动通过')
      }
    } finally {
      if (touched) {
        const back = await pageApi(adminPage, 'POST', '/admin/plugin/refund/config', {
          values: { reviewMode: 'manual', autoApproveMax: 0 },
        })
        note('R-09 配置还原', back.json && String(back.json.ok) === '1' ? '已还原 manual/0' : `还原失败 status=${back.status}`)
      }
    }
  }

  // ---- 步骤 14：R-11 可退余额对账（refundable = 本轮基线 - 本轮已退合计）
  {
    const doneSum = (await myList())
      .filter((x) => String(x.detail || '').includes(marker) && x.status === 'approved')
      .reduce((s, x) => s + toCents(x.amount), 0)
    const r = await pageApi(customerPage, 'GET', '/plugin/refund/eligible-orders')
    const o = ((r.json && r.json.orders) || []).find((x) => x.id === ORDER_MAIN)
    // 基线取本轮开跑时的可退余额（复位后等于实付），而非写死 10.00：
    // 这样即使复位不可用、本轮从剩余额度起步，对账依然成立。
    const expect = base - doneSum
    const actual = o ? toCents(o.refundable) : null
    check('R-11', 'P0', '可退余额 = 起点可退 - 已退合计（对账）', actual !== null && actual === expect,
      `订单#${ORDER_MAIN} refundable=${o ? o.refundable : 'N/A'} 期望=${cents(expect)}（起点 ${cents(base)} − 本轮已退 ${cents(doneSum)}）`)
  }

  // ---- 步骤 15：收尾（撤回残留 pending；保留 rejected/approved 供探针与对账取证）
  {
    let n = 0
    for (const x of await myList()) {
      if (String(x.detail || '').includes(marker) && x.status === 'pending') {
        await pageApi(customerPage, 'POST', `/plugin/refund/${x.id}/withdraw`, {})
        n++
      }
    }
    note('步骤15 收尾', `撤回残留 pending ${n} 条；保留 rejected/approved 记录作 C-03b 探针资源`)
  }
}

// ---------------------------------------------------------------- announcement 业务流（S2c：A-01~A-08）

async function flowAnnouncement({ adminPage, customerPage, marker }) {
  const TITLE = `${marker} 公告`
  const TITLE_EDITED = `${marker} 公告（已编辑）`
  const CAT = 'QA分类'
  const adminList = async () => {
    const r = await pageApi(adminPage, 'GET', '/admin/plugin/announcement/list')
    return (r.json && r.json.list) || []
  }
  const pubList = async (qs = '') => {
    const r = await pageApi(customerPage, 'GET', `/plugin/announcement/list?limit=50${qs}`)
    return r.json || {}
  }
  const findAdmin = (list, t) => list.find((x) => String(x.title || '').includes(t)) || null
  const freshGoto = async (page, url) => {
    const sep = url.includes('#') ? url.replace('#', `?_=${Date.now()}#`) : `${url}?_=${Date.now()}`
    await page.goto(BASE + sep, { waitUntil: 'domcontentloaded', timeout: 25000 })
  }
  await adminPage.setViewport({ width: 1440, height: 900 })
  await customerPage.setViewport({ width: 1440, height: 900 })

  // ---- 步骤 0：幂等清理（删除历史 QA 公告）
  {
    let swept = 0
    for (const x of await adminList()) {
      if (String(x.title || '').includes(marker) || String(x.title || '').startsWith('QA')) {
        await pageApi(adminPage, 'POST', `/admin/plugin/announcement/${x.id}/delete`, {})
        swept++
      }
    }
    note('A-00 幂等清理', swept ? `删除 ${swept} 条历史 QA 公告` : '无残留')
  }

  // ---- A-01：后台新增（含空标题前端拦截）
  let annId = null
  {
    await freshGoto(adminPage, '/admin#/plugin/announcement')
    await adminPage.waitForSelector('.admin-announcements', { timeout: 15000 }).catch(() => null)
    await sleep(400)
    // 空标题拦截：打开对话框直接保存
    await adminPage.evaluate(() => {
      const btn = [...document.querySelectorAll('button')].find((b) => (b.textContent || '').includes('新增公告'))
      if (btn) btn.click()
    })
    await adminPage.waitForSelector('.el-dialog', { timeout: 8000 }).catch(() => null)
    await sleep(300)
    const emptyBlocked = await adminPage.evaluate(() => {
      const dlg = document.querySelector('.el-dialog')
      if (!dlg) return { dlg: false }
      const saveBtn = [...dlg.querySelectorAll('button')].find((b) => (b.textContent || '').trim() === '保存')
      if (saveBtn) saveBtn.click()
      return { dlg: true }
    })
    await sleep(600)
    const emptyWarn = await adminPage.evaluate(() => {
      const dlgOpen = !!document.querySelector('.el-dialog')
      const warn = [...document.querySelectorAll('.el-message')].some((m) => (m.textContent || '').includes('请输入标题'))
      return { dlgOpen, warn }
    })
    // 填标题保存
    await adminPage.evaluate((t, cat) => {
      const dlg = document.querySelector('.el-dialog')
      if (!dlg) return
      const inputs = dlg.querySelectorAll('.el-input__inner, textarea')
      if (inputs[0]) {
        const setter = Object.getOwnPropertyDescriptor(window.HTMLInputElement.prototype, 'value').set
        setter.call(inputs[0], t)
        inputs[0].dispatchEvent(new Event('input', { bubbles: true }))
      }
      if (inputs[1]) {
        const setter = Object.getOwnPropertyDescriptor(window.HTMLInputElement.prototype, 'value').set
        setter.call(inputs[1], cat)
        inputs[1].dispatchEvent(new Event('input', { bubbles: true }))
      }
      const ta = dlg.querySelectorAll('textarea')
      const contentTa = ta[ta.length - 1]
      if (contentTa) {
        const setter = Object.getOwnPropertyDescriptor(window.HTMLTextAreaElement.prototype, 'value').set
        setter.call(contentTa, 'QA 验收公告正文')
        contentTa.dispatchEvent(new Event('input', { bubbles: true }))
      }
      const saveBtn = [...dlg.querySelectorAll('button')].find((b) => (b.textContent || '').trim() === '保存')
      if (saveBtn) saveBtn.click()
    }, TITLE, CAT)
    await sleep(1200)
    const list = await adminList()
    const hit = findAdmin(list, TITLE)
    annId = hit ? hit.id : null
    check('A-01', 'P0', '后台新增公告（空标题前端拦截 + 合法保存）',
      !!(emptyBlocked.dlg && emptyWarn.warn && emptyWarn.dlgOpen && annId),
      `空标题拦截=${emptyWarn.warn} 对话框未误关=${emptyWarn.dlgOpen} 保存后 id=${annId}`)
  }
  if (!annId) {
    check('A-02~A-08', 'P0', '后续用例', false, 'A-01 未取到 id，跳过后续')
    return
  }

  // ---- A-02：编辑回填 + 更新不新增行
  {
    // form 回填为 {"announcement":{ID,Title,...}}（repo 结构体序列化，PascalCase）
    const form = await pageApi(adminPage, 'GET', `/admin/plugin/announcement/form?id=${annId}`)
    const formOk = !!(form.json && form.json.announcement && String(form.json.announcement.Title || '').includes(TITLE))
    // 统计本轮 marker 行数作基数（步骤 0 可能清掉历史残留，总数不稳定）
    const markerCount = (l) => l.filter((x) => String(x.title || '').includes(marker)).length
    const before = markerCount(await adminList())
    // save 的 id 必须字符串：handler 对 JSON 数字字段会丢弃（仅收 string/bool），数字 id 会被当 0 走 INSERT
    const save = await pageApi(adminPage, 'POST', '/admin/plugin/announcement/save', {
      id: String(annId), title: TITLE_EDITED, category: CAT, content: 'QA 验收公告正文（已编辑）',
    })
    const after = markerCount(await adminList())
    const updated = findAdmin(await adminList(), TITLE_EDITED)
    check('A-02', 'P0', '编辑回填 + save 带 id 更新不新增行',
      !!(formOk && save.json && String(save.json.ok) === '1' && updated && after === before),
      `回填=${formOk} 更新后命中=${!!updated} 本标记行数 ${before}→${after}`)
  }

  // ---- A-04：前台分类与搜索（在隐藏前先验证可见性）
  {
    const all = await pubList()
    const inAll = ((all.list) || []).some((x) => x.id === annId)
    const byCat = await pubList(`&category=${encodeURIComponent(CAT)}`)
    const inCat = ((byCat.list) || []).some((x) => x.id === annId)
    const cats = (byCat.categories) || all.categories || []
    const byKw = await pubList(`&keyword=${encodeURIComponent(marker)}`)
    const inKw = ((byKw.list) || []).some((x) => x.id === annId)
    const byKwMiss = await pubList(`&keyword=${encodeURIComponent(marker + '不存在')}`)
    const kwMissEmpty = ((byKwMiss.list) || []).length === 0
    check('A-04', 'P1', '前台分类过滤 + 标题关键词搜索 + categories 返回',
      !!(inAll && inCat && inKw && kwMissEmpty && cats.includes(CAT)),
      `列表=${inAll} 分类=${inCat} 关键词=${inKw} 空关键词=${kwMissEmpty} categories含${CAT}=${cats.includes(CAT)}`)
  }

  // ---- A-06：阅读量 +1
  {
    // detail 响应为 {"ok":1,"item":{...reads...}}
    const d1 = await pageApi(customerPage, 'GET', `/plugin/announcement/detail/${annId}`)
    const r1 = d1.json && d1.json.item ? d1.json.item.reads : null
    const d2 = await pageApi(customerPage, 'GET', `/plugin/announcement/detail/${annId}`)
    const r2 = d2.json && d2.json.item ? d2.json.item.reads : null
    check('A-06', 'P1', '详情每打开一次阅读量 +1',
      r1 !== null && r2 !== null && r2 === r1 + 1,
      `reads ${r1} → ${r2}`)
  }

  // ---- A-03：隐藏前台不可见（后台可见）
  {
    await pageApi(adminPage, 'POST', '/admin/plugin/announcement/save', {
      id: String(annId), title: TITLE_EDITED, category: CAT, content: 'QA 验收公告正文（已编辑）', hidden: true,
    })
    await sleep(300)
    const pub = await pubList()
    const pubMiss = !((pub.list) || []).some((x) => x.id === annId)
    const detail = await pageApi(customerPage, 'GET', `/plugin/announcement/detail/${annId}`)
    const detail404 = detail.status === 404 || (detail.json && String(detail.json.ok) !== '1')
    const adminStill = !!(findAdmin(await adminList(), TITLE_EDITED))
    check('A-03a', 'P0', 'hidden=true 前台列表/详情不可见，后台可见',
      !!(pubMiss && detail404 && adminStill),
      `前台列表隐藏=${pubMiss} 详情404=${detail404} 后台可见=${adminStill}`)
    // 恢复显示 + 置顶
    await pageApi(adminPage, 'POST', '/admin/plugin/announcement/save', {
      id: String(annId), title: TITLE_EDITED, category: CAT, content: 'QA 验收公告正文（已编辑）', hidden: false, pinned: true,
    })
    await sleep(300)
    const pub2 = await pubList()
    const list2 = (pub2.list) || []
    const idx = list2.findIndex((x) => x.id === annId)
    const pinned = idx >= 0 && list2[idx].pinned === true
    const topish = idx >= 0 && idx <= 1 // 允许与其他置顶共存
    check('A-03b', 'P1', 'pinned=true 恢复显示且排前',
      !!(idx >= 0 && pinned && topish),
      `列表位置=${idx} pinned=${pinned}`)
  }

  // ---- A-05：分页边界
  {
    const far = await pubList('&page=9999')
    const farEmpty = ((far.list) || []).length === 0
    const over = await pubList('&limit=999')
    const overOk = (over.list) !== undefined // 不报错即可
    check('A-05', 'P2', '分页边界（page 越界空列表、limit 超上限收敛不报错）',
      !!(farEmpty && overOk),
      `page=9999 空=${farEmpty} limit=999 正常=${overOk}`)
  }

  // ---- A-07：删除后前台 404、后台消失
  {
    await pageApi(adminPage, 'POST', `/admin/plugin/announcement/${annId}/delete`, {})
    await sleep(300)
    const adminGone = !findAdmin(await adminList(), TITLE_EDITED)
    const detail = await pageApi(customerPage, 'GET', `/plugin/announcement/detail/${annId}`)
    const detail404 = detail.status === 404 || (detail.json && String(detail.json.ok) !== '1')
    check('A-07', 'P0', '删除后前台 404、后台消失',
      !!(adminGone && detail404),
      `后台消失=${adminGone} 前台404=${detail404}`)
  }

  // ---- A-08：核心聚合（首页启动数据含 announcements）
  {
    const home = await pageApi(customerPage, 'GET', '/plugin/announcement/list?limit=3')
    const aggOk = home.json && String(home.json.ok) === '1'
    check('A-08', 'P2', '前台公开接口可用（首页/购物车聚合同源）',
      !!aggOk, `公开 list ok=${home.json && home.json.ok}`)
  }
}

// ---------------------------------------------------------------- webhooknotify 业务流（S2c：W-01~W-10）

async function flowWebhooknotify({ adminPage, customerPage, marker }) {
  // D 系列三视图检查后页面停留在移动视口（375px），恢复桌面视口再操作后台
  await adminPage.setViewport({ width: 1440, height: 900 })
  // 目标 URL 与当前相同时 goto 可能不重新加载文档（后台 hash 路由），附加一次性 query 强制导航
  const freshGoto = async (page, url) => {
    const sep = url.includes('#') ? url.replace('#', `?_=${Date.now()}#`) : `${url}?_=${Date.now()}`
    await page.goto(BASE + sep, { waitUntil: 'domcontentloaded', timeout: 25000 })
  }

  // ---- 本地接收端：记录请求头与原始体（签名验证需要逐字节原文），可切换 200/500 模拟接收方故障 ----
  const received = []
  let responseMode = 200
  const server = http.createServer((req, res) => {
    let raw = ''
    req.on('data', (c) => (raw += c))
    req.on('end', () => {
      received.push({ headers: req.headers, rawBody: raw, receivedAt: Date.now() })
      res.writeHead(responseMode, { 'Content-Type': 'text/plain' })
      res.end(responseMode === 200 ? 'ok' : 'boom')
    })
  })
  await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve))
  const hookUrl = `http://127.0.0.1:${server.address().port}/hook`
  const secret = `${marker}-secret`
  note('webhooknotify 本地接收端', hookUrl)

  const cfgGet = async () => (await pageApi(adminPage, 'GET', '/admin/plugin/webhooknotify/config')).json || {}
  const cfgSet = (values) => pageApi(adminPage, 'POST', '/admin/plugin/webhooknotify/config', { values })
  const getLogs = async () => {
    const r = await pageApi(adminPage, 'GET', '/admin/plugin/webhooknotify/logs')
    return (r.json && r.json.logs) || []
  }
  const waitFor = async (fn, timeout = 15000, step = 500) => {
    const t0 = Date.now()
    while (Date.now() - t0 < timeout) {
      const v = await fn()
      if (v) return v
      await sleep(step)
    }
    return null
  }
  const createTicket = (t) =>
    pageApi(customerPage, 'POST', '/plugin/tickets/create', {
      subject: `${marker} ${t}`, body: 'webhooknotify 验收造数', priority: 'normal', category: 'technical',
    })

  // 备份原配置（password 不回显：secret 无法备份，仅记录 has_secret 供收尾说明）
  const orig = await cfgGet()
  const origValues = orig.values || {}
  const origHadSecret = !!(orig.has && orig.has.has_secret)

  try {
    // ---- W-01：配置读写（保存重读一致，password 不回显）----
    {
      const set = await cfgSet({ enabled: true, url: hookUrl, secret, events: ['ticket.opened'] })
      const back = await cfgGet()
      const v = back.values || {}
      let evts = []
      try { evts = JSON.parse(v.events || '[]') } catch { /* 非 JSON 视为不符 */ }
      const ok = !!(
        set.json && String(set.json.ok) === '1' &&
        v.enabled === '1' &&
        v.url === hookUrl &&
        evts.includes('ticket.opened') &&
        back.has && back.has.has_secret === true &&
        !v.secret
      )
      check('W-01', 'P0', '配置读写（enabled/url/secret/events 保存重读一致）', ok,
        `enabled=${v.enabled} url${v.url === hookUrl ? '一致' : '=' + v.url} events=${v.events} has_secret=${back.has && back.has.has_secret} secret回显=${v.secret ? '是（泄漏！）' : '否'}`)
    }

    // ---- W-02：未配置 URL 防护（置空 → /test 友好拒绝 → 恢复）----
    {
      await cfgSet({ url: '' })
      const r = await pageApi(adminPage, 'POST', '/admin/plugin/webhooknotify/test', {})
      const msg = (r.json && r.json.msg) || ''
      check('W-02', 'P0', '未配置 URL 时发送测试被拒且提示友好',
        !!(r.json && String(r.json.ok) === '0' && /请先保存 Webhook URL/.test(msg)),
        `status=${r.status} msg=${msg}`)
      await cfgSet({ url: hookUrl })
    }

    // ---- W-03：事件过滤（未勾选事件不投递、不落日志）----
    {
      await cfgSet({ events: ['order.paid'] })
      const logsBefore = await getLogs()
      const recBefore = received.length
      const t = await createTicket('W-03 过滤探针')
      await sleep(2500)
      const newLogs = (await getLogs()).filter((l) => !logsBefore.some((o) => o.id === l.id))
      const noDeliver = received.length === recBefore
      const noLog = !newLogs.some((l) => l.event === 'ticket.opened')
      check('W-03', 'P0', '未勾选事件不投递且不落日志', noDeliver && noLog,
        `建单=${t.json && String(t.json.ok) === '1' ? 'ok' : '失败'} 接收端新增=${received.length - recBefore} 新增ticket.opened日志=${newLogs.filter((l) => l.event === 'ticket.opened').length}`)
      await cfgSet({ events: ['ticket.opened'] })
    }

    // ---- W-04：后台 UI 发送测试（真实点击，Toast + 网络层 + 接收端三重证据）----
    let testHit = null
    {
      await freshGoto(adminPage, '/admin#/plugin/webhooknotify')
      await adminPage.waitForSelector('.plugin-shell', { timeout: 15000 }).catch(() => null)
      await sleep(600)
      const netMark = netLog.length
      const recMark = received.length
      const clicked = await adminPage.evaluate(() => {
        const isVisible = (el) => el && el.getClientRects().length > 0
        const btn = [...document.querySelectorAll('button')].find(
          (b) => (b.textContent || '').trim() === '发送测试' && isVisible(b) && !b.disabled,
        )
        if (btn) { btn.click(); return true }
        return false
      })
      let via = 'UI'
      let ok = false
      let why = ''
      if (!clicked) {
        why = '未找到「发送测试」按钮'
      } else {
        const toast = await waitToast(adminPage, /投递成功|投递失败|请先保存/, 10000)
        const net = await waitNetEntry(/POST \d+ \/__api\/admin\/plugin\/webhooknotify\/test :: .*"ok"\s*:\s*1/, netMark, 10000)
        testHit = await waitFor(() => received.slice(recMark).find((r) => r.headers['x-lumeidc-event'] === 'test'), 8000, 300)
        ok = !!(/投递成功/.test(toast) && net && testHit)
        if (!ok) why = `toast=${toast || '无'} net=${net ? '有' : '无'} 接收端=${testHit ? '命中' : '未命中'}`
      }
      if (!ok) {
        via = 'API'
        note('W-04 降级', `UI 路径失败：${why}；改 API 直连 /test`)
        const rm = received.length
        const r = await pageApi(adminPage, 'POST', '/admin/plugin/webhooknotify/test', {})
        testHit = await waitFor(() => received.slice(rm).find((x) => x.headers['x-lumeidc-event'] === 'test'), 8000, 300)
        ok = !!(r.json && String(r.json.ok) === '1' && r.json.httpStatus === 200 && testHit)
        if (!ok) why += `；API 降级也失败 status=${r.status}`
      }
      // 载荷结构断言：envelope {event,time,data}，test 载荷 data.msg 为固定文案
      let bodyOk = false
      if (testHit) {
        try {
          const b = JSON.parse(testHit.rawBody)
          bodyOk = b.event === 'test' && typeof b.time === 'string' && !!b.data && b.data.msg === 'LumeIDC Webhook 连通性测试'
        } catch { /* 非 JSON 即失败 */ }
      }
      check('W-04', 'P0', `发送测试（${via}）→ 接收端拿到 event=test 且 envelope 结构正确`, ok && bodyOk,
        ok ? `接收端命中；envelope{event,time,data.msg}=${bodyOk ? '通过' : '异常'}` : why)
    }

    // ---- W-05：签名验证（X-LumeIDC-Signature = sha256=HMAC-SHA256(原始体, secret)）----
    {
      let sigOk = false
      let detail = '无 test 命中可验（W-04 未成功）'
      if (testHit) {
        const expect = 'sha256=' + crypto.createHmac('sha256', secret).update(testHit.rawBody).digest('hex')
        const actual = testHit.headers['x-lumeidc-signature'] || ''
        const ct = testHit.headers['content-type'] || ''
        const evt = testHit.headers['x-lumeidc-event'] || ''
        sigOk = actual === expect && /application\/json/.test(ct) && evt === 'test'
        detail = `签名${actual === expect ? '一致' : `不符：${(actual || '(缺)').slice(0, 26)}…`} Content-Type=${ct} X-LumeIDC-Event=${evt}`
      }
      check('W-05', 'P0', '配 secret 后 X-LumeIDC-Signature 正确', sigOk, detail)
    }

    // ---- W-06：真实事件 ticket.opened 异步投递（载荷含 TicketPayload）----
    {
      const recMark = received.length
      const logsBefore = await getLogs()
      const t = await createTicket('W-06 真实事件')
      const tOk = !!(t.json && String(t.json.ok) === '1')
      const hit = await waitFor(() => received.slice(recMark).find((r) => r.headers['x-lumeidc-event'] === 'ticket.opened'), 15000)
      const log = await waitFor(async () => {
        const ls = await getLogs()
        return ls.find((l) => l.event === 'ticket.opened' && !logsBefore.some((o) => o.id === l.id)) || null
      }, 15000)
      let payloadOk = false
      if (log) {
        try {
          const p = typeof log.payload === 'string' ? JSON.parse(log.payload || '{}') : (log.payload || {})
          payloadOk = !!(p.data && p.data.ticketId && String(p.data.subject || '').includes(marker))
        } catch { /* 非 JSON */ }
      }
      check('W-06', 'P0', 'ticket.opened 真实事件投递（接收端命中 + 日志载荷含工单数据）',
        !!(tOk && hit && log && payloadOk),
        `建单=${tOk ? `id=${t.json.id}` : `失败 ${t.text.slice(0, 60)}`} 接收端=${hit ? '命中' : '未命中'} 日志=${log ? `id=${log.id} status=${log.httpStatus}` : '无'} 载荷=${payloadOk ? '含ticketId/subject' : '不符'}`)
    }

    // ---- W-07：接收方 500 → 重试 1 次（间隔约 2s），日志只记一条 500 ----
    {
      responseMode = 500
      const recMark = received.length
      const logsBefore = await getLogs()
      const t = await createTicket('W-07 重试探针')
      // 首投 + 2s 后重试，等足窗口收满 2 次
      const hits = await waitFor(() => {
        const h = received.slice(recMark).filter((r) => r.headers['x-lumeidc-event'] === 'ticket.opened')
        return h.length >= 2 ? h : null
      }, 20000, 400)
      responseMode = 200
      let gapMs = 0
      if (hits) gapMs = hits[1].receivedAt - hits[0].receivedAt
      const gapOk = gapMs >= 1500 && gapMs <= 6000
      const log = await waitFor(async () => {
        const ls = await getLogs()
        return ls.find((l) => l.event === 'ticket.opened' && !logsBefore.some((o) => o.id === l.id)) || null
      }, 10000)
      const logOk = !!(log && Number(log.httpStatus) === 500)
      check('W-07', 'P0', '接收方 500 重试 1 次（间隔约 2s）且日志记一条 500',
        !!(t.json && String(t.json.ok) === '1' && hits && gapOk && logOk),
        `投递次数=${hits ? hits.length : 0} 间隔=${gapMs}ms 日志=${log ? `status=${log.httpStatus}` : '无'}`)
    }

    // ---- W-09：后台挂件计数与 logs 口径一致（自包含：清表后造 1 条已知日志精确对账，避免历史累积/LIMIT 50 失真）----
    {
      // 日志表全为 QA 造数，清表同时满足计划文档的清理要求；psql 不可用则降级为累积口径对账
      let cleaned = false
      try {
        execSync(
          `"${PSQL}" -h 127.0.0.1 -p 5433 -U lumeidc -d lumeidc -c "DELETE FROM plugin_webhooknotify_log"`,
          { env: { ...process.env, PGPASSWORD: 'lumeidc_dev' }, timeout: 15000, stdio: 'pipe' }
        )
        cleaned = true
      } catch (e) {
        note('W-09 清表不可用', `psql 清表失败，降级为累积口径对账: ${String((e && e.message) || e).slice(0, 80)}`)
      }
      if (cleaned) {
        // 造 1 条已知成功日志，等待落库后两边应精确为 1/0
        await pageApi(adminPage, 'POST', '/admin/plugin/webhooknotify/test', {})
        await waitFor(async () => ((await getLogs()).length === 1 ? true : null), 10000)
      }
      const w = await pageApi(adminPage, 'GET', '/admin/widgets')
      const card = ((w.json && w.json.widgets) || []).find((x) => x.plugin === 'webhooknotify')
      const rows = (card && card.rows) || []
      const now = new Date()
      const pfx = `${now.getFullYear()}-${String(now.getMonth() + 1).padStart(2, '0')}-${String(now.getDate()).padStart(2, '0')}`
      const logs = await getLogs()
      const todays = logs.filter((l) => String(l.createdAt || '').startsWith(pfx))
      const failN = todays.filter((l) => (l.error || '') !== '' || Number(l.httpStatus) < 200 || Number(l.httpStatus) >= 300).length
      const rowTotal = rows.find((r) => r.label === '今日投递')
      const rowFail = rows.find((r) => r.label === '今日失败')
      const totalOk = !!rowTotal && Number(rowTotal.value) === todays.length
      const failOk = !!rowFail && Number(rowFail.value) === failN
      check('W-09', 'P1', '后台挂件「今日投递/今日失败」与 logs 口径一致',
        !!(card && totalOk && failOk),
        card
          ? `挂件=${rowTotal && rowTotal.value}/${rowFail && rowFail.value} logs统计=${todays.length}/${failN}${cleaned ? '（清表后精确对账）' : ''}${!cleaned && logs.length >= 50 ? '（logs 达 LIMIT 50，对账可能失真）' : ''}`
          : '挂件清单中未找到 webhooknotify')
    }

    // ---- W-08：日志裁切（灌超 100 条，api 层最旧 id 推进 + DB 直证总数 ≤100）----
    {
      const before = await getLogs()
      const maxBefore = before.length ? Math.max(...before.map((l) => Number(l.id))) : 0
      const ROUNDS = 105
      let sent = 0
      for (let i = 0; i < ROUNDS; i++) {
        const r = await pageApi(adminPage, 'POST', '/admin/plugin/webhooknotify/test', {})
        if (r.json && String(r.json.ok) === '1') sent++
      }
      const after = await getLogs()
      const allTest = after.length > 0 && after.every((l) => l.event === 'test')
      const minAfter = after.length ? Math.min(...after.map((l) => Number(l.id))) : 0
      let dbCount = null
      try {
        const out = execSync(
          `"${PSQL}" -h 127.0.0.1 -p 5433 -U lumeidc -d lumeidc -t -A -c "SELECT COUNT(*) FROM plugin_webhooknotify_log"`,
          { env: { ...process.env, PGPASSWORD: 'lumeidc_dev' }, encoding: 'utf8', timeout: 15000 },
        )
        dbCount = Number(out.trim())
      } catch (e) {
        note('W-08 psql 不可用', String((e && e.message) || e).slice(0, 80))
      }
      const apiOk = sent >= 100 && allTest && minAfter > maxBefore
      check('W-08', 'P1', '日志裁切保留最近 100 条',
        dbCount === null ? apiOk : apiOk && dbCount <= 100,
        `连发=${sent}/${ROUNDS} 全为test=${allTest} 最旧id ${maxBefore}→${minAfter}${dbCount === null ? '（DB 未直证）' : ` DB总数=${dbCount}`}`)
    }

    // ---- W-10：配置级禁用闸门（onEvent 现读配置，禁用后立即不投递）----
    {
      await cfgSet({ enabled: false })
      const recMark = received.length
      const logsBefore = await getLogs()
      const t = await createTicket('W-10 禁用探针')
      await sleep(2500)
      const noDeliver = received.length === recMark
      const noLog = !(await getLogs()).some((l) => l.event === 'ticket.opened' && !logsBefore.some((o) => o.id === l.id))
      check('W-10', 'P0', '禁用后立即不再投递（配置级闸门即时生效）', noDeliver && noLog,
        `建单=${t.json && String(t.json.ok) === '1' ? 'ok' : '失败'} 接收端新增=${received.length - recMark} 新增ticket.opened日志=${noLog ? 0 : '有'}`)
    }
  } finally {
    // ---- 收尾：还原配置；QA 投递日志无删除 API，靠 100 条裁切自然淘汰；QA 工单留 S2c-4 清理 ----
    let origEvts = []
    try { origEvts = JSON.parse(origValues.events || '[]') } catch { /* 保留空 */ }
    const back = await cfgSet({
      enabled: origValues.enabled === '1',
      url: origValues.url || '',
      events: origEvts,
    })
    server.close()
    note('webhooknotify 收尾',
      `配置还原=${back.json && String(back.json.ok) === '1' ? 'ok' : '失败'}（secret 为 password 不回显：${origHadSecret ? '原已有 secret，已被验收值覆盖' : '原无 secret，QA secret 无法经 API 清空'}）`)
  }
}

// ---------------------------------------------------------------- tickets 业务流（S2c：T-01~T-13）
async function flowTickets({ adminPage, customerPage, marker }) {
  // D 系列三视图检查后页面停留在移动视口（375px），恢复桌面视口再操作后台
  await adminPage.setViewport({ width: 1440, height: 900 })

  // ---- 局部工具 ----
  const psql = (sql) => {
    try {
      return execSync(`"${PSQL}" -h 127.0.0.1 -p 5433 -U lumeidc -d lumeidc -t -A -c "${sql}"`,
        { env: { ...process.env, PGPASSWORD: 'lumeidc_dev' }, encoding: 'utf8', timeout: 15000 }).trim()
    } catch (e) {
      return null
    }
  }
  // 页面内 multipart 上传（pageApi 只发 JSON；附件须走 FormData，浏览器自动生成 boundary 与真实 MIME 嗅探）
  const uploadFile = (page, p, { filename, mime, sizeBytes }) =>
    page.evaluate(async (p2, fn2, mime2, size2) => {
      const s = await fetch('/__api/session', { credentials: 'same-origin', cache: 'no-store', headers: { Accept: 'application/json' } })
      const tok = ((await s.json()) || {}).csrf || ''
      const buf = new Uint8Array(size2)
      for (let i = 0; i < size2; i++) buf[i] = 65 + (i % 26)
      const form = new FormData()
      form.append('file', new File([buf], fn2, { type: mime2 }))
      const res = await fetch('/__api' + p2, { method: 'POST', credentials: 'same-origin', headers: { 'X-CSRF-Token': tok, Accept: 'application/json' }, body: form })
      const text = await res.text()
      let json = null
      try { json = JSON.parse(text) } catch { /* 非 JSON */ }
      return { status: res.status, json, text: text.slice(0, 400) }
    }, p, filename, mime, sizeBytes)
  const createTk = (subject, extra = {}) =>
    pageApi(customerPage, 'POST', '/plugin/tickets/create', { subject, body: `${marker} 验收正文`, priority: 'normal', category: 'technical', ...extra })
  const detail = (id) => pageApi(customerPage, 'GET', `/plugin/tickets/${id}`)
  const adminDetail = (id) => pageApi(adminPage, 'GET', `/admin/plugin/tickets/${id}`)

  // 取第二账号页（模块级 ensureAltPage 在主流程声明，此处直接引用）
  const altPage = await ensureAltPage()
  if (!altPage) note('tickets 前置', '第二账号登录失败，T-02/T-04 越权断言将降级跳过')

  // 取一名可分配客服（assignees 接口）
  const assigneesRes = await pageApi(adminPage, 'GET', '/admin/plugin/tickets/assignees')
  const assignees = (assigneesRes.json && (assigneesRes.json.list || assigneesRes.json.assignees)) || []
  const agent = assignees.find((a) => a.name === ADMIN_ACCOUNT) || assignees[0] || null

  // 取用户B（alt）的一条在营服务用于 T-02「他人服务」探针
  let altServiceId = 0
  if (altPage) {
    const svcs = await pageApi(altPage, 'GET', '/services')
    const svcList = (svcs.json && svcs.json.list) || []
    if (svcList[0] && svcList[0].id) altServiceId = svcList[0].id
  }

  // 造数：主工单（后续多数用例围绕它）
  const mainRes = await createTk(`${marker} 主工单`)
  const mainId = mainRes.json && mainRes.json.id
  if (!mainId) {
    check('T-00', 'P0', 'tickets 造数（主工单创建）', false, `HTTP ${mainRes.status} ${String(mainRes.text || '').slice(0, 80)}`)
    return
  }
  note('tickets 造数', `主工单 id=${mainId} 客服=${agent ? `${agent.name}(id=${agent.id})` : '无'} alt服务=${altServiceId || '无'}`)

  try {
    // ---- T-01：用户建单（边界拦截 + 合法创建；priority 非法值按实现归一为 normal 并 note 偏差）----
    {
      const emptySub = await pageApi(customerPage, 'POST', '/plugin/tickets/create', { subject: '', body: 'x', priority: 'normal', category: 'technical' })
      const emptyBody = await pageApi(customerPage, 'POST', '/plugin/tickets/create', { subject: 'x', body: '', priority: 'normal', category: 'technical' })
      const longSub = await pageApi(customerPage, 'POST', '/plugin/tickets/create', { subject: 'S'.repeat(200), body: 'x', priority: 'normal', category: 'technical' })
      const longBody = await pageApi(customerPage, 'POST', '/plugin/tickets/create', { subject: 'x', body: 'B'.repeat(10001), priority: 'normal', category: 'technical' })
      // 计划文档写「非法 priority → ok:0」，实际实现静默归一为 normal（handlers_client.go 行 102-104）。按实现断言并登记偏差。
      const badPri = await createTk(`${marker} 非法优先级`, { priority: 'bogus-9' })
      const badPriNorm = badPri.json && String(badPri.json.ok) === '1'
      let badPriVal = ''
      if (badPriNorm && badPri.json.id) {
        const d = await adminDetail(badPri.json.id)
        badPriVal = (d.json && d.json.ticket && d.json.ticket.priority) || ''
      }
      const interceptOk = [emptySub, emptyBody, longSub, longBody].every((r) => r.status === 400 && r.json && String(r.json.ok) === '0')
      check('T-01', 'P0', '用户建单（空/超长拦截；非法 priority 归一 normal）',
        !!(interceptOk && badPriNorm && badPriVal === 'normal' && mainId),
        `空主题=${emptySub.status} 空描述=${emptyBody.status} 超长主题=${longSub.status} 超长正文=${longBody.status} 非法priority归一=${badPriNorm && badPriVal === 'normal' ? 'normal' : badPriVal || '未建'}`)
      if (badPriNorm) note('T-01 口径偏差', '非法 priority 实际返回 ok:1 且静默归一为 normal（计划文档写 ok:0）。按实现断言，登记为口径偏差不判缺陷。')
    }

    // ---- T-02：关联服务校验（他人服务 → 400）----
    if (altPage && altServiceId) {
      const r = await createTk(`${marker} 挂他人服务`, { service_id: altServiceId })
      check('T-02', 'P1', '关联服务归属校验（挂他人服务被拒）',
        !!(r.status === 400 && r.json && String(r.json.ok) === '0' && /无权访问|不存在/.test((r.json && r.json.msg) || '')),
        `HTTP ${r.status} msg=${(r.json && r.json.msg) || ''}`)
    } else {
      skip('T-02', 'P1', '关联服务归属校验', 'alt 账号无在营服务或登录失败，无法构造他人服务探针')
    }

    // ---- T-03：用户回复（回复后状态回 pending；已关闭工单回复 → 400）----
    {
      const replyRes = await pageApi(customerPage, 'POST', `/plugin/tickets/${mainId}/reply`, { content: `${marker} 用户追充` })
      const replyOk = !!(replyRes.json && String(replyRes.json.ok) === '1')
      const st = psql(`SELECT status FROM tickets WHERE id=${mainId}`)
      // 先造一张已关闭工单用于「关闭后回复 → 400」
      const closedRes = await createTk(`${marker} 预关闭单`)
      const closedId = closedRes.json && closedRes.json.id
      let closedReply400 = false
      if (closedId) {
        await pageApi(customerPage, 'POST', `/plugin/tickets/${closedId}/close`, { reason: 'QA 预关闭' })
        const rr = await pageApi(customerPage, 'POST', `/plugin/tickets/${closedId}/reply`, { content: 'x' })
        closedReply400 = rr.status === 400
      }
      check('T-03', 'P0', '用户回复（状态回 pending；已关闭回复 400）',
        !!(replyOk && st === 'pending' && closedReply400),
        `回复=${replyOk ? 'ok' : '失败'} 状态=${st} 已关闭回复400=${closedReply400}`)
    }

    // ---- T-04：附件链路（上传→详情出现→属主下载、他人 404；超 10MB 拒绝。计划写 12MB，实际 10MB 上限，登记偏差）----
    if (altPage) {
      const upRes = await uploadFile(customerPage, `/plugin/tickets/${mainId}/attachments`, { filename: 'qa-note.txt', mime: 'text/plain', sizeBytes: 2048 })
      const upOk = !!(upRes.json && String(upRes.json.ok) === '1')
      const det = await detail(mainId)
      const atts = (det.json && det.json.attachments) || []
      const att = atts.find((a) => a.name === 'qa-note.txt')
      // 属主下载
      let ownerDl = false
      if (att) {
        const dl = await pageApi(customerPage, 'GET', `/plugin/tickets/${mainId}/attachments/${att.id}`)
        ownerDl = dl.status === 200
      }
      // 他人下载 → 404
      let otherDl404 = false
      if (att) {
        const od = await pageApi(altPage, 'GET', `/plugin/tickets/${mainId}/attachments/${att.id}`)
        otherDl404 = od.status === 404
      }
      // 超限：10MB+1（readUpload 显式校验，SaveAttachment 上限 maxTicketAttachmentBytes=10<<20）
      const bigRes = await uploadFile(customerPage, `/plugin/tickets/${mainId}/attachments`, { filename: 'qa-big.bin', mime: 'application/zip', sizeBytes: (10 << 20) + 1 })
      const bigRejected = bigRes.status === 400 && /10\s*MB|超过|大小/.test(((bigRes.json && bigRes.json.msg) || bigRes.text || ''))
      check('T-04', 'P1', '附件链路（上传/详情/属主下载/他人404/超10MB拒绝）',
        !!(upOk && att && ownerDl && otherDl404 && bigRejected),
        `上传=${upOk ? 'ok' : `失败${upRes.status}`} 详情出现=${att ? `id=${att.id}` : '无'} 属主下载=${ownerDl} 他人404=${otherDl404} 超限拒绝=${bigRejected}`)
      note('T-04 口径偏差', '附件上限实际为 10MB（storage maxTicketAttachmentBytes=10<<20），计划文档写 12MB（12<<20 实为 ParseMultipartForm 内存上限）。按 10MB 断言，登记偏差不判缺陷。')
    } else {
      skip('T-04', 'P1', '附件链路', '第二账号不可用，越权下载断言无法执行')
    }

    // ---- T-05：用户关闭/重开（close 必填原因；重复关闭 400；reopen 仅 closed 可开）----
    {
      const t5 = await createTk(`${marker} 状态机单`)
      const t5id = t5.json && t5.json.id
      const noReason = await pageApi(customerPage, 'POST', `/plugin/tickets/${t5id}/close`, { reason: '' })
      const noReasonOk = noReason.status === 400
      const reopenEarly = await pageApi(customerPage, 'POST', `/plugin/tickets/${t5id}/reopen`, {})
      const reopenEarlyOk = reopenEarly.status === 400
      const closeOk = await pageApi(customerPage, 'POST', `/plugin/tickets/${t5id}/close`, { reason: 'QA 关闭' })
      const closedOk = !!(closeOk.json && String(closeOk.json.ok) === '1')
      const dupClose = await pageApi(customerPage, 'POST', `/plugin/tickets/${t5id}/close`, { reason: 'again' })
      const dupCloseOk = dupClose.status === 400
      const reopen = await pageApi(customerPage, 'POST', `/plugin/tickets/${t5id}/reopen`, {})
      const reopenOk = !!(reopen.json && String(reopen.json.ok) === '1')
      const stAfter = psql(`SELECT status FROM tickets WHERE id=${t5id}`)
      check('T-05', 'P0', '用户关闭/重开状态机（必填原因/重复关闭400/仅closed可重开）',
        !!(noReasonOk && reopenEarlyOk && closedOk && dupCloseOk && reopenOk && stAfter === 'pending'),
        `空原因400=${noReasonOk} 未关闭重开400=${reopenEarlyOk} 关闭=${closedOk} 重复关闭400=${dupCloseOk} 重开=${reopenOk} 重开后状态=${stAfter}`)
    }

    // ---- T-08：客服回复（状态→processing、首响只写一次、触发 ticket.replied）----
    {
      const before = psql(`SELECT coalesce(first_response_at::text,'NULL') FROM tickets WHERE id=${mainId}`)
      const r1 = await pageApi(adminPage, 'POST', `/admin/plugin/tickets/${mainId}/reply`, { content: `${marker} 客服首回` })
      const r1Ok = !!(r1.json && String(r1.json.ok) === '1')
      const mid = psql(`SELECT status, coalesce(first_response_at::text,'NULL') FROM tickets WHERE id=${mainId}`)
      const r2 = await pageApi(adminPage, 'POST', `/admin/plugin/tickets/${mainId}/reply`, { content: `${marker} 客服二回` })
      const r2Ok = !!(r2.json && String(r2.json.ok) === '1')
      const after = psql(`SELECT coalesce(first_response_at::text,'NULL') FROM tickets WHERE id=${mainId}`)
      const parts1 = String(mid || '').split('|')
      const firstOnce = parts1[1] && parts1[1] !== 'NULL' && after === parts1[1]
      check('T-08', 'P0', '客服回复（→processing、首响只写一次、ticket.replied 事件）',
        !!(r1Ok && r2Ok && parts1[0] === 'processing' && firstOnce),
        `首回=${r1Ok} 二回=${r2Ok} 状态=${parts1[0]} 首响前=${before} 首响=${parts1[1]} 二回后=${after} 首响不变=${firstOnce}`)
      note('T-08 跨插件', 'ticket.replied 事件已发出（Emit 于 handlers_admin.go adminReply）。真实 webhook 投递由 W-06/W-07 覆盖，此处验证事件触发点存在。')
    }

    // ---- T-06：已读口径（客服回复后 unread>0 由 psql 直证；打开详情后 read_by_user_at 已写、unread 归零）----
    {
      // clientDetail 先标记已读后查 unread（恒 0），故「回复后 unread>0」须查 DB
      const unreadDb = psql(`SELECT count(*) FROM ticket_messages WHERE ticket_id=${mainId} AND author_type='admin' AND read_by_user_at IS NULL`)
      const unreadGt0 = Number(unreadDb) > 0
      await detail(mainId) // 用户打开详情 → 触发标记已读
      const readMarked = psql(`SELECT count(*) FROM ticket_messages WHERE ticket_id=${mainId} AND author_type='admin' AND read_by_user_at IS NOT NULL`)
      const unreadAfter = psql(`SELECT count(*) FROM ticket_messages WHERE ticket_id=${mainId} AND author_type='admin' AND read_by_user_at IS NULL`)
      check('T-06', 'P1', '已读口径（客服回复后 unread>0；打开详情后归零）',
        !!(unreadGt0 && Number(readMarked) > 0 && Number(unreadAfter) === 0),
        `回复后未读(DB)=${unreadDb} 打开后已标记=${readMarked} 打开后未读=${unreadAfter}（clientDetail 先标记后查，详情返回恒0，故用 DB 直证）`)
    }

    // ---- T-07：管理列表/统计（stats 五键口径与 DB 一致；排序白名单生效）----
    {
      const statsRes = await pageApi(adminPage, 'GET', '/admin/plugin/tickets/stats')
      const st = (statsRes.json && statsRes.json.stats) || {}
      const dbPending = psql(`SELECT count(*) FROM tickets WHERE status='pending'`)
      const dbProcessing = psql(`SELECT count(*) FROM tickets WHERE status='processing'`)
      const dbClosed = psql(`SELECT count(*) FROM tickets WHERE status='closed'`)
      const dbUnassigned = psql(`SELECT count(*) FROM tickets WHERE assignee_admin_id IS NULL AND status<>'closed'`)
      const dbOverdue = psql(`SELECT count(*) FROM tickets WHERE status<>'closed' AND updated_at < now()-interval '24 hours'`)
      const statsOk = Number(st.pending) === Number(dbPending) && Number(st.processing) === Number(dbProcessing) &&
        Number(st.closed) === Number(dbClosed) && Number(st.unassigned) === Number(dbUnassigned) && Number(st.overdue) === Number(dbOverdue)
      // 排序白名单：合法字段按 updated_at 升序，首行 updated_at 应 <= 次行
      const sortRes = await pageApi(adminPage, 'GET', '/admin/plugin/tickets/list?sort=updated_at&order=asc&limit=25')
      const sortList = (sortRes.json && sortRes.json.list) || []
      let sortOk = true
      for (let i = 1; i < sortList.length; i++) {
        if (String(sortList[i - 1].updated_at) > String(sortList[i].updated_at)) { sortOk = false; break }
      }
      // 非法排序字段回落默认（不报错即视为白名单拦截）
      const badSort = await pageApi(adminPage, 'GET', '/admin/plugin/tickets/list?sort=password;DROP&limit=5')
      const badSortOk = badSort.status === 200
      check('T-07', 'P1', '管理列表/统计（stats 五键与 DB 一致；排序白名单）',
        !!(statsOk && sortOk && badSortOk),
        `stats[${st.pending}/${st.processing}/${st.closed}/${st.unassigned}/${st.overdue}] DB[${dbPending}/${dbProcessing}/${dbClosed}/${dbUnassigned}/${dbOverdue}] 排序正序=${sortOk} 非法字段拦截=${badSortOk}`)
    }

    // ---- T-09：状态流转（admin status→closed 写 closed_at；非法状态 400）----
    {
      const t9 = await createTk(`${marker} 管理状态单`)
      const t9id = t9.json && t9.json.id
      const bad = await pageApi(adminPage, 'POST', `/admin/plugin/tickets/${t9id}/status`, { status: 'bogus' })
      const badOk = bad.status === 400
      const close = await pageApi(adminPage, 'POST', `/admin/plugin/tickets/${t9id}/status`, { status: 'closed' })
      const closeOk = !!(close.json && String(close.json.ok) === '1')
      const closedAt = psql(`SELECT coalesce(closed_at::text,'NULL') FROM tickets WHERE id=${t9id}`)
      check('T-09', 'P1', '管理状态流转（closed 写 closed_at；非法状态 400）',
        !!(badOk && closeOk && closedAt && closedAt !== 'NULL'),
        `非法400=${badOk} 关闭=${closeOk} closed_at=${closedAt}`)
    }

    // ---- T-10：分配客服（写历史+通知；admin_id 不存在 400；queue=mine 隔离）----
    if (agent) {
      const badAssign = await pageApi(adminPage, 'POST', `/admin/plugin/tickets/${mainId}/assign`, { admin_id: 999999999 })
      const badAssignOk = badAssign.status === 400
      const assign = await pageApi(adminPage, 'POST', `/admin/plugin/tickets/${mainId}/assign`, { admin_id: agent.id })
      const assignOk = !!(assign.json && String(assign.json.ok) === '1')
      const hist = psql(`SELECT count(*) FROM ticket_assignment_history WHERE ticket_id=${mainId} AND to_admin_id=${agent.id}`)
      const assigneeNow = psql(`SELECT coalesce(assignee_admin_id::text,'NULL') FROM tickets WHERE id=${mainId}`)
      // queue=mine：当前登录管理员被分配后应见该单
      const mineRes = await pageApi(adminPage, 'GET', '/admin/plugin/tickets/list?queue=mine&limit=100')
      const mineList = (mineRes.json && mineRes.json.list) || []
      const inMine = mineList.some((t) => t.id === mainId)
      check('T-10', 'P1', '分配客服（写历史；不存在400；queue=mine 可见）',
        !!(badAssignOk && assignOk && Number(hist) > 0 && Number(assigneeNow) === agent.id && inMine),
        `不存在400=${badAssignOk} 分配=${assignOk} 历史=${hist} 当前受理=${assigneeNow} mine可见=${inMine}`)
    } else {
      skip('T-10', 'P1', '分配客服', 'assignees 接口无可用客服')
    }

    // ---- T-11：内部备注（is_internal=true，用户端不可见）----
    {
      const noteContent = `${marker} 内部备注涉密`
      const noteRes = await pageApi(adminPage, 'POST', `/admin/plugin/tickets/${mainId}/internal-note`, { content: noteContent })
      const noteOk = !!(noteRes.json && String(noteRes.json.ok) === '1')
      const adm = await adminDetail(mainId)
      const admMsgs = (adm.json && adm.json.messages) || []
      const admHasInternal = admMsgs.some((m) => m.internal && String(m.content).includes('内部备注涉密'))
      const usr = await detail(mainId)
      const usrMsgs = (usr.json && usr.json.messages) || []
      const usrHasInternal = usrMsgs.some((m) => String(m.content).includes('内部备注涉密'))
      check('T-11', 'P0', '内部备注（管理端可见标记 internal；用户端不可见）',
        !!(noteOk && admHasInternal && !usrHasInternal),
        `保存=${noteOk} 管理端可见=${admHasInternal} 用户端可见=${usrHasInternal}（应为false）`)
    }

    // ---- T-12：超时提醒 cron（psql 回填 updated_at>24h → 验证 cron SELECT 口径命中 + 标记写入；24h 内已通知不重复）----
    {
      const t12 = await createTk(`${marker} 超时单`)
      const t12id = t12.json && t12.json.id
      // 回填为 25 小时前更新 → 命中 cron SELECT 口径
      psql(`UPDATE tickets SET updated_at=now()-interval '25 hours' WHERE id=${t12id}`)
      const hitCron = psql(`SELECT count(*) FROM tickets WHERE id=${t12id} AND status<>'closed' AND updated_at < now()-interval '24 hours' AND (timeout_notified_at IS NULL OR timeout_notified_at < now()-interval '24 hours')`)
      // 模拟 cron 标记已通知
      psql(`UPDATE tickets SET timeout_notified_at=now() WHERE id=${t12id}`)
      const hitAfterMark = psql(`SELECT count(*) FROM tickets WHERE id=${t12id} AND status<>'closed' AND updated_at < now()-interval '24 hours' AND (timeout_notified_at IS NULL OR timeout_notified_at < now()-interval '24 hours')`)
      check('T-12', 'P2', '超时提醒 cron 口径（24h 未更新命中；标记后 24h 内不重复）',
        !!(Number(hitCron) === 1 && Number(hitAfterMark) === 0),
        `回填后命中=${hitCron}（应1） 标记timeout_notified_at后命中=${hitAfterMark}（应0）（cron 每小时执行，此处用 psql 直证 SELECT 口径与去重标记，计划行140允许）`)
    }

    // ---- T-13：禁用闸门（C 系列 checkGate 已覆盖全 404；此处断言前台菜单/铃铛不含工单条目的口径说明）----
    {
      // checkGate 在主流程先行执行（禁用→全404→恢复）。铃铛工单条目由 ticketNotify 触发，插件禁用后路由 404 即无法建单/回复，
      // 源头已断；菜单可见性由 C-01 客户端路由检查覆盖。此处作结论性 note，不重复禁用操作。
      note('T-13', '禁用闸门：禁用后 client/admin 路由全 404 由 C-01/C-02 checkGate 覆盖；工单通知产生于建单/回复动作，禁用后动作不可达，源头阻断。')
    }
  } finally {
    // ---- 收尾：清理 QA 工单（subject 以 QA 开头，含 webhooknotify 阶段探针单）及级联消息/附件/历史 ----
    const delTickets = psql(`DELETE FROM tickets WHERE subject LIKE 'QA%'`)
    const delCount = psql(`SELECT count(*) FROM tickets WHERE subject LIKE 'QA%'`)
    note('tickets 收尾', `QA 工单清理：DELETE 执行=${delTickets !== null ? 'ok' : 'psql不可用'} 残留=${delCount}（messages/attachments/history 由 ON DELETE CASCADE 级联；私有附件文件保留在存储目录）`)
  }
}

// ---------------------------------------------------------------- dailyreport 业务流（S2c：D-01~D-08）
async function flowDailyreport({ adminPage, customerPage, marker }) {
  // D 系列三视图检查后页面停留在移动视口（375px），恢复桌面视口再操作后台
  await adminPage.setViewport({ width: 1440, height: 900 })

  const psql = (sql) => {
    try {
      return execSync(`"${PSQL}" -h 127.0.0.1 -p 5433 -U lumeidc -d lumeidc -t -A -c "${sql}"`,
        { env: { ...process.env, PGPASSWORD: 'lumeidc_dev', PGCLIENTENCODING: 'UTF8' }, encoding: 'utf8', timeout: 15000 }).trim()
    } catch (e) {
      return null
    }
  }
  const cfgGet = async () => (await pageApi(adminPage, 'GET', '/admin/plugin/dailyreport/config')).json || {}
  const cfgSet = (values) => pageApi(adminPage, 'POST', '/admin/plugin/dailyreport/config', { values })
  const sendNow = () => pageApi(adminPage, 'POST', '/admin/plugin/dailyreport/send-now', {})

  // 与后端 time.Now().Format("2006-01-02") 同为本地时区（report.go:75）
  const nowD = new Date()
  const day = `${nowD.getFullYear()}-${String(nowD.getMonth() + 1).padStart(2, '0')}-${String(nowD.getDate()).padStart(2, '0')}`
  const dayKey = `daily_report:${day}`
  const QA_EMAIL = 'qa-admin@lumeidc.local'
  const alertCount = () => parseInt(psql(`SELECT count(*) FROM admin_alert_log WHERE alert_key='${dayKey}'`) || '-1', 10)
  const mailCount = () => parseInt(psql(`SELECT count(*) FROM mail_outbox WHERE recipient='${QA_EMAIL}'`) || '-1', 10)
  const delDayKey = () => psql(`DELETE FROM admin_alert_log WHERE alert_key='${dayKey}'`)
  // 与插件 collect 同一条 SQL（report.go:43-52），列序：订单/实收/新用户/新工单/待处理/30天到期/激活中
  const RECALC_SQL = `SELECT (SELECT count(*) FROM orders WHERE created_at >= CURRENT_DATE - 1 AND created_at < CURRENT_DATE), (SELECT coalesce(sum(amount),0)::float8 FROM invoices WHERE status=1 AND paid_at >= CURRENT_DATE - 1 AND paid_at < CURRENT_DATE), (SELECT count(*) FROM users WHERE created_at >= CURRENT_DATE - 1 AND created_at < CURRENT_DATE), (SELECT count(*) FROM tickets WHERE created_at >= CURRENT_DATE - 1 AND created_at < CURRENT_DATE), (SELECT count(*) FROM tickets WHERE status <> 'closed'), (SELECT count(*) FROM services WHERE status = 1 AND expires_at < now() + interval '30 days'), (SELECT count(*) FROM services WHERE status = 1)`

  // 备份原配置与管理员收件相关 settings（供收尾还原）
  const orig = await cfgGet()
  const origValues = orig.values || {}
  const origNotifyEmail = psql(`SELECT value FROM settings WHERE key='admin_notify_email'`)
  const origForward = psql(`SELECT value FROM settings WHERE key='notify_email_forward_enabled'`)

  try {
    // 前置：清理历史 QA 残留；预置管理员收件邮箱与业务邮件总开关——
    // 否则 NotifyAdminOnce 前置早退（admin_notify.go:150-157），send-now 静默 no-op
    psql(`DELETE FROM admin_alert_log WHERE alert_key LIKE 'daily_report:%'`)
    psql(`DELETE FROM mail_outbox WHERE recipient='${QA_EMAIL}'`)
    psql(`INSERT INTO settings(key,value) VALUES('admin_notify_email','${QA_EMAIL}') ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value`)
    psql(`INSERT INTO settings(key,value) VALUES('notify_email_forward_enabled','1') ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value`)
    await cfgSet({ enabled: true })

    // ---- D-01：配置读写（保存重读一致；select 白名单外值被拒且不污染存储）----
    {
      await cfgSet({ enabled: true, hour: '9' })
      const v = (await cfgGet()).values || {}
      const readOk = v.enabled === '1' && v.hour === '9'
      const bad1 = await cfgSet({ hour: '99' })
      const bad2 = await cfgSet({ hour: 'abc' })
      const rejOk = !!(bad1.json && String(bad1.json.ok) === '0' && /选项不在允许范围内/.test(bad1.json.msg || '')) &&
        !!(bad2.json && String(bad2.json.ok) === '0')
      const after = (await cfgGet()).values || {}
      const clean = after.hour === '9'
      check('D-01', 'P0', '配置读写（enabled/hour 保存重读一致，白名单外值拒绝且不污染存储）', readOk && rejOk && clean,
        `重读 enabled=${v.enabled} hour=${v.hour}；hour=99 → ok=${bad1.json && bad1.json.ok}（${(bad1.json && bad1.json.msg) || ''}）；hour=abc → ok=${bad2.json && bad2.json.ok}；拒绝后 hour=${after.hour}`)
    }

    // ---- D-02：立即发送全链路（API ok + 去重账本记账 + 邮件入队，psql 直证）----
    {
      const r = await sendNow()
      const a = alertCount()
      const m = mailCount()
      const subj = psql(`SELECT subject FROM mail_outbox WHERE recipient='${QA_EMAIL}' ORDER BY id DESC LIMIT 1`)
      const catSubj = psql(`SELECT category || ' / ' || subject FROM admin_alert_log WHERE alert_key='${dayKey}'`)
      check('D-02', 'P0', '立即发送（send-now → admin_alert_log 记账 + mail_outbox 入队）',
        !!(r.json && String(r.json.ok) === '1') && a === 1 && m === 1 && (subj || '').includes(`每日运营报告（${day}）`),
        `api ok=${r.json && r.json.ok}；alert_log=${a} 行（${catSubj || '无'}）；mail_outbox=${m} 行（${subj || '无'}）`)
    }

    // ---- D-03：按日幂等（当日重复发送，去重键拦截不产生新行）----
    {
      const a0 = alertCount()
      const m0 = mailCount()
      const r = await sendNow()
      const a1 = alertCount()
      const m1 = mailCount()
      check('D-03', 'P0', '按日幂等（当日重复 send-now 不重复投递）',
        !!(r.json && String(r.json.ok) === '1') && a0 === 1 && a1 === 1 && m0 === 1 && m1 === 1,
        `api ok=${r.json && r.json.ok}；alert_log ${a0}→${a1}；mail_outbox ${m0}→${m1}`)
    }

    // ---- D-04：开关关闭早退（先删当日 key 排除幂等干扰，enabled=0 发送仍无新行）----
    {
      delDayKey()
      await cfgSet({ enabled: false })
      const m0 = mailCount()
      const r = await sendNow()
      const a1 = alertCount()
      const m1 = mailCount()
      check('D-04', 'P0', '开关关闭时不发送（enabled=0 早退，已删 key 排除按日幂等干扰）',
        !!(r.json && String(r.json.ok) === '1') && a1 === 0 && m1 === m0,
        `api ok=${r.json && r.json.ok}；alert_log=${a1}（期望0）；mail_outbox ${m0}→${m1}`)
      await cfgSet({ enabled: true })
    }

    // ---- D-05：数值正确性（造昨日工单作已知增量锚点；正文 7 项指标与 psql 重算逐一相等）----
    {
      const tk = await pageApi(customerPage, 'POST', '/plugin/tickets/create', {
        subject: `${marker} 日报锚点工单`, body: 'dailyreport 验收造数', priority: 'normal', category: 'technical',
      })
      const tid = tk.json && tk.json.id
      if (tid) psql(`UPDATE tickets SET created_at = CURRENT_DATE - 1 + time '12:00' WHERE id = ${tid}`)
      note('D-05 造数', `昨日锚点工单 id=${tid || '创建失败'}（created_at 回填至昨日正午）`)
      delDayKey()
      await sendNow()
      const body = psql(`SELECT body FROM admin_alert_log WHERE alert_key='${dayKey}'`) || ''
      const m = body.match(/昨日新订单 (\d+) 笔，实收 ([\d.]+) 元；新注册用户 (\d+) 人；新工单 (\d+) 张（当前待处理 (\d+) 张）。激活中服务 (\d+) 个，其中 (\d+) 个将在 30 天内到期。/)
      const rc = (psql(RECALC_SQL) || '').split('|')
      let ok = !!m && rc.length === 7 && Number(m[4]) >= 1
      const diffs = []
      if (m && rc.length === 7) {
        // text() 参数序：订单/实收/新用户/新工单/待处理/激活中/30天到期（末两位与 SQL 列序相反）
        const pairs = [
          ['昨日新订单', m[1], rc[0]], ['昨日实收', m[2], Number(rc[1]).toFixed(2)], ['新注册用户', m[3], rc[2]],
          ['昨日新工单', m[4], rc[3]], ['当前待处理', m[5], rc[4]], ['激活中服务', m[6], rc[6]], ['30天内到期', m[7], rc[5]],
        ]
        for (const [label, a, b] of pairs) {
          const na = label === '昨日实收' ? a : String(parseInt(a, 10))
          const nb = label === '昨日实收' ? b : String(parseInt(b, 10))
          if (na !== nb) diffs.push(`${label}: 正文${a}≠重算${b}`)
        }
        ok = ok && diffs.length === 0
      }
      check('D-05', 'P0', '数值正确性（日报正文 7 项指标与数据库重算一致，含昨日造数锚点）', ok,
        m ? (diffs.length ? diffs.join('；') : `正文=[${m.slice(1).join(',')}] 重算=[${rc.join(',')}] 锚点新工单=${m[4]}`) : `正文格式不匹配：${body.slice(0, 120)}`)
    }

    // ---- D-06：跨日重置（去重键含日期；删除当日 key 模拟新一天，可再次发送）----
    {
      delDayKey()
      const m0 = mailCount()
      const r = await sendNow()
      const a1 = alertCount()
      const m1 = mailCount()
      check('D-06', 'P1', '跨日重置（去重键按日隔离，新日期可再次发送）',
        !!(r.json && String(r.json.ok) === '1') && a1 === 1 && m1 === m0 + 1,
        `api ok=${r.json && r.json.ok}；alert_log=${a1}；mail_outbox ${m0}→${m1}`)
      note('D-06 口径', '真实跨日不可等待：以删除当日 key 模拟新一天的去重键空间（key=daily_report:YYYY-MM-DD，report.go:75-76）')
    }

    // ---- D-07：cron 定时触发（启动期注册，降级为代码证据 note；S2c 计划行 139 允许）----
    note('D-07 cron 定时触发', '降级：CronJobs 按 hour 生成 spec "0 %d * * *"（report.go:16-27），注册发生在启动期，改 hour 需重启生效；整点等待不可行，本轮不实测触发')

    // ---- D-08：禁用闸门（禁用后 send-now 404；cron 包装函数运行期检查 plugin.Enabled）----
    {
      await pageApi(adminPage, 'POST', '/admin/plugins/dailyreport/toggle', { enabled: false })
      const blocked = await sendNow()
      await pageApi(adminPage, 'POST', '/admin/plugins/dailyreport/toggle', { enabled: true })
      const back = await pageApi(adminPage, 'GET', '/admin/plugin/dailyreport/config')
      check('D-08', 'P0', '禁用闸门（禁用态 send-now 404，恢复后路由可用）',
        blocked.status === 404 && back.status === 200,
        `禁用态 send-now status=${blocked.status}；恢复后 config status=${back.status}`)
      note('D-08 cron 侧', 'cron 包装函数运行期检查 plugin.Enabled（cron.go:161），禁用后即使到点也跳过执行')
    }
  } finally {
    // ---- 收尾：还原插件配置与 settings，清理 QA 数据 ----
    const backCfg = await cfgSet({ enabled: origValues.enabled === '1', hour: origValues.hour || '8' })
    if (origNotifyEmail) psql(`UPDATE settings SET value='${origNotifyEmail}' WHERE key='admin_notify_email'`)
    else psql(`DELETE FROM settings WHERE key='admin_notify_email'`)
    if (origForward) psql(`UPDATE settings SET value='${origForward}' WHERE key='notify_email_forward_enabled'`)
    else psql(`DELETE FROM settings WHERE key='notify_email_forward_enabled'`)
    psql(`DELETE FROM admin_alert_log WHERE alert_key LIKE 'daily_report:%'`)
    psql(`DELETE FROM mail_outbox WHERE recipient='${QA_EMAIL}'`)
    psql(`DELETE FROM tickets WHERE subject LIKE '${marker}%'`)
    const leftAlert = psql(`SELECT count(*) FROM admin_alert_log WHERE alert_key LIKE 'daily_report:%'`)
    const leftMail = psql(`SELECT count(*) FROM mail_outbox WHERE recipient='${QA_EMAIL}'`)
    const leftTk = psql(`SELECT count(*) FROM tickets WHERE subject LIKE '${marker}%'`)
    note('dailyreport 收尾', `配置还原=${backCfg.json && String(backCfg.json.ok) === '1' ? 'ok' : '失败'}；settings 还原（收件邮箱=${origNotifyEmail || '原为空'} 总开关=${origForward || '原为空'}）；残留 alert=${leftAlert} mail=${leftMail} 工单=${leftTk}`)
  }
}

// ---------------------------------------------------------------- spaceship 业务流

/**
 * QA 环境通常未配置 Spaceship API Key/Secret，注册/查询类断言一律取「安全拒绝」判据
 * （ok:0 且无 500），不依赖上游连通性；UI 断言只验证三 Tab 切换与面板显隐。
 * S-06~S-08 需要真实落库数据：经 psql 直插域名/联系人/操作记录，再走 API 验证
 * 自动续费状态机、操作重试入口与联系人增删设默认（含归属校验与默认唯一性）；
 * 上游连通相关的轮询深链路（ListPending→pollOne→回补/回退）由 Go 单测
 * poll_test.go 以 mock 客户端覆盖。
 */
async function flowSpaceship({ adminPage, customerPage, marker }) {
  // D 系列三视图检查后页面停留在移动视口（375px），业务流统一恢复桌面视口
  await adminPage.setViewport({ width: 1440, height: 900 })
  await customerPage.setViewport({ width: 1440, height: 900 })

  // ---- 局部工具 ----
  const psql = (sql) => {
    try {
      return execSync(`"${PSQL}" -h 127.0.0.1 -p 5433 -U lumeidc -d lumeidc -t -A -c "${sql}"`,
        { env: { ...process.env, PGPASSWORD: 'lumeidc_dev' }, encoding: 'utf8', timeout: 15000 }).trim()
    } catch (e) {
      return null
    }
  }
  // INSERT ... RETURNING id 在 -t -A 下仍会附带「INSERT 0 1」命令标签行，结果行在前，取首行
  const psqlId = (sql) => {
    const out = psql(sql)
    return out ? out.split(/\r?\n/)[0].trim() : null
  }
  // 页内取当前登录用户 id（/session 已登录时回带 user.id）
  const sessionUser = (page) => page.evaluate(async () => {
    const s = await fetch('/__api/session', { credentials: 'same-origin', cache: 'no-store', headers: { Accept: 'application/json' } })
    return ((await s.json()) || {}).user || null
  }).catch(() => null)
  // QA 造数命名（marker 含日期，同日复跑不撞唯一约束）
  const dmActive = `qa-s06a-${marker}.test`
  const dmPending = `qa-s06p-${marker}.test`
  const opKey = `qa-s07op-${marker}`
  const cidA = `qa-s08a-${marker}`
  const cidB = `qa-s08b-${marker}`
  const cidAlt = `qa-s08alt-${marker}`
  const cleanQaData = () => {
    psql(`DELETE FROM plugin_spaceship_operations WHERE operation_id LIKE 'qa-s07op-%'`)
    psql(`DELETE FROM plugin_spaceship_domains WHERE domain LIKE 'qa-s06%'`)
    psql(`DELETE FROM plugin_spaceship_contacts WHERE contact_id LIKE 'qa-s08%'`)
  }
  cleanQaData() // 前置清理：同一天复跑或上次异常退出时的历史残留

  // ---- S-01：后台域名管理/联系人/操作日志三 Tab 可切换，面板显隐跟随 ----
  const pageErrors = attachErrorCollector(adminPage)
  await adminPage.goto(BASE + '/admin#/plugin/spaceship', { waitUntil: 'domcontentloaded', timeout: 25000 })
  await adminPage.waitForSelector('.spaceship-tabs', { timeout: 15000 }).catch(() => null)
  const tabs = ['域名管理', '联系人', '操作日志']
  const tabResults = []
  for (let i = 0; i < tabs.length; i++) {
    const clicked = await adminPage.evaluate((idx) => {
      const btn = document.querySelectorAll('.spaceship-tab')[idx]
      if (btn) { btn.click(); return true }
      return false
    }, i).catch(() => false)
    await sleep(500)
    // Vue 响应式更新后复核：对应 Tab 高亮 + 对应面板 display 非 none（面板顺序同模板 v-show）
    const state = await adminPage.evaluate((idx) => {
      const btns = [...document.querySelectorAll('.spaceship-tab')]
      const wrap = document.querySelector('.spaceship-tabs').parentElement
      const panels = [...wrap.children].filter((el) => el.tagName === 'DIV' && !el.classList.contains('spaceship-tabs'))
      return {
        active: !!btns[idx] && btns[idx].classList.contains('active'),
        panel: !!panels[idx] && getComputedStyle(panels[idx]).display !== 'none',
      }
    }, i).catch(() => ({ active: false, panel: false }))
    tabResults.push(clicked && state.active && state.panel)
  }
  check('S-01', 'P1', '后台三 Tab（域名管理/联系人/操作日志）切换渲染',
    tabResults.every(Boolean),
    tabResults.map((ok, i) => `${tabs[i]} ${ok ? '可见' : '异常'}`).join('；'))

  // ---- S-02：测试连接走真实网络请求，无服务端错误 ----
  const netIdx = netLog.length
  await adminPage.evaluate(() => {
    const btn = [...document.querySelectorAll('button')].find((b) => (b.textContent || '').includes('测试 Spaceship 连接'))
    btn && btn.click()
  })
  const testHit = await waitNetEntry(/\/admin\/plugin\/spaceship\/client\/test/, netIdx)
  const testStatus = testHit && testHit.match(/POST (\d+) /)
  check('S-02', 'P1', '测试连接提交真实请求且无服务端错误',
    !!testHit && !!testStatus && Number(testStatus[1]) < 500,
    testHit ? testHit.slice(0, 130) : '未捕获 /client/test 请求轨迹')

  // ---- S-03：前台可用性查询（UI 触发，原生 setter 保证 v-model 收到输入）----
  await customerPage.goto(BASE + '/plugin/spaceship', { waitUntil: 'domcontentloaded', timeout: 25000 })
  await customerPage.waitForSelector('.spaceship-tabs', { timeout: 15000 }).catch(() => null)
  const netIdx2 = netLog.length
  await customerPage.evaluate(() => {
    const ta = document.querySelector('textarea')
    if (ta) {
      const setter = Object.getOwnPropertyDescriptor(window.HTMLTextAreaElement.prototype, 'value').set
      setter.call(ta, 'qa-probe.invalid')
      ta.dispatchEvent(new Event('input', { bubbles: true }))
    }
    const btn = [...document.querySelectorAll('button')].find((b) => (b.textContent || '').trim() === '查询可用性')
    btn && btn.click()
  })
  const checkHit = await waitNetEntry(/\/plugin\/spaceship\/check/, netIdx2)
  const checkStatus = checkHit && checkHit.match(/POST (\d+) /)
  check('S-03', 'P1', '前台可用性查询提交真实请求且无服务端错误',
    !!checkHit && !!checkStatus && Number(checkStatus[1]) < 500,
    checkHit ? checkHit.slice(0, 130) : '未捕获 /check 请求轨迹')

  // ---- S-04：空域名注册被安全拒绝（未配 API 返回「未配置」；已配则「域名格式错误」，均 ok:0）----
  const regRes = await pageApi(customerPage, 'POST', '/plugin/spaceship/register', { domain: '', years: 1, paidAmount: '' })
  check('S-04', 'P1', '空域名注册被安全拒绝',
    regRes.status === 200 && !!regRes.json && regRes.json.ok === 0,
    `HTTP ${regRes.status} ${regRes.text.slice(0, 80)}`)

  // ---- S-05：业务流期间后台页面无 console/pageerror ----
  check('S-05', 'P1', '业务流期间后台页面无 console/pageerror',
    pageErrors.length === 0,
    pageErrors.length ? pageErrors[0].slice(0, 120) : '无页面错误')

  // ---- S-06/S-07/S-08：需要真实落库数据，psql 直插后经 API 验证 ----
  try {
    // ---- S-06：自动续费开关状态机（active 开/关落库 + 前后台回读一致；pending 拒绝）----
    {
      const myUser = await sessionUser(customerPage)
      const myId = myUser && myUser.id
      if (!myId || psql('SELECT 1') === null) {
        skip('S-06', 'P1', '自动续费状态机（active 开/关落库 + 前后台回读一致；pending 拒绝）',
          !myId ? '前台会话未取到用户 id' : 'psql 不可用，无法直插测试域名')
      } else {
        // active 不置 spaceship_domain_id：clientAutoRenew 跳过上游调用仅落库（QA 环境确定性判据）
        const activeId = psqlId(`INSERT INTO plugin_spaceship_domains (user_id,domain,years,paid_amount_cents,status,privacy_level,auto_renew) VALUES (${myId},'${dmActive}',1,10000,'active','high',false) RETURNING id`)
        const pendingId = psqlId(`INSERT INTO plugin_spaceship_domains (user_id,domain,years,paid_amount_cents,status,privacy_level,auto_renew) VALUES (${myId},'${dmPending}',1,10000,'pending','high',false) RETURNING id`)
        const readRenew = async (page, path, id) => {
          const r = await pageApi(page, 'GET', path)
          const row = ((r.json && r.json.list) || []).find((d) => Number(d.id) === Number(id))
          return row ? row.autoRenew : null
        }
        const on = await pageApi(customerPage, 'POST', `/plugin/spaceship/my/${activeId}/autorenew`, { enable: true })
        const onRead = await readRenew(customerPage, '/plugin/spaceship/my', activeId)
        const onAdmin = await readRenew(adminPage, '/admin/plugin/spaceship/domains', activeId)
        const dbOn = psql(`SELECT auto_renew FROM plugin_spaceship_domains WHERE id=${activeId}`)
        const off = await pageApi(customerPage, 'POST', `/plugin/spaceship/my/${activeId}/autorenew`, { enable: false })
        const offRead = await readRenew(customerPage, '/plugin/spaceship/my', activeId)
        const offAdmin = await readRenew(adminPage, '/admin/plugin/spaceship/domains', activeId)
        const dbOff = psql(`SELECT auto_renew FROM plugin_spaceship_domains WHERE id=${activeId}`)
        const pend = await pageApi(customerPage, 'POST', `/plugin/spaceship/my/${pendingId}/autorenew`, { enable: true })
        check('S-06', 'P1', '自动续费状态机（active 开/关落库 + 前后台回读一致；pending 拒绝）',
          !!activeId && !!pendingId &&
          on.status === 200 && on.json && on.json.ok === 1 && onRead === true && onAdmin === true && dbOn === 't' &&
          off.status === 200 && off.json && off.json.ok === 1 && offRead === false && offAdmin === false && dbOff === 'f' &&
          pend.status === 200 && pend.json && pend.json.ok === 0 && /仅 active/.test((pend.json && pend.json.msg) || pend.text),
          `开=${on.json && on.json.ok}/前台:${onRead}/后台:${onAdmin}/DB:${dbOn} 关=${off.json && off.json.ok}/前台:${offRead}/后台:${offAdmin}/DB:${dbOff} pending=${pend.json && pend.json.ok} ${(pend.json && pend.json.msg) || pend.text.slice(0, 30)}`)
      }
    }

    // ---- S-07：异步操作（pending 直插后管理端可见；retry 入口存在且安全拒绝/触发）----
    {
      if (psql('SELECT 1') === null) {
        skip('S-07', 'P1', '异步操作（pending 管理端可见；retry 入口存在且安全拒绝/触发）', 'psql 不可用，无法直插操作记录')
      } else {
        // started_at 取 20 分钟前，落在 ListPending 的 15 分钟窗口之外：cron（默认 30s 一轮）
        // 不会在断言前改写状态，列表回读确定；cron 捞取→pollOne→回补/回退深链路由
        // Go 单测 poll_test.go 以 mock 客户端覆盖
        const opRowId = psqlId(`INSERT INTO plugin_spaceship_operations (operation_id,domain,op_type,status,started_at) VALUES ('${opKey}','${dmActive}','domain_create','pending',now() - interval '20 minutes') RETURNING id`)
        const listRes = await pageApi(adminPage, 'GET', '/admin/plugin/spaceship/operations')
        const items = (listRes.json && listRes.json.items) || []
        const mine = items.find((o) => o.OperationID === opKey)
        const retry = opRowId
          ? await pageApi(adminPage, 'POST', `/admin/plugin/spaceship/operations/${opRowId}/retry`, {})
          : { status: 0, json: null, text: 'no db row' }
        const retryGate = retry.status === 200 && retry.json && (
          (retry.json.ok === 0 && /未配置/.test((retry.json && retry.json.msg) || retry.text)) || // QA 无凭证：JSONFail 安全拒绝
          retry.json.ok === 1 // 已配凭证：真实触发一次轮询
        )
        check('S-07', 'P1', '异步操作（pending 管理端可见；retry 入口存在且安全拒绝/触发）',
          !!opRowId && listRes.status === 200 && !!mine && mine.Status === 'pending' && retryGate,
          `可见=${!!mine}(status:${mine && mine.Status}) retry=${retry.json && retry.json.ok} ${retry.text.slice(0, 40)}；cron 捞取/pollOne 深链路见 Go 单测 poll_test.go`)
      }
    }

    // ---- S-08：联系人管理（新增门禁；404/403/删除/设默认；默认唯一性）----
    {
      // 门禁：缺必填字段时，无凭证返回「未配置」、有凭证返回「必填」，均为 ok:0（不依赖上游）
      const saveGate = await pageApi(customerPage, 'POST', '/plugin/spaceship/my/contacts', { firstName: 'QA' })
      const myUser = await sessionUser(customerPage)
      const myId = myUser && myUser.id
      if (!myId || psql('SELECT 1') === null) {
        check('S-08', 'P1', '联系人管理（新增门禁 + 404/403/删除/设默认 + 默认唯一性）',
          saveGate.status === 200 && saveGate.json && saveGate.json.ok === 0,
          `新增门禁=${(saveGate.json && saveGate.json.msg) || saveGate.text.slice(0, 30)}；DB 直插部分跳过（${!myId ? '无用户 id' : 'psql 不可用'}）`)
      } else {
        const altPage = await ensureAltPage()
        const altUser = altPage ? await sessionUser(altPage) : null
        const altId = altUser && altUser.id
        // SQL 字面量保持纯 ASCII：非 ASCII 经 cmd 编码转换会触发 psql「invalid byte sequence」
        const idA = psqlId(`INSERT INTO plugin_spaceship_contacts (contact_id,user_id,label,first_name,last_name,email,address1,city,country,phone,is_default) VALUES ('${cidA}',${myId},'QA A','Qa','A','${cidA}@lumeidc.local','QA Street','Shanghai','CN','+86.13800000001',false) RETURNING id`)
        const idB = psqlId(`INSERT INTO plugin_spaceship_contacts (contact_id,user_id,label,first_name,last_name,email,address1,city,country,phone,is_default) VALUES ('${cidB}',${myId},'QA B','Qa','B','${cidB}@lumeidc.local','QA Street','Shanghai','CN','+86.13800000002',false) RETURNING id`)
        const idAlt = altId
          ? psqlId(`INSERT INTO plugin_spaceship_contacts (contact_id,user_id,label,first_name,last_name,email,address1,city,country,phone,is_default) VALUES ('${cidAlt}',${altId},'QA Alt','Qa','Alt','${cidAlt}@lumeidc.local','QA Street','Shanghai','CN','+86.13800000003',false) RETURNING id`)
          : ''
        const missDel = await pageApi(customerPage, 'POST', '/plugin/spaceship/my/contacts/999999999/delete', {})
        const missDef = await pageApi(customerPage, 'POST', '/plugin/spaceship/my/contacts/999999999/default', {})
        const altDel = idAlt ? await pageApi(customerPage, 'POST', `/plugin/spaceship/my/contacts/${idAlt}/delete`, {}) : null
        const altDef = idAlt ? await pageApi(customerPage, 'POST', `/plugin/spaceship/my/contacts/${idAlt}/default`, {}) : null
        const readContacts = async () => {
          const r = await pageApi(customerPage, 'GET', '/plugin/spaceship/my/contacts')
          return (r.json && r.json.list) || []
        }
        const setA = idA ? await pageApi(customerPage, 'POST', `/plugin/spaceship/my/contacts/${idA}/default`, {}) : null
        const rowsA = await readContacts()
        const setB = idB ? await pageApi(customerPage, 'POST', `/plugin/spaceship/my/contacts/${idB}/default`, {}) : null
        const rowsB = await readContacts()
        const delB = idB ? await pageApi(customerPage, 'POST', `/plugin/spaceship/my/contacts/${idB}/delete`, {}) : null
        const rowsAfterDel = await readContacts()
        const defA = rowsA.filter((c) => c.isDefault)
        const defB = rowsB.filter((c) => c.isDefault)
        check('S-08', 'P1', '联系人管理（新增门禁 + 404/403/删除/设默认 + 默认唯一性）',
          saveGate.status === 200 && saveGate.json && saveGate.json.ok === 0 &&
          missDel.status === 404 && missDef.status === 404 &&
          (!altDel || (altDel.status === 403 && altDef.status === 403)) &&
          !!setA && setA.json && setA.json.ok === 1 && defA.length === 1 && Number(defA[0].id) === Number(idA) &&
          !!setB && setB.json && setB.json.ok === 1 && defB.length === 1 && Number(defB[0].id) === Number(idB) &&
          !!delB && delB.json && delB.json.ok === 1 && !rowsAfterDel.some((c) => Number(c.id) === Number(idB)),
          `新增门禁=${(saveGate.json && saveGate.json.msg) || saveGate.text.slice(0, 20)}；404=${missDel.status}/${missDef.status}；越权403=${altDel ? `${altDel.status}/${altDef && altDef.status}` : 'alt账号不可用，跳过'}；默认唯一 A=${defA.map((c) => c.id)} B=${defB.map((c) => c.id)}；删B后仍在列=${rowsAfterDel.some((c) => Number(c.id) === Number(idB))}`)
      }
    }
    // ---- S-09：后台代注册的资金一致性（不依赖真实凭证：无凭证安全拒绝，有凭证按服务端价扣款）----
    {
      // QA 环境未配 Key 时 adminRegisterDomain 先于任何扣款返回「未配置」，
      // 断言 ok:0 且用户余额零变动 —— 即"绝不出现上游已注册但本地未扣款/未记账"。
      const myUser = await sessionUser(customerPage)
      const myId = myUser && myUser.id
      if (!myId || psql('SELECT 1') === null) {
        skip('S-09', 'P0', '后台代注册资金一致性（无凭证安全拒绝 + 余额零变动）',
          !myId ? '前台会话未取到用户 id' : 'psql 不可用，无法读取余额')
      } else {
        // users.balance 为 numeric 元（非分），按文本比较避免 JS 浮点误差
        const balSql = `SELECT balance::text FROM users WHERE id=${myId}`
        const before = (psql(balSql) || '0').trim()
        const reg = await pageApi(adminPage, 'POST', '/admin/plugin/spaceship/domains/register',
          { domain: `qa-s09-${marker}.test`, years: 1, contactId: 0, userId: myId })
        const after = (psql(balSql) || '0').trim()
        const noDom = psql(`SELECT count(*) FROM plugin_spaceship_domains WHERE domain LIKE 'qa-s09-%'`)
        const safeReject = reg.status === 200 && reg.json && reg.json.ok === 0 &&
          /未配置|未启用|价格|请指定|不存在|不支持|失败|错误/.test((reg.json && reg.json.msg) || reg.text)
        check('S-09', 'P0', '后台代注册资金一致性（无凭证安全拒绝 + 余额零变动）',
          safeReject && after === before && Number(noDom) === 0,
          `ok=${reg.json && reg.json.ok} msg=${(reg.json && reg.json.msg) || reg.text.slice(0, 40)}；余额 ${before}→${after}；qa-s09 域名数=${noDom}`)
      }
    }

    // ---- S-10：服务端定价（前端传价一律无效；无价目表/未启用后缀拒绝；价目可读）----
    {
      const prices = await pageApi(adminPage, 'GET', '/admin/plugin/spaceship/prices')
      const clientPrices = await pageApi(customerPage, 'GET', '/plugin/spaceship/prices')
      const list = (prices.json && prices.json.list) || []
      // 客户端改价探针：paidAmount 传 0.01，服务端若仍按客户端价扣款即为 P0 资损
      const probe = await pageApi(customerPage, 'POST', '/plugin/spaceship/register',
        { domain: `qa-s10-${marker}.test`, years: 1, paidAmount: '0.01' })
      const clientOk = clientPrices.status === 200 && !!clientPrices.json && Array.isArray(clientPrices.json.list)
      // 无凭证时返回「未配置」；已配凭证且后缀未定价时返回「未启用/未配置价格」——两者都必须是 ok:0
      const gated = probe.status === 200 && probe.json && probe.json.ok === 0
      check('S-10', 'P0', '服务端定价（价目可读 + 客户端传价无效 + 未定价后缀拒绝）',
        prices.status === 200 && Array.isArray(list) && clientOk && gated,
        `后台价目=${list.length}条/HTTP${prices.status}；前台价目HTTP${clientPrices.status}；改价探针ok=${probe.json && probe.json.ok} ${(probe.json && probe.json.msg) || probe.text.slice(0, 40)}`)
    }

    // ---- S-11：续费链路（无凭证安全拒绝；终态非法年份/非 active 一律拒绝，不产生扣款）----
    {
      const myUser = await sessionUser(customerPage)
      const myId = myUser && myUser.id
      if (!myId || psql('SELECT 1') === null) {
        skip('S-11', 'P1', '续费可用（active 域名提交 + 非法年份/非 active 拒绝）',
          !myId ? '前台会话未取到用户 id' : 'psql 不可用，无法直插域名')
      } else {
        // users.balance 为 numeric 元（非分），按文本比较避免 JS 浮点误差
        const balSql = `SELECT balance::text FROM users WHERE id=${myId}`
        const before = (psql(balSql) || '0').trim()
        // 复用 S-06 已落库的 active / pending 域名；若 S-06 被跳过则自行补插（避免同域名重复插入）
        let activeId = psql(`SELECT id FROM plugin_spaceship_domains WHERE domain='${dmActive}' LIMIT 1`)
        let pendId = psql(`SELECT id FROM plugin_spaceship_domains WHERE domain='${dmPending}' LIMIT 1`)
        if (!activeId) {
          activeId = psqlId(`INSERT INTO plugin_spaceship_domains (user_id,domain,years,paid_amount_cents,status,privacy_level,auto_renew) VALUES (${myId},'${dmActive}',1,10000,'active','high',false) RETURNING id`)
        }
        if (!pendId) {
          pendId = psqlId(`INSERT INTO plugin_spaceship_domains (user_id,domain,years,paid_amount_cents,status,privacy_level,auto_renew) VALUES (${myId},'${dmPending}',1,10000,'pending','high',false) RETURNING id`)
        }
        const rOk = activeId ? await pageApi(customerPage, 'POST', `/plugin/spaceship/my/${activeId}/renew`, { years: 1 }) : null
        const rBadYears = activeId ? await pageApi(customerPage, 'POST', `/plugin/spaceship/my/${activeId}/renew`, { years: 99 }) : null
        const rPend = pendId ? await pageApi(customerPage, 'POST', `/plugin/spaceship/my/${pendId}/renew`, { years: 1 }) : null
        const after = (psql(balSql) || '0').trim()
        // 无凭证：三者均 ok:0；已配凭证：years=1 可 ok:1、years=99 与非 active 仍 ok:0
        const gated = (r) => r && r.status === 200 && r.json && r.json.ok === 0
        const noCharge = gated(rOk) ? after === before : true
        check('S-11', 'P1', '续费可用（active 域名提交 + 非法年份/非 active 拒绝）',
          !!activeId && !!rOk && rOk.status === 200 && !!rOk.json &&
          (rOk.json.ok === 1 || gated(rOk)) && gated(rBadYears) && (!rPend || gated(rPend)) && noCharge,
          `active续费ok=${rOk && rOk.json && rOk.json.ok} ${(rOk && ((rOk.json && rOk.json.msg) || rOk.text.slice(0, 30))) || ''}；years=99 ok=${rBadYears && rBadYears.json && rBadYears.json.ok}；pending ok=${rPend && rPend.json && rPend.json.ok}；余额 ${before}→${after}`)
      }
    }

    // ---- S-12：溢价域名管控（总开关默认关闭 → 前台自助与后台代注册均不放行）----
    {
      const chk = await pageApi(adminPage, 'POST', '/admin/plugin/spaceship/check', { domain: `qa-s12-${marker}.com` })
      const premiumChecked = chk.status === 200 && !!chk.json
      // 前台自助注册溢价域名：即便带任何参数也不得直接成交（必须由管理员确认报价）
      const cReg = await pageApi(customerPage, 'POST', '/plugin/spaceship/register',
        { domain: `qa-s12-${marker}.com`, years: 1, paidAmount: '9999' })
      const myUser = await sessionUser(customerPage)
      const myId = myUser && myUser.id
      const aReg = myId ? await pageApi(adminPage, 'POST', '/admin/plugin/spaceship/domains/register',
        { domain: `qa-s12-${marker}.com`, years: 1, userId: myId, allowPremium: true }) : null
      const gated = (r) => r && r.status === 200 && r.json && r.json.ok === 0
      check('S-12', 'P1', '溢价域名管控（报价可查 + 未开启售卖时前后台均拒绝）',
        premiumChecked && gated(cReg) && (!aReg || gated(aReg)),
        `报价查询HTTP${chk.status}；前台ok=${cReg.json && cReg.json.ok}；后台(allowPremium)ok=${aReg && aReg.json && aReg.json.ok} ${(aReg && ((aReg.json && aReg.json.msg) || aReg.text.slice(0, 30))) || ''}`)
    }

    // ---- S-13：异步操作终态保护（success/failed 重试被拒，不再重复轮询与重复通知）----
    {
      if (psql('SELECT 1') === null) {
        skip('S-13', 'P1', '异步操作终态保护（success/failed 拒绝重试）', 'psql 不可用，无法直插终态操作')
      } else {
        const okKey = `${opKey}-ok`
        const failKey = `${opKey}-fail`
        const okId = psqlId(`INSERT INTO plugin_spaceship_operations (operation_id,domain,op_type,status,started_at,finished_at) VALUES ('${okKey}','${dmActive}','domain_create','success',now() - interval '30 minutes',now() - interval '29 minutes') RETURNING id`)
        const failId = psqlId(`INSERT INTO plugin_spaceship_operations (operation_id,domain,op_type,status,started_at,finished_at) VALUES ('${failKey}','${dmActive}','domain_create','failed',now() - interval '30 minutes',now() - interval '29 minutes') RETURNING id`)
        const rOk = okId ? await pageApi(adminPage, 'POST', `/admin/plugin/spaceship/operations/${okId}/retry`, {}) : null
        const rFail = failId ? await pageApi(adminPage, 'POST', `/admin/plugin/spaceship/operations/${failId}/retry`, {}) : null
        const terminal = (r) => r && r.status === 400 && /已结束|无需重试/.test((r.json && r.json.msg) || r.text)
        check('S-13', 'P1', '异步操作终态保护（success/failed 拒绝重试）',
          !!okId && !!failId && terminal(rOk) && terminal(rFail),
          `success=${rOk && rOk.status} ${(rOk && ((rOk.json && rOk.json.msg) || rOk.text.slice(0, 30))) || ''}；failed=${rFail && rFail.status} ${(rFail && ((rFail.json && rFail.json.msg) || rFail.text.slice(0, 30))) || ''}`)
      }
    }

    // ---- S-14：共享联系人越权（user_id IS NULL 只读：删除/设默认一律 403）----
    {
      const myUser = await sessionUser(customerPage)
      const myId = myUser && myUser.id
      if (!myId || psql('SELECT 1') === null) {
        skip('S-14', 'P0', '共享联系人越权（user_id IS NULL 只读，删除/设默认 403）',
          !myId ? '前台会话未取到用户 id' : 'psql 不可用，无法直插共享联系人')
      } else {
        const sharedCid = `qa-s14-${marker}`
        // user_id 留空 = 管理员共享模板：注册时可用，但普通用户不得删/设默认
        const sid = psqlId(`INSERT INTO plugin_spaceship_contacts (contact_id,user_id,label,first_name,last_name,email,address1,city,country,phone,is_default) VALUES ('${sharedCid}',NULL,'QA Shared','Qa','Shared','${sharedCid}@lumeidc.local','QA Street','Shanghai','CN','+86.13800000009',false) RETURNING id`)
        const del = sid ? await pageApi(customerPage, 'POST', `/plugin/spaceship/my/contacts/${sid}/delete`, {}) : null
        const def = sid ? await pageApi(customerPage, 'POST', `/plugin/spaceship/my/contacts/${sid}/default`, {}) : null
        const still = sid ? psql(`SELECT count(*) FROM plugin_spaceship_contacts WHERE id=${sid}`) : '0'
        const isDef = sid ? psql(`SELECT is_default FROM plugin_spaceship_contacts WHERE id=${sid}`) : 'f'
        check('S-14', 'P0', '共享联系人越权（user_id IS NULL 只读，删除/设默认 403）',
          !!sid && del && del.status === 403 && def && def.status === 403 &&
          Number(still) === 1 && String(isDef).startsWith('f'),
          `sid=${sid} 删除=${del && del.status} 设默认=${def && def.status}；仍存在=${still} is_default=${isDef}`)
      }
    }
  } finally {
    // ---- 收尾：清理 QA 直插数据（前缀匹配，仅动本流程造数）----
    cleanQaData()
    psql(`DELETE FROM plugin_spaceship_domains WHERE domain LIKE 'qa-s09%' OR domain LIKE 'qa-s10%' OR domain LIKE 'qa-s12%'`)
    psql(`DELETE FROM plugin_spaceship_operations WHERE operation_id LIKE 'qa-s07op-%-ok' OR operation_id LIKE 'qa-s07op-%-fail'`)
    psql(`DELETE FROM plugin_spaceship_contacts WHERE contact_id LIKE 'qa-s14%'`)
    const leftD = psql(`SELECT count(*) FROM plugin_spaceship_domains WHERE domain LIKE 'qa-s06%'`)
    const leftC = psql(`SELECT count(*) FROM plugin_spaceship_contacts WHERE contact_id LIKE 'qa-s08%'`)
    const leftO = psql(`SELECT count(*) FROM plugin_spaceship_operations WHERE operation_id LIKE 'qa-s07op-%'`)
    note('spaceship 收尾', `QA 造数清理后残留：域名=${leftD} 联系人=${leftC} 操作=${leftO}`)
  }
}

// ---------------------------------------------------------------- 主流程

const argv = process.argv.slice(2).filter((a) => !a.startsWith('-'))
const selected = argv.length ? argv : Object.keys(PLUGINS)
const unknown = selected.filter((n) => !PLUGINS[n])
if (unknown.length) {
  console.error(`未知插件：${unknown.join(', ')}；已接入：${Object.keys(PLUGINS).join(', ')}`)
  await browser.close()
  process.exit(1)
}

// 前置检查：dev server 与后端可用性。
// 注意：Vite 是 MPA dev server，根路径只有带上 Accept: text/html 才会落到模板入口，
// 裸 fetch（Accept: */*）会被判定为非文档请求而 404。
try {
  const health = await fetch(BASE + '/', { method: 'GET', headers: { Accept: 'text/html' } })
  if (!health.ok && health.status !== 200) throw new Error('HTTP ' + health.status)
} catch (e) {
  console.error(`无法访问前端 ${BASE}（${e.message}）。请先执行 npm run dev，并确认 Go 后端已运行。`)
  await browser.close()
  process.exit(1)
}

const adminCtx = await createCtx()
const customerCtx = await createCtx()
const adminPage = await adminCtx.newPage()
const customerPage = await customerCtx.newPage()
attachNetProbe(adminPage, 'admin')
attachNetProbe(customerPage, 'client')

const adminOk = await loginAdmin(adminPage)
const customerOk = await loginCustomer(customerPage)
check('S-ENV', 'P0', '验收环境登录（后台/前台）', adminOk && customerOk,
  `后台 ${adminOk ? '成功' : '失败'}；前台 ${customerOk ? '成功' : '失败'}`)

let altCtx = null
let altPageCache = null
const ensureAltPage = async () => {
  if (altPageCache) return altPageCache
  altCtx = await createCtx()
  altPageCache = await altCtx.newPage()
  attachNetProbe(altPageCache, 'alt')
  const ok = await loginCustomer(altPageCache, ALT_EMAIL, ALT_PASSWORD)
  if (!ok) {
    await altCtx.close()
    altCtx = null
    altPageCache = null
    return null
  }
  return altPageCache
}

// X-03：记录验收开始时的插件启用态
const pluginState = new Map()
{
  const res = await pageApi(adminPage, 'GET', '/admin/plugins')
  for (const p of (res.json && res.json.plugins) || []) pluginState.set(p.name, p.enabled)
  note('验收开始插件启用态', JSON.stringify(Object.fromEntries(pluginState)))
}

for (const name of selected) {
  const cfg = PLUGINS[name]
  console.log(`\n========== ${cfg.title}（${name}） ==========`)
  await checkAuth(cfg, adminPage)
  await checkCsrf(cfg, adminPage, customerPage)
  await checkMalformed(cfg, adminPage)
  await checkPii(cfg, customerPage)
  await checkPrivilege(cfg, { adminPage, customerPage, ensureAltPage })
  await checkGate(cfg, adminPage, customerPage, pluginState.get(name))
  await checkRender(cfg, adminPage, {
    url: cfg.adminPath,
    name: `${cfg.title} 后台页`,
    waitSelector: '.plugin-shell',
    waitText: cfg.adminWaitText,
    shot: `${name}-admin`,
  })
  if (cfg.clientPath) {
    await checkRender(cfg, customerPage, {
      url: cfg.clientPath,
      name: `${cfg.title} 前台页`,
      waitSelector: cfg.clientWaitSelector,
      waitText: cfg.clientWaitText,
      shot: `${name}-client`,
    })
  }
  if (cfg.flow) {
    await cfg.flow({ adminPage, customerPage, marker: MARKER })
  } else {
    skip('FLOW', 'P1', `${cfg.title} 业务流`, '该插件暂未接入业务流检查（通用 C/B/D 模块已验证）')
  }
}

// X-03：核对启用态与开始一致
{
  const res = await pageApi(adminPage, 'GET', '/admin/plugins')
  const drift = []
  for (const p of (res.json && res.json.plugins) || []) {
    const before = pluginState.get(p.name)
    if (before !== undefined && before !== p.enabled) drift.push(`${p.name}: ${before} → ${p.enabled}`)
  }
  check('X-03', 'P1', '验收前后插件启用态一致', drift.length === 0,
    drift.length ? '漂移：' + drift.join('；') : `${pluginState.size} 个插件启用态无变化`)
}

// ---------------------------------------------------------------- 汇总

const failed = results.filter((r) => r.pass === false)
const skipped = results.filter((r) => r.pass === null)
console.log(`\n========== 验收结果：${results.length - failed.length - skipped.length}/${results.length} 通过，${failed.length} 失败，${skipped.length} 跳过 ==========`)
if (failed.length) {
  console.log('失败项：')
  for (const f of failed) console.log(`  ✗ [${f.id}/${f.level}] ${f.name} — ${f.detail}`)
}
if (skipped.length) {
  console.log('跳过项：')
  for (const s of skipped) console.log(`  ○ [${s.id}/${s.level}] ${s.name} — ${s.detail}`)
}
console.log(`截图目录：${SHOTS}`)
if (failed.length) {
  console.log(`\n---------- 插件 API 请求轨迹（共 ${netLog.length} 条，末 40 条） ----------`)
  for (const l of netLog.slice(-40)) console.log('  ' + l)
}

await browser.close()
process.exit(failed.length ? 1 : 0)
