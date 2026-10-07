// 渲染冒烟测试：把真实的组件在 Node 里服务端渲染一遍，再加一道「模板里用到的名字都存在」的静态检查。
//
// 为什么需要它：`vite build` 对下面几类错误一概放行，它们都只在**渲染期**才抛异常，
// 表现是整个页面白屏（组件被 Vue 卸载），而这个页面已经因此白屏过两次——
//   · v-if 与 v-for 写在同一个元素上（v-if 先执行，此时循环变量还不存在）
//   · 兄弟节点引用了 v-for 的别名（别名只在带 v-for 的那个元素里有效）
//   · 模板里用了脚本里不存在的名字（拼错的函数名 / 变量名）
// check-vue-templates.mjs 用文本规则挡前两类；这里补上「真的渲染一遍」，并补上第三类的静态检查。
//
// 渲染用的是 SSR：不需要浏览器，也不需要 jsdom。组件内部状态（tasks、runResults ……）
// 在 created 钩子里直接写进 setupState，就能把页面推到「核对窗口打开」「被补录拦下」之类
// 平时要点好几步才能到的状态，逐个渲染一遍。
//
// 用法：node scripts/smoke-render.mjs   （npm run build 会自动先跑它）

import { readFileSync, readdirSync } from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { createServer } from 'vite'
import { createSSRApp } from 'vue'
import { renderToString } from 'vue/server-renderer'
import { compileScript, compileTemplate, parse } from 'vue/compiler-sfc'

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')
const srcDir = path.join(root, 'src')

let failures = 0
const fail = (msg) => {
  failures++
  console.error('  ✗ ' + msg)
}
const pass = (msg) => console.log('  ✓ ' + msg)

// 卡死比失败更糟（CI 会一直挂着），给一个总的兜底超时。
setTimeout(() => {
  console.error('渲染冒烟测试超时（90s），强制退出')
  process.exit(2)
}, 90_000).unref()

// ---------------------------------------------------------------------------
// 检查一：模板里引用的名字，脚本里都得有。
//
// 用与 plugin-vue 相同的方式编译模板（带上 <script setup> 的 bindings）：
// 能在脚本里找到的名字会被编成 $setup.xxx，**找不到的编成 _ctx.xxx**。
// 所以编出来的渲染函数里凡是出现 _ctx.xxx，就是模板用了脚本里不存在的名字。
// ---------------------------------------------------------------------------
function unresolvedNames(file) {
  const source = readFileSync(file, 'utf8')
  const { descriptor, errors } = parse(source, { filename: file })
  if (errors.length) throw errors[0]
  if (!descriptor.template) return []
  const id = 'smoke'
  const script = compileScript(descriptor, { id, inlineTemplate: false })
  const tpl = compileTemplate({
    source: descriptor.template.content,
    filename: file,
    id,
    compilerOptions: { bindingMetadata: script.bindings, prefixIdentifiers: true },
  })
  if (tpl.errors.length) throw tpl.errors[0]

  // $emit / $slots 一类是 Vue 自带的实例属性；window / document 是浏览器全局。
  const allowed = new Set(['window', 'document', 'navigator', 'console', 'location'])
  const names = new Set()
  for (const m of tpl.code.matchAll(/_ctx\.([A-Za-z_]\w*)/g)) names.add(m[1])
  return [...names].filter((n) => !allowed.has(n))
}

console.log('检查一：模板引用的名字在脚本里都存在')
const sfcFiles = readdirSync(srcDir).filter((f) => f.endsWith('.vue'))
for (const f of sfcFiles) {
  try {
    const missing = unresolvedNames(path.join(srcDir, f))
    if (missing.length > 0) fail(`${f}：模板用到了脚本里不存在的名字 → ${missing.join(', ')}`)
    else pass(f)
  } catch (err) {
    fail(`${f}：编译失败 → ${err.message}`)
  }
}

// ---------------------------------------------------------------------------
// 检查二：把组件推到各种状态，逐个渲染，要求不抛异常、不出现渲染期告警。
// ---------------------------------------------------------------------------

// 这些告警都意味着「渲染期访问了不存在的东西」，是白屏的前兆。
const FATAL_WARN = /was accessed during render but is not defined|Cannot read prop|is not a function|is not defined/

const vite = await createServer({
  root,
  logLevel: 'error',
  appType: 'custom',
  server: { middlewareMode: true, hmr: false, ws: false, watch: null },
  optimizeDeps: { noDiscovery: true, include: [] },
})

async function loadSfc(name) {
  return (await vite.ssrLoadModule('/src/' + name)).default
}

// render 渲染一次组件。state 里的键直接写进它的 setupState（相当于用户已经操作到了那一步）。
async function render(Component, state = {}) {
  const warnings = []
  const app = createSSRApp(Component)
  app.config.warnHandler = (msg) => warnings.push(msg)
  app.config.errorHandler = (err) => {
    throw err
  }
  app.mixin({
    created() {
      // 只对根组件写状态。不能用 this.$.type === Component 判断：
      // 经过 vite 加载的 SFC，运行时拿到的组件对象与导入的默认导出并不是同一个引用。
      if (this.$.parent) return
      for (const [k, v] of Object.entries(state)) this.$.setupState[k] = v
    },
  })
  const html = await renderToString(app)
  return { html, warnings }
}

// scenario 跑一个场景：渲染 → 检查无异常无致命告警 → 检查页面内容。
async function scenario(label, Component, state, { has = [], hasNot = [], regex = [], notRegex = [] } = {}) {
  let out
  try {
    out = await render(Component, state)
  } catch (err) {
    fail(`${label}：渲染抛异常 → ${err && err.message}`)
    return
  }
  const bad = out.warnings.filter((w) => FATAL_WARN.test(w))
  if (bad.length > 0) {
    fail(`${label}：渲染期告警 → ${bad[0].split('\n')[0]}`)
    return
  }
  const problems = []
  for (const s of has) if (!out.html.includes(s)) problems.push(`缺少「${s}」`)
  for (const s of hasNot) if (out.html.includes(s)) problems.push(`不该出现「${s}」`)
  for (const [re, why] of regex) if (!re.test(out.html)) problems.push(why)
  for (const [re, why] of notRegex) if (re.test(out.html)) problems.push(why)
  if (problems.length > 0) fail(`${label}：${problems.join('；')}`)
  else pass(label)
}

// ---- 造数据：形状与后端真实返回一致 ----

const task = (over = {}) => ({
  id: 1, customerId: 1, customerName: '龙树', name: '龙数9月',
  periodYear: 2026, periodMonth: 9,
  startAt: '2026-09-01T00:00:00', endAt: '2026-09-30T23:59:59',
  generateSanitized: true, generateCost: true, checkCost: true, reviewUpstream: true,
  useManualDiscount: false, billTemplate: '',
  settleCny: 732.4869, costCny: 66.656, profitCny: 665.8311, costedSettleCny: 732.4869,
  costComplete: true, lastRunAt: '2026-10-07T10:00:00Z', runCount: 1,
  summaryText: '客户：龙树\n账期：2026-09', ...over,
})

const reviewChannel = (over = {}) => ({
  channelId: 858, name: 'GLM-858', known: true, upstreamRatio: 0.4, isDomestic: false,
  groups: ['GLM'], models: ['glm-5.2', 'glm-5.1'], rows: 2925, amountCny: 405.94,
  allDomesticModels: true, anyDomesticModels: true, ...over,
})

const reviewResult = (over = {}) => ({
  taskId: 1, ok: false, needsUpstreamReview: true, taskName: '龙数9月',
  customerName: '龙树', customerId: 1, periodYear: 2026, periodMonth: 9,
  channelCheck: {
    needsUpstreamReview: true, totalRows: 7909,
    // no_upstream_ratio 在预检里是「渠道数」不是「行数」，核对窗口必须不拿它造句。
    uncostableRows: { no_upstream_ratio: 2, no_channel: 3 },
    reviewChannels: [
      reviewChannel(),
      reviewChannel({
        channelId: 669, name: 'MiniMax-669', upstreamRatio: 0.55, isDomestic: true,
        groups: ['M3'], models: ['MiniMax-M3'], rows: 1651, amountCny: 65.96,
      }),
      reviewChannel({
        channelId: 1234, name: '渠道 1234（不在渠道清单里）', known: false, upstreamRatio: null,
        groups: [], models: [], rows: 3, amountCny: 0.1,
        allDomesticModels: false, anyDomesticModels: false,
      }),
    ],
  },
  ...over,
})

const blockedResult = (over = {}) => ({
  taskId: 2, ok: false, needsChannelRatios: true, taskName: '孙鹏9月', customerId: 2,
  customerName: '孙鹏对公',
  channelCheck: {
    totalRows: 100, missingGroupRatioRows: 0, uncostableRows: { no_upstream_ratio: 1 },
    missing: [{
      channelId: 858, name: 'GLM-858', known: true, groups: ['GLM'], rowCount: 2925,
      isDomestic: false, models: ['glm-5.2'], allDomesticModels: true, anyDomesticModels: true,
    }],
  },
  ...over,
})

const okResult = (over = {}) => ({
  taskId: 3, ok: true, taskName: '巨日禄9月', billUrl: '/api/download/j/bill',
  costSummary: '客户：巨日禄\n上游成本：¥1', ...over,
})

console.log('检查二：BillTasks.vue 各状态渲染')
const BillTasks = await loadSfc('BillTasks.vue')

await scenario('空页面', BillTasks, {}, { has: ['账单导出任务'] })

await scenario('计划列表（含摘要展开行，v-for 作用域）', BillTasks, {
  tasks: [task(), task({ id: 2, name: '第二条', reviewUpstream: false, checkCost: false, billTemplate: 'simple', summaryText: '' })],
  expandedSummary: 1,
}, {
  has: ['龙数9月', '第二条', '客户：龙树'],
  // 「核对」标记只出现在勾了核对且开了成本核算的那一条上。
  regex: [[/class="tag"[^>]*>核对<\/span>/, '勾了核对的计划应带「核对」标记']],
})

await scenario('计划表单：勾了核对 → 说明文字出现', BillTasks, {
  showPlanForm: true,
  customers: [{ id: 1, name: '龙树', usernames: 'longshu' }],
  planForm: { id: 0, customerId: 1, name: '', startAt: '', endAt: '', generateSanitized: true,
    generateCost: true, checkCost: true, reviewUpstream: true, useManualDiscount: false, billTemplate: '' },
}, { has: ['执行时核对上游倍率与国模标识', '每次执行都会在导出日志后停下来'] })

await scenario('计划表单：成本核算关闭 → 核对开关禁用且不出说明', BillTasks, {
  showPlanForm: true,
  customers: [{ id: 1, name: '龙树', usernames: 'longshu' }],
  planForm: { id: 0, customerId: 1, name: '', startAt: '', endAt: '', generateSanitized: true,
    generateCost: true, checkCost: false, reviewUpstream: true, useManualDiscount: false, billTemplate: '' },
}, {
  has: ['执行时核对上游倍率与国模标识'],
  hasNot: ['每次执行都会在导出日志后停下来'],
  regex: [[/<input[^>]*type="checkbox"[^>]*disabled[^>]*>\s*执行时核对/, '成本核算关闭时核对开关应为禁用']],
})

await scenario('核对窗口打开', BillTasks, {
  tasks: [task()],
  runResults: [reviewResult()],
  reviewOpen: true,
  // 倍率草稿故意混着字符串与数字：type=number 的 v-model 会给出 number，
  // 模板里的函数若直接 .trim() 就会在这里抛 TypeError（白屏）。
  reviewRatioDraft: { 858: '0.4', 669: 0.55, 1234: '' },
  reviewDomesticDraft: { 858: false, 669: true, 1234: false },
}, {
  has: [
    '核对上游渠道倍率与国模标识', 'GLM-858', 'MiniMax-669', '不在渠道清单里',
    '0.57 折', // 渠道 858：海外口径，0.4 ÷ 7
    '5.5 折', //  渠道 669：国模口径，0.55 就是 5.5 折
    '日志里只跑国产模型，通常应标为国模渠道', // 858 看起来该标国模却没标
    '还没维护倍率，必须填写', // 1234
    '3 行日志里取不到渠道号',
  ],
  hasNot: [
    '需要先补录', // 也不是缺倍率被拦
    '2 行渠道未维护', // 预检里这一类是渠道数，不能套进「N 行」
  ],
  notRegex: [[/执行失败 \d+ 条/, '不该出现「执行失败 N 条」标题']], // 停在核对上不是失败
  regex: [
    [/<button[^>]*btn-primary[^>]*disabled[^>]*>\s*确认无误/, '有渠道没填倍率时「继续」应禁用'],
  ],
})

await scenario('核对窗口：标了国模但模型名识别不出国产厂商 → 警示 7 倍风险', BillTasks, {
  tasks: [task()],
  runResults: [reviewResult({
    channelCheck: {
      needsUpstreamReview: true, totalRows: 10, uncostableRows: {},
      reviewChannels: [reviewChannel({
        channelId: 5, name: '豆包渠道', upstreamRatio: 0.4, isDomestic: true,
        models: ['doubao-pro-32k'], allDomesticModels: false, anyDomesticModels: false,
      })],
    },
  })],
  reviewOpen: true,
  reviewRatioDraft: { 5: '0.4' },
  reviewDomesticDraft: { 5: true },
}, {
  // 标准明细对识别不出的国产模型会把人民币刊例当美金再乘汇率，所以提示里要点明这个后果。
  has: ['豆包渠道', 'doubao-pro-32k', '按模型名识别不出国产厂商的模型', '偏高约 7 倍'],
})

await scenario('核对窗口：全部填好、有改动 → 按钮变成「保存修改并继续」且可点', BillTasks, {
  tasks: [task()],
  runResults: [reviewResult()],
  reviewOpen: true,
  // 渠道 1234 补上倍率；渠道 858 勾上国模（与库里现状不同 → 有改动）。
  reviewRatioDraft: { 858: '0.4', 669: '0.55', 1234: '0.6' },
  reviewDomesticDraft: { 858: true, 669: true, 1234: false },
}, {
  has: ['保存修改并继续执行', '2 个渠道的改动', '4 折'],
  regex: [[/<button[^>]*btn-primary(?![^>]*disabled)[^>]*>\s*保存修改并继续执行/, '填完且有改动时按钮应可点']],
})

await scenario('核对窗口关闭 → 结果区留重新打开的入口', BillTasks, {
  tasks: [task()], runResults: [reviewResult()], reviewOpen: false,
}, {
  has: ['等待核对上游倍率与国模标识（1 条）', '打开核对窗口', '还没有出账'],
  hasNot: ['modal-mask'],
  notRegex: [[/执行失败 \d+ 条/, '不该出现「执行失败 N 条」标题']],
})

await scenario('被缺倍率拦下：补录面板带国模勾选与提示', BillTasks, {
  tasks: [task()],
  runResults: [blockedResult()],
  blockedRatioDraft: { 858: 0.4 }, // number，同上
  blockedDomesticDraft: { 858: false },
}, {
  has: ['需要先补录（1 条）', '国模渠道', '折合官方折扣', '0.57 折', '待保存', '日志里只跑国产模型，通常应标为国模渠道'],
  notRegex: [[/执行失败 \d+ 条/, '不该出现「执行失败 N 条」标题']],
})

await scenario('成功 + 被拦 + 待核对 同时出现', BillTasks, {
  tasks: [task()],
  runResults: [okResult(), blockedResult(), reviewResult()],
  reviewOpen: true,
  reviewRatioDraft: { 858: '0.4', 669: '0.55', 1234: '' },
  reviewDomesticDraft: { 858: false, 669: true, 1234: false },
  blockedRatioDraft: {}, blockedDomesticDraft: { 858: false },
}, {
  has: ['执行成功 1 条', '需要先补录（1 条）', '核对上游渠道倍率与国模标识', '巨日禄9月'],
  notRegex: [[/执行失败 \d+ 条/, '不该出现「执行失败 N 条」标题']],
})

await scenario('脏数据：核对清单缺字段、草稿没初始化 → 不得抛异常', BillTasks, {
  runResults: [{
    taskId: 9, ok: false, needsUpstreamReview: true,
    channelCheck: { needsUpstreamReview: true, reviewChannels: [{ channelId: 9 }, { channelId: 10, upstreamRatio: 'x' }] },
  }],
  reviewOpen: true,
  // 草稿一个都没有——模板里所有取值函数都得带兜底。
  reviewRatioDraft: {}, reviewDomesticDraft: {},
}, { has: ['核对上游渠道倍率与国模标识', '还没维护倍率，必须填写'] })

console.log('检查二：ChannelRatios.vue 各状态渲染')
const ChannelRatios = await loadSfc('ChannelRatios.vue')

await scenario('渠道页：国模列与折合折扣', ChannelRatios, {
  channelsLoaded: true,
  channels: [
    { channelId: 858, name: 'GLM-858', channelType: 1, status: 1, upstreamRatio: 0.4, note: '', isDomestic: true },
    { channelId: 101, name: 'AZ', channelType: 1, status: 1, upstreamRatio: 1.8, note: '', isDomestic: false },
    { channelId: 7, name: '新渠道', channelType: 1, status: 1, upstreamRatio: null, note: '', isDomestic: false },
  ],
  ratioDraft: { 858: 0.4, 101: '1.8', 7: '' }, // 同样混着 number 与 string
  ratioNotes: { 858: '', 101: '', 7: '' },
  domesticDraft: { 858: true, 101: false, 7: false },
}, {
  has: ['国模渠道', '折合官方折扣', '4 折', '2.57 折'], // 858 国模 0.4→4 折；101 海外 1.8÷7=0.257→2.57 折
})

await scenario('渠道页：草稿为空也不得抛异常', ChannelRatios, {
  channelsLoaded: true,
  channels: [{ channelId: 1, name: 'x', upstreamRatio: null }],
  ratioDraft: {}, ratioNotes: {}, domesticDraft: {},
}, { has: ['渠道成本倍率维护'] })

console.log('检查二：App.vue 各状态渲染')
const App = await loadSfc('App.vue')

await scenario('出账页：登录后空状态', App, { authChecked: true, authenticated: true }, {
  // 没有 has 会让「什么都没渲染」也算通过（authChecked 不置位时就是这样），
  // 所以必须断言页面骨架真的出来了。
  has: ['钛动账单工具', '生成账单'],
})

await scenario('出账页：渠道检查结果里的国模列', App, {
  authChecked: true,
  authenticated: true,
  checkChannelsDone: true,
  usedChannels: [
    { channelId: 858, name: 'GLM-858', upstreamRatio: 0.4, isDomestic: true },
    { channelId: 101, name: 'AZ', upstreamRatio: 1.8, isDomestic: false },
    { channelId: 7, name: '新渠道', upstreamRatio: null, isDomestic: false },
  ],
  usedRatioDraft: { 858: 0.4, 101: '1.8', 7: '' }, // number 与 string 混着
  usedDomesticDraft: { 858: true, 101: false, 7: false },
  usedGroupChannels: { GLM: [858] },
  usedReviewById: {
    858: { channelId: 858, models: ['glm-5.2'], allDomesticModels: true, anyDomesticModels: true },
    101: { channelId: 101, models: ['glm-5.1'], allDomesticModels: true, anyDomesticModels: true },
  },
}, {
  has: [
    '国模渠道', '折合官方折扣', 'glm-5.2',
    '4 折', //     858：国模，0.4 就是 4 折
    '2.57 折', //  101：海外，1.8 ÷ 7 = 0.257 → 2.57 折
    '日志里只跑国产模型，通常应标为国模渠道', // 101 只跑国产模型却没标国模
  ],
})

await scenario('出账页：渠道检查数据缺字段 → 不得抛异常', App, {
  authChecked: true,
  authenticated: true,
  checkChannelsDone: true,
  usedChannels: [{ channelId: 1 }, { channelId: 2, upstreamRatio: 'x' }],
  usedRatioDraft: {}, usedDomesticDraft: {}, usedReviewById: {},
}, { has: ['钛动账单工具'] })

// ---------------------------------------------------------------------------
// 检查三：交互逻辑。
//
// 渲染只能证明「页面不崩」，证明不了「点下去之后对不对」。这里把组件里真实的函数跑一遍
// （fetch 用桩替掉，记录发出的请求），重点验证：
//   · 核对 / 补录之后的重跑一定带 skipUpstreamReview——否则每次重跑又停回同一个弹窗，永远出不了账；
//   · 首次执行不带它——否则核对永远被跳过，开关形同虚设；
//   · 草稿只补缺、不覆盖；改动检测与保存请求的形状（带 isDomestic、不带 note）。
// ---------------------------------------------------------------------------

// mount 渲染一次，并把根组件的 setupState 带回来，之后可以直接调用组件里的函数、读写它的状态。
async function mount(Component, state = {}) {
  let vm = null
  const app = createSSRApp(Component)
  app.config.errorHandler = (err) => {
    throw err
  }
  app.mixin({
    created() {
      if (this.$.parent) return
      vm = this.$.setupState
      for (const [k, v] of Object.entries(state)) vm[k] = v
    },
  })
  await renderToString(app)
  return vm
}

// stubFetch 把全局 fetch 换成桩：记录每次请求，按路径返回预设的响应；没预设的路径直接报错，
// 这样「不该发的请求发了」会当场暴露，而不是悄悄通过。
function stubFetch(routes) {
  const calls = []
  globalThis.fetch = async (url, opts = {}) => {
    const body = opts.body ? JSON.parse(opts.body) : undefined
    calls.push({ url, method: opts.method || 'GET', body })
    if (!(url in routes)) throw new Error('未预期的请求：' + url)
    const payload = typeof routes[url] === 'function' ? routes[url](body) : routes[url]
    return { ok: true, status: 200, json: async () => payload }
  }
  return calls
}

const same = (label, got, want) => {
  const g = JSON.stringify(got)
  const w = JSON.stringify(want)
  if (g === w) pass(label)
  else fail(`${label}：期望 ${w}，实际 ${g}`)
}

console.log('检查三：BillTasks.vue 交互逻辑')

{
  const vm = await mount(BillTasks, {
    runResults: [
      reviewResult(),
      blockedResult({
        taskId: 2,
        channelCheck: { missing: [{ channelId: 777, name: 'n', isDomestic: true, groups: [], models: [] }] },
      }),
    ],
    reviewRatioDraft: { 858: '9' }, // 用户已经改过的
  })
  vm.seedChannelDrafts()
  same('草稿只补缺不覆盖：用户改过的 858 保持不变', vm.reviewRatioDraft[858], '9')
  same('草稿补缺：669 取库里的倍率', vm.reviewRatioDraft[669], '0.55')
  same('草稿补缺：未维护的 1234 是空串', vm.reviewRatioDraft[1234], '')
  same('国模草稿取库里现状', [vm.reviewDomesticDraft[858], vm.reviewDomesticDraft[669]], [false, true])
  same('补录面板的国模勾选同样取库里现状（先标了国模的不能被冲掉）', vm.blockedDomesticDraft[777], true)
}

{
  const vm = await mount(BillTasks, {
    // 同一批渠道出现在两条计划里：改一次就够，提交时不能重复。
    runResults: [reviewResult(), reviewResult({ taskId: 5 })],
    reviewRatioDraft: { 858: '0.4', 669: '0.55', 1234: '0.6' },
    reviewDomesticDraft: { 858: true, 669: true, 1234: false },
  })
  const { items } = vm.reviewChangedItems()
  same('只提交有改动的渠道，同一渠道跨计划只算一次', items, [
    { channelId: 858, upstreamRatio: 0.4, isDomestic: true },
    { channelId: 1234, upstreamRatio: 0.6, isDomestic: false },
  ])
  same('不带 note：后端把没传的 note 当「保持原值」', items.some((i) => 'note' in i), false)
  same('改动数', vm.reviewDirtyCount, 2)

  vm.reviewRatioDraft = { 858: '-1', 669: '0.55', 1234: '0.6' }
  same('非法倍率会报错而不是静默提交', typeof vm.reviewChangedItems().error, 'string')

  // 0.40 与库里的 0.4 是同一个数，不能被当成改动而白白提交一次。
  vm.reviewRatioDraft = { 858: '0.40', 669: '0.55', 1234: '0.6' }
  vm.reviewDomesticDraft = { 858: false, 669: true, 1234: false }
  same('0.40 与 0.4 不算改动（1234 补了倍率才是）', vm.reviewChangedItems().items.map((i) => i.channelId), [1234])
}

{
  const vm = await mount(BillTasks, {})
  same('国模 0.4 → 4 折', vm.discountText('0.4', true), '4 折')
  same('海外 0.4 → 0.57 折', vm.discountText('0.4', false), '0.57 折')
  same('倍率为空 → —', vm.discountText('', true), '—')
  same('非数字 → —', vm.discountText('abc', false), '—')
  same('number 类型的草稿（type=number 的 v-model）也能处理', vm.discountText(0.4, true), '4 折')
  vm.discountBaseFactor = 8
  same('换算基数取自后端、不是写死的 7', vm.discountText('0.4', false), '0.5 折')
}

{
  // 首次执行：不带 skipUpstreamReview，停在核对上时自动弹窗。
  const calls = stubFetch({
    '/api/validate-bill-tasks': { validations: [{ taskId: 1, taskName: '龙数9月', error: '' }], runnable: 1, blocked: 0 },
    '/api/run-bill-tasks': { results: [reviewResult()], okCount: 0, failCount: 0, discountBaseFactor: 8 },
    '/api/bill-tasks': { tasks: [task()], summaries: [] },
  })
  const vm = await mount(BillTasks, { selectedTasks: [1], tasks: [task()] })
  await vm.runSelected()
  const run = calls.find((c) => c.url === '/api/run-bill-tasks')
  same('首次执行不带 skipUpstreamReview（否则核对永远被跳过）', run.body, { taskIds: [1] })
  same('停在核对上 → 自动弹出核对窗口', vm.reviewOpen, true)
  same('后端给的换算基数被采用', vm.discountBaseFactor, 8)
  same('草稿已用库里现状初始化', [vm.reviewRatioDraft[858], vm.reviewDomesticDraft[669]], ['0.4', true])
  same('提示里说明有计划在等待核对，而不是「0 条全部成功」', /等待核对\/补录/.test(vm.message), true)
}

{
  // 核对后继续：先保存改动，再带 skipUpstreamReview 重跑。
  const calls = stubFetch({
    '/api/channel-ratios': { saved: 2 },
    '/api/run-bill-tasks': { results: [okResult({ taskId: 1 })], okCount: 1, failCount: 0 },
    '/api/bill-tasks': { tasks: [], summaries: [] },
  })
  const vm = await mount(BillTasks, {
    runResults: [reviewResult()],
    reviewOpen: true,
    reviewRatioDraft: { 858: '0.4', 669: '0.55', 1234: '0.6' },
    reviewDomesticDraft: { 858: true, 669: true, 1234: false },
  })
  await vm.continueReview()
  const save = calls.find((c) => c.url === '/api/channel-ratios')
  same('先保存改动（带 isDomestic、不带 note）', save.body.items, [
    { channelId: 858, upstreamRatio: 0.4, isDomestic: true },
    { channelId: 1234, upstreamRatio: 0.6, isDomestic: false },
  ])
  const rerun = calls.find((c) => c.url === '/api/run-bill-tasks')
  same('重跑必须带 skipUpstreamReview，否则每次重跑都停回同一个弹窗', rerun.body, { taskIds: [1], skipUpstreamReview: true })
  same('保存发生在重跑之前', calls.map((c) => c.url).slice(0, 2), ['/api/channel-ratios', '/api/run-bill-tasks'])
  same('核对窗口已关闭', vm.reviewOpen, false)
  same('重跑结果并回了 runResults', vm.runResults.map((r) => r.ok), [true])
}

{
  // 没有任何改动：不该发保存请求，只重跑。
  const calls = stubFetch({
    '/api/run-bill-tasks': { results: [okResult({ taskId: 1 })], okCount: 1, failCount: 0 },
    '/api/bill-tasks': { tasks: [], summaries: [] },
  })
  const clean = reviewResult()
  clean.channelCheck.reviewChannels = clean.channelCheck.reviewChannels.slice(0, 2) // 去掉没维护的那个
  const vm = await mount(BillTasks, {
    runResults: [clean],
    reviewOpen: true,
    reviewRatioDraft: { 858: '0.4', 669: '0.55' },
    reviewDomesticDraft: { 858: false, 669: true },
  })
  await vm.continueReview()
  same('确认无误：不发保存请求，只发重跑', calls.map((c) => c.url), ['/api/run-bill-tasks', '/api/bill-tasks'])
  same('重跑带 skipUpstreamReview', calls[0].body, { taskIds: [1], skipUpstreamReview: true })
}

{
  // 还有渠道没填倍率：什么请求都不该发，窗口不关。
  const calls = stubFetch({})
  const vm = await mount(BillTasks, {
    runResults: [reviewResult()],
    reviewOpen: true,
    reviewRatioDraft: { 858: '0.4', 669: '0.55', 1234: '' },
    reviewDomesticDraft: { 858: false, 669: true, 1234: false },
  })
  same('未维护的渠道数', vm.reviewMissingCount, 1)
  await vm.continueReview()
  same('还有渠道没填倍率时不发任何请求', calls.length, 0)
  same('给出原因', /还有 1 个渠道没填上游倍率/.test(vm.reviewError), true)
  same('窗口不关', vm.reviewOpen, true)
}

{
  // 缺倍率补录后继续：同样带 skipUpstreamReview；保存带 isDomestic、不带 note。
  const calls = stubFetch({
    '/api/channel-ratios': { saved: 1 },
    '/api/run-bill-tasks': { results: [okResult({ taskId: 2 })], okCount: 1, failCount: 0 },
    '/api/bill-tasks': { tasks: [], summaries: [] },
  })
  const vm = await mount(BillTasks, {
    runResults: [blockedResult()],
    blockedRatioDraft: { 858: 0.4 }, // number
    blockedDomesticDraft: { 858: true },
  })
  await vm.continueAfterFix()
  same('补录保存的形状', calls.find((c) => c.url === '/api/channel-ratios').body.items,
    [{ channelId: 858, upstreamRatio: 0.4, isDomestic: true }])
  same('补录后重跑同样带 skipUpstreamReview', calls.find((c) => c.url === '/api/run-bill-tasks').body,
    { taskIds: [2], skipUpstreamReview: true })
}

console.log('检查三：ChannelRatios.vue 交互逻辑')
{
  const calls = stubFetch({
    '/api/channel-ratios': { saved: 1 },
    '/api/channels': { channels: [], discountBaseFactor: 7 },
  })
  const vm = await mount(ChannelRatios, {
    channelsLoaded: true,
    channels: [
      { channelId: 1, name: 'a', upstreamRatio: 0.4, note: '', isDomestic: false },
      { channelId: 2, name: 'b', upstreamRatio: 1.8, note: '', isDomestic: false },
    ],
    ratioDraft: { 1: '0.4', 2: '1.8' },
    ratioNotes: { 1: '', 2: '' },
    domesticDraft: { 1: true, 2: false }, // 只勾了 1 的国模，倍率没动
  })
  same('只改国模勾选也算改动（保存按钮上的数）', vm.pendingCount, 1)
  await vm.saveChannelRatios()
  same('保存的内容带 isDomestic', calls.find((c) => c.url === '/api/channel-ratios').body.items,
    [{ channelId: 1, upstreamRatio: 0.4, note: '', isDomestic: true }])
}

console.log('检查三：App.vue 交互逻辑')
{
  const calls = stubFetch({ '/api/channel-ratios': { saved: 1 } })
  const vm = await mount(App, {
    authChecked: true, authenticated: true,
    usedChannels: [{ channelId: 1, upstreamRatio: 0.4, isDomestic: false }],
    usedRatioDraft: { 1: '' }, // 倍率框是空的，只勾了国模
    usedDomesticDraft: { 1: true },
  })
  same('只改国模勾选也算「有改动」', vm.usedUnsavedCount, 1)
  await vm.saveUsedRatios()
  same('倍率框为空时沿用库里原值，不能借此取消维护；带 isDomestic、不带 note',
    calls.find((c) => c.url === '/api/channel-ratios').body.items,
    [{ channelId: 1, upstreamRatio: 0.4, isDomestic: true }])
}

await vite.close()

console.log(failures === 0 ? '\n渲染冒烟测试全部通过' : `\n渲染冒烟测试失败：${failures} 项`)
process.exit(failures === 0 ? 0 : 1)
