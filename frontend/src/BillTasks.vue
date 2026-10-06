<script setup>
import { computed, onMounted, ref } from 'vue'
import { copyText, selectElementText } from './clipboard'

// 账单导出任务：**可维护的计划** + 选择性批量执行。
//
// 与上一版的区别：任务不再是「填参数 → 立刻跑」的一次性动作，而是先建好计划
// （客户 + 时段 + 勾选），再勾选执行。一个客户可以有多条计划——
// 月度对账一条，按周导的每周一条——这也是唯一键从 (客户, 账期) 改成任务自身的原因。

const emit = defineEmits(['unauthorized'])

const customers = ref([])
const tasks = ref([])
const summaries = ref([])
const settings = ref(null)
const jobRetentionHours = ref(6)

const loading = ref(false)
const savingPlan = ref(false)
const validating = ref(false)
const running = ref(false)
const savingSettings = ref(false)
const showSettings = ref(false)
const showPlanForm = ref(false)
const error = ref('')
const message = ref('')

// 计划表单。id 为 0 表示新建。
//
// startAt/endAt 是**北京时间墙上时间**字符串（"YYYY-MM-DDTHH:mm:ss"），
// 直接绑定 <input type="datetime-local">，中间不经过任何 Date 转换——
// 后端按 +08:00 解释并回存，前端按原样展示，两边看到的是同一个时刻。
const planForm = ref({
  id: 0,
  customerId: 0,
  name: '',
  startAt: '',
  endAt: '',
  generateSanitized: true,
  // 成本核算：执行前检查本任务日志用到的渠道有没有维护上游倍率，
  // 缺了就拦下让用户就地补录。默认勾选（与后端 CreateBillTask 的默认值一致）。
  //
  // 与 generateCost 是两件事：checkCost 管「成本要不要算得对」，
  // generateCost 管「要不要那张独立的成本利润表」。简易模板没有那张表，
  // 但它的账单上有成本三列——所以简易模板下 generateCost 无意义、checkCost 照样有用。
  checkCost: true,
  // 是否套用该客户手工维护的「分组 → 折扣」（线下谈定、没同步到 new-api 的）。
  // 默认不勾：折扣直接决定收客户多少钱，不该在用户没表态时自动套用人工数值。
  useManualDiscount: false,
  generateCost: true,
  // 出账模板：'' = 标准明细账单，'simple' = 简易汇总账单。
  // 存在计划上而不是全局设置里：同一个部署里两类客户都可能存在。
  billTemplate: '',
})

// 勾选与批量执行结果
const selectedTasks = ref([])
const runResults = ref([])
const validationResults = ref([])
// runResults 里带下载链接的那几条，用于展示
const runFailures = computed(() => runResults.value.filter((r) => !r.ok && !r.needsChannelRatios))
const runSuccesses = computed(() => runResults.value.filter((r) => r.ok))
// 被成本核算预检拦下的那几条。**不是失败**：没有产物，但用户补录倍率后重跑即可，
// 所以不能混进「执行失败」里——那会让用户以为任务坏了，去查根本不存在的 bug。
const runBlocked = computed(() => runResults.value.filter((r) => r.needsChannelRatios))

// 复制按钮的状态，按任务 ID 记（见 copySummary）。
const copyStates = ref({})

const settingsDraft = ref({
  priceSource: 'db',
  exchangeRate: 7,
  discount: '',
  domesticMarkers: '',
  sanitizedFormat: 'tsv',
})

const hasCustomers = computed(() => customers.value.length > 0)
const isEditingPlan = computed(() => planForm.value.id > 0)

const allSelected = computed(
  () => tasks.value.length > 0 && selectedTasks.value.length === tasks.value.length
)

// 有勾选但还没执行过的计划数——用它提示「先执行再下载」。
const selectedCount = computed(() => selectedTasks.value.length)

const hasMissingCost = computed(() =>
  summaries.value.some((s) => (s.missingCostCount || 0) > 0 || (s.partialCostCount || 0) > 0)
)

const hasUnrun = computed(() => summaries.value.some((s) => (s.unrunCount || 0) > 0))

async function loadAll() {
  error.value = ''
  loading.value = true
  try {
    await Promise.all([loadCustomers(), loadTasks(), loadSettings()])
  } finally {
    loading.value = false
  }
}

async function loadCustomers() {
  const resp = await fetch('/api/customers')
  const data = await resp.json()
  if (!resp.ok) {
    if (resp.status === 401) emit('unauthorized')
    error.value = data.error || `读取客户失败（${resp.status}）`
    return
  }
  customers.value = data.customers || []
}

async function loadTasks() {
  const resp = await fetch('/api/bill-tasks')
  const data = await resp.json()
  if (!resp.ok) {
    if (resp.status === 401) emit('unauthorized')
    error.value = data.error || `读取任务失败（${resp.status}）`
    return
  }
  tasks.value = data.tasks || []
  summaries.value = data.summaries || []
  if (data.jobRetentionHours) jobRetentionHours.value = data.jobRetentionHours

  // 列表刷新后丢掉已经不存在的勾选项，避免执行到已删除的计划。
  const present = new Set(tasks.value.map((t) => t.id))
  selectedTasks.value = selectedTasks.value.filter((id) => present.has(id))
}

async function loadSettings() {
  const resp = await fetch('/api/bill-task-settings')
  const data = await resp.json()
  if (!resp.ok) {
    if (resp.status === 401) emit('unauthorized')
    error.value = data.error || `读取默认参数失败（${resp.status}）`
    return
  }
  const s = data.settings || {}
  settings.value = s
  settingsDraft.value = {
    priceSource: s.priceSource || 'db',
    exchangeRate: s.exchangeRate || 7,
    discount: s.discount === null || s.discount === undefined ? '' : String(s.discount),
    domesticMarkers: s.domesticMarkers || '',
    sanitizedFormat: s.sanitizedFormat || 'tsv',
  }
}

async function saveSettings() {
  error.value = ''
  message.value = ''
  savingSettings.value = true
  try {
    // 折扣留空 = 不强制，按分组自动反推。空串要转成 null，不能发 0——
    // 0 会被当成「折扣为零」把结算额算成 0。
    const raw = String(settingsDraft.value.discount ?? '').trim()
    let discount = null
    if (raw !== '') {
      const n = Number(raw)
      if (!Number.isFinite(n) || n <= 0) {
        error.value = '折扣必须大于 0，留空表示按分组自动反推'
        return
      }
      discount = n
    }

    const resp = await fetch('/api/bill-task-settings', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        priceSource: settingsDraft.value.priceSource,
        exchangeRate: Number(settingsDraft.value.exchangeRate) || 7,
        discount,
        domesticMarkers: settingsDraft.value.domesticMarkers,
        sanitizedFormat: settingsDraft.value.sanitizedFormat,
      }),
    })
    const data = await resp.json()
    if (!resp.ok) {
      if (resp.status === 401) emit('unauthorized')
      error.value = data.error || `保存失败（${resp.status}）`
      return
    }
    settings.value = data.settings
    message.value = '默认出账参数已保存'
  } catch (err) {
    error.value = '保存失败：' + err.message
  } finally {
    savingSettings.value = false
  }
}

// ---- 计划表单 ----

// 快捷预设：只帮用户少敲几个数字，与账期解析无关（那在服务端按 +08:00 做）。
//
// 用本地日期算「上月/上周」是**可以**的：用户所在时区就是他理解「今天」的时区，
// 预设只是把这几个数字填进输入框，真正的时间语义由后端按 +08:00 解释。
// 起止分别补 00:00:00 / 23:59:59，与「整天」的直觉一致（闭区间，末日最后一秒也含）。
function applyPreset(kind) {
  const today = new Date()
  const fmt = (d) => {
    const p = (n) => String(n).padStart(2, '0')
    return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}`
  }
  const dayStart = (d) => `${fmt(d)}T00:00:00`
  const dayEnd = (d) => `${fmt(d)}T23:59:59`

  if (kind === 'lastMonth') {
    const first = new Date(today.getFullYear(), today.getMonth() - 1, 1)
    const last = new Date(today.getFullYear(), today.getMonth(), 0)
    planForm.value.startAt = dayStart(first)
    planForm.value.endAt = dayEnd(last)
  } else if (kind === 'lastWeek') {
    // 上一整周（周一~周日）。getDay() 的 0 是周日，换算成 ISO 的周一为起点。
    const dow = (today.getDay() + 6) % 7
    const thisMonday = new Date(today.getFullYear(), today.getMonth(), today.getDate() - dow)
    const lastMonday = new Date(thisMonday.getFullYear(), thisMonday.getMonth(), thisMonday.getDate() - 7)
    const lastSunday = new Date(thisMonday.getFullYear(), thisMonday.getMonth(), thisMonday.getDate() - 1)
    planForm.value.startAt = dayStart(lastMonday)
    planForm.value.endAt = dayEnd(lastSunday)
  } else if (kind === 'monthToDate') {
    planForm.value.startAt = dayStart(new Date(today.getFullYear(), today.getMonth(), 1))
    planForm.value.endAt = dayEnd(today)
  } else if (kind === 'today') {
    planForm.value.startAt = dayStart(today)
    planForm.value.endAt = dayEnd(today)
  }
}

function resetPlanForm() {
  planForm.value = {
    id: 0,
    customerId: planForm.value.customerId || 0,
    name: '',
    startAt: '',
    endAt: '',
    generateSanitized: planForm.value.generateSanitized,
    generateCost: planForm.value.generateCost,
    checkCost: planForm.value.checkCost,
    useManualDiscount: planForm.value.useManualDiscount,
    billTemplate: planForm.value.billTemplate,
  }
}

function startEditPlan(t) {
  error.value = ''
  message.value = ''
  showPlanForm.value = true
  // startAt/endAt 由后端给出北京时间墙上时间，原样回填，不做时区换算——
  // 换算一次就可能偏 8 小时，而且用户看不出哪里错了。
  planForm.value = {
    id: t.id,
    customerId: t.customerId,
    name: t.name || '',
    startAt: t.startAt || '',
    endAt: t.endAt || '',
    generateSanitized: !!t.generateSanitized,
    generateCost: !!t.generateCost,
    // 老计划没有这个字段（库里的列是后加的，默认 true），undefined 时按勾选算。
    checkCost: t.checkCost === undefined || t.checkCost === null ? true : !!t.checkCost,
    // 这个字段相反：老计划迁移时默认 true（保住既有口径），新建默认 false。
    // 但后端已经把这个默认值落在库里了，页面只需原样反映，不再自己兜底——
    // 兜底成 false 会让已在用线下折扣的老计划显示成未勾选，而实际出账仍生效。
    useManualDiscount: !!t.useManualDiscount,
    billTemplate: t.billTemplate || '',
  }
  window.scrollTo({ top: 0, behavior: 'smooth' })
}

async function savePlan() {
  error.value = ''
  message.value = ''
  if (!planForm.value.customerId) {
    error.value = '请选择客户'
    return
  }
  const hasStart = String(planForm.value.startAt || '').trim() !== ''
  const hasEnd = String(planForm.value.endAt || '').trim() !== ''
  if (hasStart !== hasEnd) {
    error.value = '开始时间与结束时间必须同时填写'
    return
  }

  savingPlan.value = true
  try {
    const resp = await fetch('/api/save-bill-task', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        id: planForm.value.id || 0,
        customerId: planForm.value.customerId,
        name: planForm.value.name,
        startAt: planForm.value.startAt,
        endAt: planForm.value.endAt,
        generateSanitized: planForm.value.generateSanitized,
        generateCost: planForm.value.generateCost,
        checkCost: planForm.value.checkCost,
        useManualDiscount: planForm.value.useManualDiscount,
        billTemplate: planForm.value.billTemplate,
      }),
    })
    const data = await resp.json()
    if (!resp.ok) {
      if (resp.status === 401) emit('unauthorized')
      error.value = data.error || `保存失败（${resp.status}）`
      return
    }
    message.value = isEditingPlan.value ? '计划已更新' : '计划已新增'
    resetPlanForm()
    showPlanForm.value = false
    await loadTasks()
  } catch (err) {
    error.value = '保存失败：' + err.message
  } finally {
    savingPlan.value = false
  }
}

async function deletePlan(t) {
  error.value = ''
  message.value = ''
  const ran = t.lastRunAt
    ? `\n该计划已执行过 ${t.runCount || 1} 次，删除会同时丢掉它的结算额/成本/利润统计（不影响已下载的文件）。`
    : ''
  if (!window.confirm(`确定删除计划「${t.name || t.customerName}」？${ran}`)) return
  try {
    const resp = await fetch('/api/delete-bill-task', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ id: t.id }),
    })
    const data = await resp.json()
    if (!resp.ok) {
      if (resp.status === 401) emit('unauthorized')
      error.value = data.error || `删除失败（${resp.status}）`
      return
    }
    message.value = '计划已删除'
    await loadTasks()
  } catch (err) {
    error.value = '删除失败：' + err.message
  }
}

// ---- 勾选与执行 ----

function toggleAll(checked) {
  selectedTasks.value = checked ? tasks.value.map((t) => t.id) : []
}

async function validateSelected() {
  error.value = ''
  message.value = ''
  validationResults.value = []
  if (selectedTasks.value.length === 0) {
    error.value = '请先勾选要执行的任务'
    return false
  }

  validating.value = true
  try {
    const resp = await fetch('/api/validate-bill-tasks', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ taskIds: selectedTasks.value }),
    })
    const data = await resp.json()
    if (!resp.ok) {
      if (resp.status === 401) emit('unauthorized')
      error.value = data.error || `校验失败（${resp.status}）`
      return false
    }
    validationResults.value = data.validations || []
    if (data.blocked > 0) {
      // 有问题的先摆出来让用户改，不往下跑——批量执行是同步的，
      // 跑到一半才报错，前面几条已经真地导了日志、出了账。
      message.value = `有 ${data.blocked} 条计划暂时不能执行，请按下面的原因修改后再执行。`
      return false
    }
    return true
  } catch (err) {
    error.value = '校验失败：' + err.message
    return false
  } finally {
    validating.value = false
  }
}

async function runSelected() {
  if (!(await validateSelected())) return

  error.value = ''
  runResults.value = []
  // 上一轮的补录草稿要清掉：换了批次之后，那些渠道/分组可能根本不在这一批里，
  // 留着会让「保存并继续执行」提交一批与当前结果无关的数值。
  // 折扣草稿同样清掉——它比倍率更危险：提交上去就直接改了这个客户的报价。
  blockedRatioDraft.value = {}
  blockedDiscountDraft.value = {}
  blockedError.value = ''
  blockedMsg.value = ''
  const ids = [...selectedTasks.value]
  running.value = true
  try {
    const resp = await fetch('/api/run-bill-tasks', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ taskIds: ids }),
    })
    const data = await resp.json()
    if (!resp.ok) {
      if (resp.status === 401) emit('unauthorized')
      error.value = data.error || `执行失败（${resp.status}）`
      return
    }
    runResults.value = data.results || []
    message.value = data.failCount > 0
      ? `执行完成：成功 ${data.okCount} 条，失败 ${data.failCount} 条`
      : `执行完成：${data.okCount} 条全部成功`
    await loadTasks()
  } catch (err) {
    error.value = '执行失败：' + err.message
  } finally {
    running.value = false
  }
}

// 单条执行：复用批量接口，只传一个 id。少一套代码路径，行为必然一致。
async function runOne(t) {
  selectedTasks.value = [t.id]
  await runSelected()
}

async function deleteSelected() {
  const ids = [...selectedTasks.value]
  if (ids.length === 0) return
  if (!window.confirm(`确定删除选中的 ${ids.length} 条计划？\n（只删统计记录，不影响已下载的文件）`)) return
  error.value = ''
  message.value = ''
  const failed = []
  let okCount = 0
  try {
    for (const id of ids) {
      const resp = await fetch('/api/delete-bill-task', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ id }),
      })
      if (!resp.ok) {
        const data = await resp.json().catch(() => ({}))
        if (resp.status === 401) emit('unauthorized')
        failed.push(`${id}：${data.error || resp.status}`)
        continue
      }
      okCount++
    }
  } catch (err) {
    failed.push('请求失败：' + err.message)
  }
  if (failed.length > 0) {
    error.value = `删除完成 ${okCount} 条，失败 ${failed.length} 条：${failed.join('；')}`
  } else {
    message.value = `已删除 ${okCount} 条计划`
  }
  selectedTasks.value = []
  await loadTasks()
}

// ---- 被预检拦下后就地补录上游倍率 ----

// 待补录的倍率草稿，键是渠道 ID。按任务分不开：同一批执行里两条任务可能都缺
// 同一个渠道的倍率，那个渠道只需要填一次，所以草稿是全局的一份而不是按任务一份。
const blockedRatioDraft = ref({})
const savingBlockedRatios = ref(false)
const blockedError = ref('')
const blockedMsg = ref('')

// 待补录的线下折扣草稿，键是分组名。
// 与渠道倍率草稿一样是全局一份：同一批里两条任务可能共用同一个分组，
// 那个分组只需要填一次。
const blockedDiscountDraft = ref({})

// blockedDiscountTask 取被拦任务的客户 ID 与分组，供保存折扣用。
// 折扣是按「客户 + 分组」存的，所以必须带上客户 ID——不能只提交分组名。
function blockedDiscountCustomers() {
  const out = []
  for (const r of runBlocked.value) {
    const groups = r.channelCheck?.missingDiscountGroups || []
    if (groups.length === 0) continue
    const customerId = r.customerId || r.task?.customerId
    if (!customerId) continue
    out.push({ taskId: r.taskId, customerId, groups })
  }
  return out
}

// saveBlockedDiscounts 保存线下折扣。成功后返回 true。
//
// 走 /api/save-group-discounts 这个既有接口：它内部复用出账同一套 ParseDiscountText，
// 前端不自己解析——两边解析规则一旦有差，会出现「页面显示 0.6、账单按 6 算」。
async function saveBlockedDiscounts() {
  for (const c of blockedDiscountCustomers()) {
    const items = []
    for (const g of c.groups) {
      const raw = String(blockedDiscountDraft.value[g] ?? '').trim()
      if (raw === '') continue
      items.push({ groupKey: g, discount: raw, note: '' })
    }
    if (items.length === 0) continue
    try {
      const resp = await fetch('/api/save-group-discounts', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ customerId: c.customerId, items }),
      })
      const data = await resp.json()
      if (!resp.ok) {
        if (resp.status === 401) emit('unauthorized')
        blockedError.value = data.error || `保存折扣失败（${resp.status}）`
        return false
      }
    } catch (err) {
      blockedError.value = '保存折扣失败：' + err.message
      return false
    }
  }
  return true
}

// blockedHasDiscountWork 被拦任务里是否有待填的折扣分组。
const blockedHasDiscountWork = computed(() => blockedDiscountGroups.value.length > 0)

// blockedRatioItems 把草稿整理成接口要的形态，顺手校验。
// 空串表示「这次不填」，跳过而不是报错——用户可能只想先补其中几个。
function blockedRatioItems() {
  const items = []
  for (const r of runBlocked.value) {
    for (const ch of r.channelCheck?.missing || []) {
      const raw = String(blockedRatioDraft.value[ch.channelId] ?? '').trim()
      if (raw === '') continue
      const num = Number(raw)
      if (!Number.isFinite(num) || num < 0) {
        return { error: `渠道 ${ch.channelId} 的倍率必须是非负数字` }
      }
      items.push({ channelId: ch.channelId, upstreamRatio: num, note: '' })
    }
  }
  return { items }
}

// blockedDiscountGroups 全部被拦任务里「缺线下折扣」的分组。
//
// 与「缺渠道倍率」是两件不同的事：这个要填的是**折扣**（客户侧，
// 决定收多少钱），那个填的是**上游倍率**（成本侧，决定花多少钱）。
// 两者都要「就地补录后继续」，但填进的是不同的表，所以分开渲染。
const blockedDiscountGroups = computed(() => {
  const out = []
  for (const r of runBlocked.value) {
    const groups = r.channelCheck?.missingDiscountGroups || []
    if (groups.length === 0) continue
    out.push({ taskId: r.taskId, taskName: r.taskName || r.task?.name || `任务 ${r.taskId}`, groups })
  }
  return out
})

// blockedMissingChannels 被拦任务里待补录的渠道（不含纯折扣拦截那类）。
const blockedMissingChannels = computed(() =>
  runBlocked.value.filter((r) => (r.channelCheck?.missing || []).length > 0)
)

// blockedGrouped 把待补录的渠道按**分组**归拢，供界面分节展示。
//
// 分组来自本次日志实际观测到的 group 列（后端算好随检查结果一起给），
// 不是渠道表里那个「能服务哪些分组」的候选集合——后者可能整组对不上。
// 一个渠道可能出现在多个分组下，所以它会在几节里都出现：
// 这不是重复，而是事实——那个渠道确实同时服务这几个分组。
const blockedGrouped = computed(() => {
  const out = []
  for (const r of runBlocked.value) {
    const missing = r.channelCheck?.missing || []
    const byGroup = {}
    for (const ch of missing) {
      const groups = ch.groups && ch.groups.length > 0 ? ch.groups : ['（日志未记录分组）']
      for (const g of groups) {
        if (!byGroup[g]) byGroup[g] = []
        byGroup[g].push(ch)
      }
    }
    const sections = Object.keys(byGroup)
      .sort()
      .map((g) => ({
        group: g,
        // 组内按「影响行数」降序：渠道多的时候，先补影响面最大的那个，
        // 而不是按渠道号大小排——用户关心的是补哪个划算。
        channels: byGroup[g].slice().sort((a, b) => (b.rowCount || 0) - (a.rowCount || 0)),
      }))
    out.push({
      taskId: r.taskId,
      taskName: r.taskName || r.task?.name || `任务 ${r.taskId}`,
      channelCheck: r.channelCheck,
      sections,
    })
  }
  return out
})

// blockedUnknownCount 全部被拦任务里「渠道清单里查不到」的渠道数。
//
// 这些渠道**照样能填倍率**（倍率表以 channel_id 为主键，与清单无关），
// 只是不在清单里、拿不到渠道名。从前这里把它们当成"补不了"而不给输入框，
// 结果是清单快照没拉到的渠道永远算不出成本——用户看得到账单上的缺失，
// 界面上却没有地方可填。
const blockedUnknownCount = computed(() => {
  const set = new Set()
  for (const r of runBlocked.value) {
    for (const m of r.channelCheck?.missing || []) {
      if (m.known === false) set.add(m.channelId)
    }
  }
  return set.size
})

// blockedMissGroupRatio 全部被拦任务里 group_ratio 缺失的行数。
const blockedMissGroupRatio = computed(() =>
  runBlocked.value.reduce((sum, r) => sum + (r.channelCheck?.missingGroupRatioRows || 0), 0)
)

// 算不出成本的行数按原因分类（键见后端 CostSkipReason）。
// 分开是必要的：缺倍率能补、缺渠道号只能查日志，混成一句「392 行未计入成本」
// 用户不知道该干什么——那正是这次修的问题。
// 键与后端 billing.CostSkipReason 一一对应。
// 注意没有「渠道不在清单里」这一类：在不在清单里不影响能否补录，
// 只影响页面上显示不显示得出渠道名（见 below 的 known 标记）。
const SKIP_REASON_LABELS = {
  no_upstream_ratio: '渠道未维护上游倍率',
  no_channel: '日志里取不到渠道号',
  multi_channel: '一行经多个渠道无法分摊',
  no_group_ratio: '缺分组倍率（group_ratio）',
}

// blockedSkipReasons 把各任务的原因表合并成一个列表，按固定顺序展示。
function skipReasonText(rowReasons) {
  const order = [
    'no_upstream_ratio',
    'no_channel',
    'multi_channel',
    'no_group_ratio',
  ]
  const parts = []
  for (const k of order) {
    const n = rowReasons?.[k]
    if (n > 0) parts.push(`${n} 行${SKIP_REASON_LABELS[k] || k}`)
  }
  return parts.join('、')
}

// blockedUnknownCount 全部被拦任务里取不到渠道号的任务数（noChannelInfo）。
const blockedNoChannelInfo = computed(() =>
  runBlocked.value.filter((r) => r.channelCheck?.noChannelInfo).length
)

// saveBlockedRatios 保存草稿里填好的倍率。保存成功返回 true。
async function saveBlockedRatios() {
  blockedError.value = ''
  blockedMsg.value = ''
  const { items, error } = blockedRatioItems()
  if (error) {
    blockedError.value = error
    return false
  }
  if (items.length === 0) {
    blockedError.value = '请先填写至少一个渠道的倍率'
    return false
  }

  savingBlockedRatios.value = true
  try {
    const resp = await fetch('/api/channel-ratios', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ items }),
    })
    const data = await resp.json()
    if (!resp.ok) {
      if (resp.status === 401) emit('unauthorized')
      blockedError.value = data.error || `保存失败（${resp.status}）`
      return false
    }
    blockedMsg.value = `已保存 ${data.saved} 个渠道的倍率`
    blockedRatioDraft.value = {}
    return true
  } catch (err) {
    blockedError.value = '保存失败：' + err.message
    return false
  } finally {
    savingBlockedRatios.value = false
  }
}

// continueAfterFix 保存倍率后重跑被拦下的那几条计划。
//
// **会重新导出日志**：渠道集合要导出后才知道，所以检查必然发生在导出之后，
// 「继续」只能是重跑整条链路。这是刻意的取舍——换来的是不需要一套跨请求的
// 中间态机制。界面上必须说清楚，别让用户以为点了继续就完全不重来。
async function continueAfterFix() {
  blockedError.value = ''
  blockedMsg.value = ''

  // 两类补录都要处理，且**顺序无妨**（写的是两张不同的表）。
  // 各自只提交填了值的那部分；一个都没填时要提示，而不是直接重跑——
  // 重跑一定还会被同一批缺口拦下，白导一次日志。
  const discountWork = blockedHasDiscountWork.value
  const ratioWork = blockedMissingChannels.value.length > 0

  if (discountWork) {
    if (!(await saveBlockedDiscounts())) return
  }
  if (ratioWork) {
    if (!(await saveBlockedRatios())) return
  }
  if (!discountWork && !ratioWork) {
    blockedError.value = '没有需要保存的补录内容'
    return
  }

  const ids = runBlocked.value.map((r) => r.taskId)
  if (ids.length === 0) return

  error.value = ''
  running.value = true
  try {
    const resp = await fetch('/api/run-bill-tasks', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ taskIds: ids }),
    })
    const data = await resp.json()
    if (!resp.ok) {
      if (resp.status === 401) emit('unauthorized')
      error.value = data.error || `继续执行失败（${resp.status}）`
      return
    }
    // 只替换这几条的结果，别动其它条：用户可能同时跑了别的计划，
    // 整份覆盖会把它们的结果连下载链接一起抹掉。
    const byId = new Map((data.results || []).map((r) => [r.taskId, r]))
    const merged = runResults.value.map((r) => (byId.has(r.taskId) ? byId.get(r.taskId) : r))
    // 不在原结果里的（理论上不会出现）补在后面，免得静默丢掉。
    for (const [id, r] of byId) {
      if (!runResults.value.some((x) => x.taskId === id)) merged.push(r)
    }
    runResults.value = merged
    await loadTasks()
  } catch (err) {
    error.value = '继续执行失败：' + err.message
  } finally {
    running.value = false
  }
}

// summaryText 取该条结果的可复制文字。
//
// 两种模板给的摘要不同：标准模板是成本利润摘要（要成本表算出来才有值），
// 简易模板是账单摘要（金额来自站点实收额度）。空串表示这次没有可复制的内容——
// 比如标准模板但成本表被渠道倍率拦下了，此时不显示复制框，而不是显示一个空框。
function summaryText(r) {
  return r.billSummary || r.costSummary || ''
}

// 复制一条成功结果的摘要。
//
// 状态按任务 ID 记：批量执行后页面上会有好几条摘要，用单个全局状态的话
// 点其中一个，所有按钮会一起变成「已复制 ✓」——用户根本不知道复制了哪条。
async function copySummary(r) {
  const id = r.taskId
  const state = await copyText(summaryText(r))
  copyStates.value = { ...copyStates.value, [id]: state }
  if (state === 'fail') {
    // 连 execCommand 都不行（极老的浏览器）：把文字选中，让用户自己按 Ctrl+C。
    selectElementText(summaryEls.get(id))
  }
  setTimeout(() => {
    const next = { ...copyStates.value }
    delete next[id]
    copyStates.value = next
  }, 2000)
}

// summaryEls 任务 ID → 摘要 <pre> 元素。
//
// 不能再用 ref="copyRef"：它在 v-for 里，Vue 3 会把循环内的 ref 收集成**数组**，
// 拿它当单个元素用（selectNodeContents）会直接抛异常——这正是之前「点了没反应」的原因。
const summaryEls = new Map()
function setSummaryEl(el, id) {
  if (el) summaryEls.set(id, el)
  else summaryEls.delete(id)
}

// ---- 展示辅助 ----

function accountCount(c) {
  return String(c.usernames || '')
    .split(/[\n\r,，;；\t ]+/)
    .filter(Boolean).length
}

function periodLabel(t) {
  if (!t.periodYear || !t.periodMonth) return '-'
  return `${t.periodYear}-${String(t.periodMonth).padStart(2, '0')}`
}

// rangeLabel 显示计划时段。用后端给的北京时间墙上时间，只做字符串裁剪，
// 不 new Date()——那会按浏览器本地时区解释，服务端在 UTC 时显示会偏 8 小时。
//
// 起止都是 00:00:00 / 23:59:59 时只显示日期：整天区间写成「09-01 00:00:00 ~ 09-07
// 23:59:59」噪音太大，而这类区间恰恰是最常见的。
function rangeLabel(t) {
  const s = t.startAt || ''
  const e = t.endAt || ''
  if (!s || !e) return '未设置时段'
  const short = (v) => {
    const date = v.slice(0, 10)
    const time = v.slice(11, 19)
    if (time === '00:00:00' || time === '23:59:59') return date
    return `${date} ${time}`
  }
  return `${short(s)} ~ ${short(e)}`
}

// 三态：未执行 / 已执行未核算成本 / 已执行有成本。
// 前两者金额列都是空，含义完全不同，必须分开显示。
function taskState(t) {
  if (!t.lastRunAt) return 'unrun'
  if (t.costCny === null || t.costCny === undefined) return 'nocost'
  return 'done'
}

function qualityNote(t) {
  if (taskState(t) !== 'done') return ''
  return t.costComplete ? '' : '成本不全'
}

function fmtMoney(v) {
  if (v === undefined || v === null) return '-'
  return Number(v).toLocaleString('zh-CN', { minimumFractionDigits: 4, maximumFractionDigits: 4 })
}

function fmtNum(v) {
  if (v === undefined || v === null) return '-'
  return Number(v).toLocaleString('zh-CN', { maximumFractionDigits: 2 })
}

function fmtTime(v) {
  if (!v) return '-'
  return new Date(v).toLocaleString('zh-CN')
}

onMounted(loadAll)

defineExpose({ loadAll })
</script>

<template>
  <div class="card">
    <h2>账单导出任务</h2>
    <p class="hint">
      先建好计划（客户 + 时段 + 导出内容），再勾选执行。一个客户可以建多条计划——
      月度对账一条、按周导的每周一条。
    </p>
    <p class="hint">
      执行时会自动按客户账号从业务库导出该时段的日志，产出账单 / 脱敏日志 / 成本利润表，
      并把结算额、成本、利润记入下方汇总。生成的文件只保留 {{ jobRetentionHours }} 小时，
      过期后点「执行」重跑即可，统计记录不受影响。
    </p>

    <!-- 默认出账参数 -->
    <div class="section-head">
      <strong>默认出账参数</strong>
      <button type="button" class="btn-link" @click="showSettings = !showSettings">
        {{ showSettings ? '收起' : '展开修改' }}
      </button>
      <span class="hint inline" v-if="settings">
        单价来源 {{ settings.priceSource }} ｜ 汇率 {{ settings.exchangeRate }} ｜
        折扣 {{ settings.discount === null || settings.discount === undefined ? '自动反推' : settings.discount }}
      </span>
    </div>

    <div v-if="showSettings" class="settings-box">
      <div class="form-grid">
        <label>
          <span>单价来源</span>
          <select v-model="settingsDraft.priceSource">
            <option value="db">数据库实时价格</option>
            <option value="price_table">报价表（xlsx）</option>
            <option value="official">内置官方价</option>
          </select>
        </label>
        <label>
          <span>汇率（1 美金 = ? 人民币）</span>
          <input v-model="settingsDraft.exchangeRate" type="number" step="0.01" min="0" />
        </label>
        <label>
          <span>折扣（留空 = 按分组自动反推）</span>
          <input v-model="settingsDraft.discount" type="number" step="0.001" min="0" placeholder="自动反推" />
        </label>
        <label>
          <span>脱敏日志格式</span>
          <select v-model="settingsDraft.sanitizedFormat">
            <option value="tsv">tsv（纯文本，适合超大日志）</option>
            <option value="csv">csv</option>
            <option value="xlsx">xlsx</option>
          </select>
        </label>
        <label class="full">
          <span>国产/站内定价模型标识（一行一个，命中则不参与折扣反推）</span>
          <textarea v-model="settingsDraft.domesticMarkers" rows="2" placeholder="例如：&#10;国产模型&#10;doubao"></textarea>
        </label>
      </div>
      <button type="button" class="btn-primary" @click="saveSettings" :disabled="savingSettings">
        {{ savingSettings ? '保存中…' : '保存默认参数' }}
      </button>
      <p class="hint">单价来源选「数据库实时价格」前，需先在「生成账单」页点过「拉取最新数据库价格」。</p>
    </div>

    <!-- 新建/编辑计划 -->
    <div class="section-head">
      <strong>{{ isEditingPlan ? '编辑计划' : '新建计划' }}</strong>
      <button type="button" class="btn-link" @click="showPlanForm = !showPlanForm">
        {{ showPlanForm ? '收起' : '展开' }}
      </button>
      <button type="button" class="btn-link" v-if="isEditingPlan" @click="resetPlanForm(); showPlanForm = false">
        取消编辑
      </button>
    </div>

    <div v-if="showPlanForm" class="plan-box">
      <div class="form-grid">
        <label>
          <span>客户</span>
          <select v-model.number="planForm.customerId">
            <option :value="0" disabled>请选择客户</option>
            <option v-for="c in customers" :key="c.id" :value="c.id">
              {{ c.name }}（{{ accountCount(c) }} 个账号）
            </option>
          </select>
        </label>
        <label>
          <span>计划名称（可选）</span>
          <input v-model="planForm.name" type="text" placeholder="例如：9月第1周" />
        </label>
        <label>
          <span>开始时间</span>
          <input v-model="planForm.startAt" type="datetime-local" step="1" />
        </label>
        <label>
          <span>结束时间</span>
          <input v-model="planForm.endAt" type="datetime-local" step="1" />
        </label>
      </div>

      <div class="path-row">
        <span class="hint inline">快捷：</span>
        <button type="button" class="btn-browse" @click="applyPreset('lastMonth')">上月整月</button>
        <button type="button" class="btn-browse" @click="applyPreset('lastWeek')">上周（周一~周日）</button>
        <button type="button" class="btn-browse" @click="applyPreset('monthToDate')">本月至今</button>
        <button type="button" class="btn-browse" @click="applyPreset('today')">今天</button>
      </div>

      <div class="field">
        <label>账单模板</label>
        <select v-model="planForm.billTemplate">
          <option value="">标准明细账单（按 token 明细出账，含单价、折扣与结算额）</option>
          <option value="simple">简易汇总账单（按分组+模型汇总，金额取日志额度折算）</option>
        </select>
        <span class="hint" v-if="planForm.billTemplate === 'simple'">
          简易账单不做定价：金额 = 日志额度 ÷ 500000。不生成成本利润表，
          脱敏日志也是同一张汇总表。
        </span>
      </div>

      <div class="checkboxes">
        <label><input v-model="planForm.generateSanitized" type="checkbox" /> 生成脱敏日志</label>
        <!-- 成本核算在前、生成成本利润表在后，并把后者作为它的子项：
             用户的决策顺序就是「要不要核算成本」→「要不要那张表」。
             两者排序反过来会让人以为先勾的是表，而那张表只是核算的产物之一。 -->
        <label>
          <input v-model="planForm.checkCost" type="checkbox" /> 成本核算（执行前检查上游倍率）
        </label>
        <label v-if="planForm.billTemplate !== 'simple'" :class="{ muted: !planForm.checkCost }">
          <input v-model="planForm.generateCost" type="checkbox" :disabled="!planForm.checkCost" />
          生成成本利润表
        </label>
        <!-- 折扣与成本是两个方向：折扣决定**收客户多少钱**（客户侧），
             上游倍率决定**我们花多少钱**（成本侧）。所以并排放在同一层，
             而不是谁套谁——两者可以任意组合。 -->
        <label>
          <input v-model="planForm.useManualDiscount" type="checkbox" /> 使用自定义折扣（线下谈定）
        </label>
      </div>
      <p class="hint" v-if="planForm.useManualDiscount">
        出账会用该客户在「客户折扣」页按分组维护的折扣，覆盖按倍率自动反推的值。
        勾选后执行前会检查：这份日志里的分组若还有没维护线下折扣的，会先拦下，
        可以在本页就地补录后继续。<strong>继续执行会重新导一次日志。</strong>
      </p>
      <p class="hint" v-if="!planForm.checkCost">
        未开成本核算时不做上游倍率检查，账单里也不会出现成本与利润。
      </p>
      <p class="hint" v-else-if="planForm.billTemplate === 'simple'">
        执行前会检查本时段日志用到的渠道是否都维护了上游倍率；缺了会先拦下，
        可以在本页就地补录后继续。<strong>注意：继续执行会重新导一次日志</strong>
        （只读查询，不影响业务库），因为渠道集合要导出后才知道。
      </p>
      <p class="hint" v-else>
        执行前会检查本时段日志用到的渠道是否都维护了上游倍率；缺了会先拦下，
        可以在本页就地补录后继续。
      </p>

      <div class="path-row">
        <button type="button" class="btn-primary" @click="savePlan" :disabled="savingPlan">
          {{ savingPlan ? '保存中…' : isEditingPlan ? '保存修改' : '新增计划' }}
        </button>
        <span class="hint inline">时段可留空先建计划，之后再补。单次跨度上限 92 天。</span>
      </div>
      <p class="hint">
        时间按<strong>北京时间</strong>解释，起止两端都包含在内（结束那一刻的日志不会被漏掉）。
        归属账期按<strong>开始时间</strong>所在月计算，跨月计划（如 8/28~9/3）整个计入开始月。
      </p>
    </div>

    <p class="error" v-if="error">{{ error }}</p>
    <span class="hint" v-if="message">{{ message }}</span>

    <!-- 校验 / 执行结果 -->
    <div v-if="validationResults.length > 0" class="result-box">
      <strong>执行前校验</strong>
      <ul class="result-list">
        <li v-for="v in validationResults" :key="v.taskId" :class="{ bad: v.error }">
          <span class="name">{{ v.taskName }}</span>
          <span v-if="v.error" class="reason">{{ v.error }}</span>
          <span v-else class="ok">可执行</span>
        </li>
      </ul>
    </div>

    <div v-if="runFailures.length > 0" class="result-box bad">
      <strong>执行失败 {{ runFailures.length }} 条</strong>
      <ul class="result-list">
        <li v-for="r in runFailures" :key="r.taskId" class="bad">
          <span class="name">{{ r.taskName || ('任务 ' + r.taskId) }}</span>
          <span class="reason">{{ r.error }}</span>
        </li>
      </ul>
      <p class="hint">失败的计划保持原样，不会覆盖上次的结果。修正后重新勾选执行即可。</p>
    </div>

    <!-- 被成本核算预检拦下：不是失败，而是等待用户补录上游倍率。
         放在「执行失败」之后、「执行成功」之前：它比失败轻微（补一下就能跑），
         但比成功要紧（还没出账）。 -->
    <div v-if="runBlocked.length > 0" class="result-box warn-box">
      <strong>需要先补录（{{ runBlocked.length }} 条）</strong>
      <p class="hint">
        下面这些计划**没有出账**。原因有两类，缺什么就填什么，填完点「保存并继续执行」：
      </p>
      <ul class="result-list">
        <li v-if="blockedHasDiscountWork">
          <span class="name">线下折扣没维护全</span>
          <span class="reason">勾了「使用自定义折扣」，但日志里的分组还有没填的。
            不填会变成一半按线下折扣、一半按反推，客户核对时会问为什么不一致。</span>
        </li>
        <li v-if="blockedMissingChannels.length > 0">
          <span class="name">上游倍率没维护全</span>
          <span class="reason">成本算不出来（不影响账单金额，只影响成本与利润）。</span>
        </li>
      </ul>

      <p class="error" v-if="blockedNoChannelInfo > 0">
        其中有 {{ blockedNoChannelInfo }} 条任务的日志里取不到渠道号（既没有 channel_id 列，
        other 里也没有 use_channel），这一份做不了检查。请用「导出日志明细」重新导出带
        channel_id 的日志，或到「生成账单」页手动处理。
      </p>
      <!-- 逐条列出「还差什么、各多少行」。只说总数会让人去翻日志，
           说清原因才知道是去补倍率还是去查导出方式。 -->
      <p class="hint" v-for="b in blockedGrouped" :key="'why-' + b.taskId"
         v-if="skipReasonText(b.channelCheck?.uncostableRows)">
        {{ b.taskName }}：{{ skipReasonText(b.channelCheck?.uncostableRows) }}
      </p>
      <p class="hint" v-if="blockedUnknownCount > 0">
        有 {{ blockedUnknownCount }} 个渠道不在本地渠道清单里（多半是新加的，还没点过「拉取渠道清单」）。
        它们**仍然可以补录倍率**——倍率表只认渠道号，不依赖清单，直接填就行。
      </p>

      <div v-for="b in blockedGrouped" :key="'blk-' + b.taskId" class="blocked-task">
        <strong>{{ b.taskName }}</strong>
        <p class="hint" v-if="b.channelCheck?.noChannelInfo">
          这份日志没有可用的渠道号，没法检查。请重新导出日志。
        </p>
        <div v-for="sec in b.sections" :key="b.taskId + '-' + sec.group" class="blocked-group">
          <div class="blocked-group-head">分组 {{ sec.group }}</div>
          <table>
            <thead>
              <tr>
                <th>渠道 ID</th>
                <th>渠道名称</th>
                <th>上游倍率</th>
                <th>状态</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="ch in sec.channels" :key="b.taskId + '-' + ch.channelId">
                <td>{{ ch.channelId }}</td>
                <td>
                  {{ ch.name }}
                  <span v-if="ch.known === false" class="tag warn">不在渠道清单里</span>
                </td>
                <td>
                  <input
                    v-model="blockedRatioDraft[ch.channelId]"
                    type="number"
                    step="0.01"
                    min="0"
                    placeholder="未维护"
                    class="ratio-input"
                  />
                </td>
                <td>
                  <!-- 同一个渠道可能在多个分组下重复出现，所以状态读的是**草稿**：
                       在一节里填了，另一节也会立刻显示已填，不会让人以为那边还没填。 -->
                  <span v-if="String(blockedRatioDraft[ch.channelId] ?? '').trim() !== ''">待保存</span>
                  <span v-else>未维护</span>
                  <!-- 影响行数：用户据此决定先补哪一个。 -->
                  <span v-if="ch.rowCount" class="hint inline">（影响 {{ ch.rowCount }} 行）</span>
                </td>
              </tr>
            </tbody>
          </table>
        </div>
      </div>

      <!-- 线下折扣：按「客户 + 分组」填。分组名取自本次日志实际出现的 group 列原值，
           与「客户折扣」页维护的键完全一致，所以两边填的是同一份数据。 -->
      <div v-for="b in blockedDiscountGroups" :key="'dsc-' + b.taskId" class="blocked-task">
        <strong>{{ b.taskName }} — 线下折扣</strong>
        <p class="hint">
          这些分组还没维护线下折扣。可填 <code>6折</code>、<code>60%</code> 或 <code>0.6</code>。
          维护后也会出现在「客户折扣」页，可随时二次修改。
        </p>
        <div class="blocked-group">
          <div class="blocked-group-head">分组</div>
          <table>
            <thead>
              <tr>
                <th>分组</th>
                <th>折扣</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="g in b.groups" :key="b.taskId + '-d-' + g">
                <td class="left">{{ g }}</td>
                <td>
                  <input
                    v-model="blockedDiscountDraft[g]"
                    type="text"
                    placeholder="如 6折 / 0.6"
                    class="discount-input"
                  />
                </td>
              </tr>
            </tbody>
          </table>
        </div>
      </div>

      <div class="path-row">
        <button type="button" class="btn-primary" @click="continueAfterFix" :disabled="savingBlockedRatios || running">
          {{ savingBlockedRatios ? '保存中…' : running ? '执行中…' : '保存并继续执行' }}
        </button>
        <span class="hint inline">
          继续执行会<strong>重新导一次日志</strong>（只读查询，不影响业务库）——
          渠道集合要导出后才知道，所以检查必然在导出之后。
        </span>
      </div>
      <p class="error" v-if="blockedError">{{ blockedError }}</p>
      <span class="hint" v-if="blockedMsg">{{ blockedMsg }}</span>
    </div>

    <div v-if="runSuccesses.length > 0" class="result-box ok">
      <strong>执行成功 {{ runSuccesses.length }} 条</strong>
      <ul class="result-list">
        <li v-for="r in runSuccesses" :key="r.taskId">
          <span class="name">{{ r.taskName || r.task?.name || ('任务 ' + r.taskId) }}</span>
          <a class="btn small" :href="r.billUrl">下载账单</a>
          <a class="btn small" v-if="r.sanitizedUrl" :href="r.sanitizedUrl">脱敏日志</a>
          <a class="btn small" v-if="r.costUrl" :href="r.costUrl">成本利润表</a>
          <span v-if="r.costBlocked" class="tag warn">成本表被拦下（渠道倍率未维护）</span>
          <span v-if="r.summaryPersisted === false" class="tag warn">统计未落库</span>
        </li>
      </ul>
      <p class="hint">
        文件只保留 {{ jobRetentionHours }} 小时，请及时下载。
      </p>
      <div v-for="r in runSuccesses" :key="'sum-' + r.taskId">
        <!-- 两种摘要互斥：标准模板给成本利润摘要（要成本表算出来才有），
             简易模板给账单摘要（金额是站点实收额度）。共用同一段 UI 与复制逻辑，
             区别只在标题与取哪一段文字。 -->
        <div v-if="summaryText(r)" class="cost-summary">
          <div class="cost-summary-head">
            <strong>{{ r.taskName || ('任务 ' + r.taskId) }} {{ r.billSummary ? '账单' : '成本利润' }}摘要</strong>
            <button type="button" class="btn-browse" @click="copySummary(r)">
              {{ copyStates[r.taskId] === 'ok' ? '已复制 ✓' : copyStates[r.taskId] === 'fail' ? '复制失败，请手动选中' : '复制' }}
            </button>
          </div>
          <pre :ref="(el) => setSummaryEl(el, r.taskId)" class="cost-summary-text">{{ summaryText(r) }}</pre>
        </div>
      </div>
    </div>
  </div>

  <!-- 计划列表 -->
  <div class="card">
    <h2>计划列表</h2>
    <div class="path-row">
      <button type="button" class="btn-browse" v-if="selectedCount > 0" @click="validateSelected" :disabled="validating">
        {{ validating ? '校验中…' : `仅校验选中（${selectedCount}）` }}
      </button>
      <button type="button" class="btn-primary" v-if="selectedCount > 0" @click="runSelected" :disabled="running">
        {{ running ? '执行中…' : `执行选中（${selectedCount}）` }}
      </button>
      <button type="button" class="btn-browse" v-if="selectedCount > 0" @click="deleteSelected">
        {{ `删除选中（${selectedCount}）` }}
      </button>
      <span class="hint inline" v-if="running">
        正在逐条导出日志并出账，日志量大时可能需要几分钟，请不要关闭页面。
      </span>
    </div>

    <table v-if="tasks.length > 0">
      <thead>
        <tr>
          <th class="pick">
            <input type="checkbox" :checked="allSelected" @change="toggleAll($event.target.checked)" />
          </th>
          <th>计划</th>
          <th>客户</th>
          <th>模板</th>
          <th>成本核算</th>
          <th>时段</th>
          <th>账期</th>
          <th>状态</th>
          <th>结算额(¥)</th>
          <th>成本(¥)</th>
          <th>利润(¥)</th>
          <th>操作</th>
        </tr>
      </thead>
      <tbody>
        <tr v-for="t in tasks" :key="t.id" :class="{ 'row-unrun': taskState(t) === 'unrun' }">
          <td class="pick">
            <input type="checkbox" v-model="selectedTasks" :value="t.id" />
          </td>
          <td class="name">{{ t.name || '（未命名）' }}</td>
          <td>{{ t.customerName }}</td>
          <td>{{ t.billTemplate === 'simple' ? '简易汇总' : '标准明细' }}</td>
          <td>
            <span v-if="t.checkCost" class="ok">开</span>
            <span v-else class="hint inline">关</span>
          </td>
          <td>{{ rangeLabel(t) }}</td>
          <td>{{ periodLabel(t) }}</td>
          <td>
            <span v-if="taskState(t) === 'unrun'" class="tag">未执行</span>
            <span v-else-if="taskState(t) === 'nocost'" class="tag warn">未核算成本</span>
            <span v-else class="ok">已执行</span>
            <span v-if="qualityNote(t)" class="tag warn">{{ qualityNote(t) }}</span>
          </td>
          <td>{{ fmtMoney(t.settleCny) }}</td>
          <td>{{ fmtMoney(t.costCny) }}</td>
          <td :class="{ negative: t.profitCny !== null && t.profitCny !== undefined && t.profitCny < 0 }">
            {{ fmtMoney(t.profitCny) }}
          </td>
          <td class="ops">
            <button type="button" class="btn-link" @click="runOne(t)" :disabled="running">执行</button>
            <button type="button" class="btn-link" @click="startEditPlan(t)">编辑</button>
            <button type="button" class="btn-link danger" @click="deletePlan(t)">删除</button>
          </td>
        </tr>
      </tbody>
    </table>
    <span class="hint" v-else-if="!loading">
      还没有计划，先在上面新建一条。
      <template v-if="!hasCustomers">（需要先到「客户信息」页新增客户并填好业务库账号）</template>
    </span>
    <span class="hint" v-else>正在读取计划…</span>
  </div>

  <!-- 按月汇总 -->
  <div class="card">
    <h2>按月汇总</h2>
    <p class="hint">
      只统计<strong>已执行</strong>的计划，未执行的单独列在「未跑」里。
      利润用「参与成本核算的结算额」减去成本，<strong>不是</strong>用全部账单结算额减成本——
      渠道上游倍率没维护时，那些行既没有成本、也不该计入利润，否则利润会虚高。
    </p>

    <table v-if="summaries.length > 0">
      <thead>
        <tr>
          <th>账期</th>
          <th>账单数</th>
          <th>账单结算额(¥)</th>
          <th>成本(¥)</th>
          <th>利润(¥)</th>
          <th>毛利率</th>
          <th>说明</th>
        </tr>
      </thead>
      <tbody>
        <tr v-for="s in summaries" :key="s.year + '-' + s.month">
          <td>{{ s.year }}-{{ String(s.month).padStart(2, '0') }}</td>
          <td>{{ s.taskCount }}</td>
          <td>{{ fmtMoney(s.settleCny) }}</td>
          <td>{{ fmtMoney(s.costCny) }}</td>
          <td :class="{ negative: s.profitCny < 0 }">{{ fmtMoney(s.profitCny) }}</td>
          <td>{{ fmtNum(s.margin) }}%</td>
          <td class="note">
            <span v-if="s.missingCostCount > 0" class="tag warn">{{ s.missingCostCount }} 条无成本</span>
            <span v-if="s.partialCostCount > 0" class="tag warn">{{ s.partialCostCount }} 条成本不全</span>
            <span v-if="s.unrunCount > 0" class="tag">{{ s.unrunCount }} 条未跑</span>
            <span
              v-if="s.missingCostCount === 0 && s.partialCostCount === 0 && s.unrunCount === 0"
              class="tag ok"
            >成本完整</span>
          </td>
        </tr>
      </tbody>
    </table>
    <span class="hint" v-else-if="!loading">还没有已执行的计划，执行后这里会出现汇总。</span>

    <p class="hint" v-if="hasMissingCost">
      「无成本」表示该计划没有生成成本利润表（未勾选，或渠道倍率没维护被拦）；
      「成本不全」表示部分渠道没维护倍率、成本只覆盖了一部分行。
      这两种情况下的利润<strong>只覆盖有成本的那部分</strong>，不等于整体毛利。
    </p>
    <p class="hint" v-if="hasUnrun">
      标「未跑」的计划还没有执行过，不计入账单数与金额——它们的金额列是空的，
      计进去会出现「有账单但金额为 0」这种对不上的行。
    </p>
  </div>
</template>

<style scoped>
/* 本组件自带样式副本——Vue 的 scoped 样式不会穿透到子组件内部节点。 */

.card {
  background: #fff;
  border: 1px solid #e2e2e2;
  border-radius: 8px;
  padding: 20px;
  margin-bottom: 20px;
}
.card h2 {
  margin-top: 0;
}

.hint {
  font-size: 12px;
  color: #888;
  display: block;
}
.hint.inline {
  display: inline;
  margin-left: 4px;
}

.section-head {
  display: flex;
  align-items: center;
  gap: 10px;
  flex-wrap: wrap;
  margin: 14px 0 8px;
  padding-top: 12px;
  border-top: 1px solid #eee;
}

.settings-box,
.plan-box {
  padding: 12px;
  border: 1px solid #e2e2e2;
  border-radius: 6px;
  background: #fafbfc;
  margin-bottom: 14px;
}
.plan-box {
  border-color: #d6e2f7;
  background: #f7faff;
}

.form-grid {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(220px, 1fr));
  gap: 10px 16px;
  margin: 10px 0;
}
.form-grid label {
  display: flex;
  flex-direction: column;
  gap: 4px;
  font-size: 13px;
  color: #444;
}
.form-grid label.full {
  grid-column: 1 / -1;
}
.form-grid input,
.form-grid select,
.form-grid textarea {
  padding: 5px 7px;
  border: 1px solid #ccc;
  border-radius: 4px;
  font-size: 13px;
  font-family: inherit;
  box-sizing: border-box;
  max-width: 420px;
}
.form-grid textarea {
  max-width: 100%;
  resize: vertical;
}

.checkboxes {
  display: flex;
  gap: 20px;
  flex-wrap: wrap;
  font-size: 13px;
  margin: 4px 0 8px;
}
.checkboxes label {
  display: flex;
  align-items: center;
  gap: 5px;
}
/* 子选项（生成成本利润表）在父开关关闭时淡下去：它还摆在那里，
   但一眼能看出当前不受生效。禁用态本身由 disabled 提供，
   这里只是把「为什么点不动」说得更明显一点。 */
.checkboxes label.muted {
  opacity: 0.5;
}

/* 被预检拦下的区块。用 warn 色而不是 error 红：这不是失败，
   页面配色不该让人以为任务坏了。 */
.warn-box {
  border-color: #f0c36d;
  background: #fffbf0;
}
.blocked-task {
  margin: 10px 0;
  padding: 8px 10px;
  background: #fff;
  border: 1px solid #f0c36d;
  border-radius: 6px;
}
.blocked-group {
  margin: 8px 0 4px;
}
.blocked-group-head {
  font-size: 13px;
  font-weight: 600;
  color: #8a6100;
  margin-bottom: 4px;
}
.ratio-input {
  width: 90px;
  padding: 2px 6px;
  border: 1px solid #ccc;
  border-radius: 4px;
}
.discount-input {
  width: 110px;
  padding: 2px 6px;
  border: 1px solid #ccc;
  border-radius: 4px;
}

.path-row {
  display: flex;
  gap: 10px;
  align-items: center;
  flex-wrap: wrap;
  margin: 8px 0;
}

.btn-browse {
  background: #f0f4ff;
  color: #2c6ef2;
  border: 1px solid #c7d7fb;
  border-radius: 6px;
  padding: 6px 14px;
  cursor: pointer;
  font-size: 13px;
  white-space: nowrap;
}
.btn-browse:disabled {
  opacity: 0.6;
  cursor: not-allowed;
}

.btn-primary {
  background: #2c6ef2;
  color: #fff;
  border: 1px solid #2c6ef2;
  border-radius: 6px;
  padding: 6px 16px;
  cursor: pointer;
  font-size: 13px;
  white-space: nowrap;
}
.btn-primary:disabled {
  opacity: 0.6;
  cursor: not-allowed;
}

.btn-link {
  background: none;
  border: none;
  color: #2c6ef2;
  cursor: pointer;
  font-size: 13px;
  padding: 0 6px;
  text-decoration: underline;
}
.btn-link.danger {
  color: #d92626;
}
.btn-link:disabled {
  opacity: 0.6;
  cursor: not-allowed;
}

.btn {
  display: inline-block;
  padding: 6px 14px;
  background: #fff;
  color: #2c6ef2;
  border: 1px solid #c7d7fb;
  border-radius: 6px;
  text-decoration: none;
  font-size: 13px;
}
.btn.small {
  padding: 2px 10px;
  font-size: 12px;
  margin-left: 6px;
}

.result-box {
  margin-top: 12px;
  padding: 10px 12px;
  border: 1px solid #d6e2f7;
  border-radius: 6px;
  background: #f7faff;
}
.result-box.bad {
  border-color: #f0c36d;
  background: #fffdf5;
}
.result-box.ok {
  border-color: #b7e0c2;
  background: #f4fbf6;
}
.result-list {
  margin: 8px 0 0;
  padding-left: 18px;
  font-size: 13px;
}
.result-list li {
  margin-bottom: 4px;
  line-height: 1.7;
}
.result-list .name {
  font-weight: 600;
  margin-right: 8px;
}
.result-list .reason {
  color: #d92626;
}
.result-list .ok {
  color: #1f7a3d;
}

.cost-summary {
  margin-top: 10px;
  padding: 10px 12px;
  border: 1px solid #d6e2f7;
  border-radius: 6px;
  background: #fff;
}
.cost-summary-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  margin-bottom: 8px;
}
.cost-summary-text {
  margin: 0;
  padding: 8px 10px;
  border: 1px solid #e2e2e2;
  border-radius: 4px;
  background: #fff;
  font-family: inherit;
  font-size: 13px;
  line-height: 1.7;
  white-space: pre-wrap;
  word-break: break-all;
  cursor: text;
  user-select: text;
}

table {
  width: 100%;
  border-collapse: collapse;
  font-size: 13px;
  margin-top: 12px;
}
th,
td {
  border: 1px solid #e2e2e2;
  padding: 6px 8px;
  text-align: right;
  vertical-align: top;
}
th {
  background: #fafbfc;
  font-weight: 600;
}
th.pick,
td.pick {
  width: 34px;
  text-align: center;
}
th:nth-child(2),
th:nth-child(3),
td:nth-child(2),
td:nth-child(3) {
  text-align: left;
}
td.name {
  font-weight: 600;
}
td.note,
td.ops {
  text-align: left;
}
td.ops {
  white-space: nowrap;
}
td.negative {
  color: #d92626;
}
.row-unrun {
  background: #fbfbfc;
  color: #666;
}

.tag {
  display: inline-block;
  margin: 0 4px 2px 0;
  padding: 1px 7px;
  border-radius: 10px;
  font-size: 12px;
  background: #eef4ff;
  color: #2c6ef2;
}
.tag.warn {
  background: #fff4e0;
  color: #b26a00;
}
.tag.ok {
  background: #e8f7ec;
  color: #1f7a3d;
}
.ok {
  color: #1f7a3d;
}

.error {
  color: #d92626;
  margin-top: 8px;
}
</style>
