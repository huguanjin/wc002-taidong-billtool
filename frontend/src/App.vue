<script setup>
import { ref, computed, nextTick, onMounted } from 'vue'
import ChannelRatios from './ChannelRatios.vue'

// 顶层页面切换：出账（默认）与渠道成本倍率维护。
// 用标签页而不是 vue-router：整个应用只有两个页面，装一个路由只为切两块内容不划算。
const activePage = ref('bill')
const channelRatiosRef = ref(null)

const authChecked = ref(false)
const authenticated = ref(false)
const loginForm = ref({ username: '', password: '' })
const loginError = ref('')
const loginLoading = ref(false)

async function checkSession() {
  try {
    const resp = await fetch('/api/session')
    const data = await resp.json()
    authenticated.value = !!data.authenticated
  } catch {
    authenticated.value = false
  } finally {
    authChecked.value = true
  }
}

async function handleLogin() {
  loginError.value = ''
  if (!loginForm.value.username || !loginForm.value.password) {
    loginError.value = '请输入用户名和密码'
    return
  }
  loginLoading.value = true
  try {
    const resp = await fetch('/api/login', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(loginForm.value),
    })
    const data = await resp.json()
    if (!resp.ok) {
      loginError.value = data.error || `登录失败（${resp.status}）`
      return
    }
    authenticated.value = true
    loginForm.value.password = ''
  } catch (err) {
    loginError.value = '登录失败：' + err.message
  } finally {
    loginLoading.value = false
  }
}

async function handleLogout() {
  try {
    await fetch('/api/logout', { method: 'POST' })
  } finally {
    authenticated.value = false
  }
}

onMounted(checkSession)

const fileInput = ref(null)
const selectedFileName = ref('')
const sourceMode = ref('upload')
const serverPath = ref('')

// 日志合并：可一次选多个文件（或服务器上的多个路径）拼成一个日志
const mergeInput = ref(null)
const mergeSelectedNames = ref([])
const mergeServerPaths = ref('')
const mergeForm = ref({ format: 'xlsx', dedupe: false })
const merging = ref(false)
const mergeError = ref('')
const mergeResult = ref(null)

function onMergeFileChange(e) {
  const files = e.target.files ? Array.from(e.target.files) : []
  mergeSelectedNames.value = files.map((f) => f.name)
}

async function handleMerge() {
  mergeError.value = ''
  mergeResult.value = null

  const fd = new FormData()
  if (sourceMode.value === 'upload') {
    const files = mergeInput.value && mergeInput.value.files ? Array.from(mergeInput.value.files) : []
    if (files.length < 2) {
      mergeError.value = '请至少选择两个日志文件'
      return
    }
    for (const f of files) fd.append('file', f)
  } else {
    const paths = mergeServerPaths.value
      .split('\n')
      .map((p) => p.trim())
      .filter((p) => p !== '')
    if (paths.length < 2) {
      mergeError.value = '请填写至少两个服务器文件路径（每行一个）'
      return
    }
    for (const p of paths) fd.append('serverPath', p)
  }
  fd.append('format', mergeForm.value.format)
  fd.append('dedupe', String(mergeForm.value.dedupe))

  merging.value = true
  try {
    const resp = await fetch('/api/merge-logs', { method: 'POST', body: fd })
    const data = await resp.json()
    if (!resp.ok) {
      if (resp.status === 401) authenticated.value = false
      mergeError.value = data.error || `合并失败（${resp.status}）`
      return
    }
    mergeResult.value = data
  } catch (err) {
    mergeError.value = '合并失败：' + err.message
  } finally {
    merging.value = false
  }
}

// 合并结果就在服务器 data 目录里，直接回填成账单的输入路径，省去再上传一次。
function useMergedAsBillInput() {
  if (!mergeResult.value) return
  serverPath.value = mergeResult.value.mergedPath
  sourceMode.value = 'server'
  // 换了输入文件，分组列表跟着刷新。
  loadGroups()
}

const browserOpen = ref(false)
const browserLoading = ref(false)
const browserError = ref('')
const browserRoot = ref('')
const browserPath = ref('')
const browserParent = ref('')
const browserEntries = ref([])
// 合并日志要一次选多个文件，所以浏览器支持多选模式；普通出账仍是单选即关闭。
const browserMode = ref('single')
const browserPicked = ref([])

async function loadBrowse(path) {
  browserLoading.value = true
  browserError.value = ''
  try {
    const resp = await fetch('/api/browse?path=' + encodeURIComponent(path || ''))
    const data = await resp.json()
    if (!resp.ok) {
      if (resp.status === 401) authenticated.value = false
      browserError.value = data.error || `加载失败（${resp.status}）`
      return
    }
    browserRoot.value = data.root
    browserPath.value = data.path
    browserParent.value = data.parent
    browserEntries.value = data.entries || []
  } catch (err) {
    browserError.value = '加载失败：' + err.message
  } finally {
    browserLoading.value = false
  }
}

function openBrowser() {
  browserMode.value = 'single'
  browserPicked.value = []
  browserOpen.value = true
  loadBrowse('')
}

// 合并用：多选服务器上的日志文件，路径以文本形式回填到多行输入框。
function openMergeBrowser() {
  browserMode.value = 'multi'
  browserPicked.value = []
  browserOpen.value = true
  loadBrowse('')
}

function closeBrowser() {
  browserOpen.value = false
}

function isPicked(entry) {
  return browserPicked.value.some((p) => p.path === entry.path)
}

// 多选模式下点文件是勾选/取消勾选，点目录仍是进出目录。
function pickEntry(entry) {
  if (browserMode.value === 'single') {
    pickFile(entry)
    return
  }
  if (entry.isDir) {
    loadBrowse(entry.path)
    return
  }
  const idx = browserPicked.value.findIndex((p) => p.path === entry.path)
  if (idx >= 0) browserPicked.value.splice(idx, 1)
  else browserPicked.value.push(entry)
}

function confirmBrowserPick() {
  if (browserMode.value === 'single') {
    browserOpen.value = false
    return
  }
  browserOpen.value = false
  if (browserPicked.value.length === 0) return
  const picked = browserPicked.value.map((e) => e.path)
  const existing = mergeServerPaths.value
    .split('\n')
    .map((p) => p.trim())
    .filter((p) => p !== '')
  const merged = existing.slice()
  for (const p of picked) {
    if (!merged.includes(p)) merged.push(p)
  }
  mergeServerPaths.value = merged.join('\n')
}

function pickFile(entry) {
  serverPath.value = entry.path
  browserOpen.value = false
  // 这条路不经过 input 的 blur，手动触发一次分组加载。
  loadGroups()
}

const form = ref({
  month: '',
  year: '',
  exchangeRate: 7,
  discount: '',
  priceSource: 'db',
  sanitizedLog: true,
  sanitizedFormat: 'tsv',
  includeBillingParams: false,
  keepLog: false,
  generateCost: false,
})

const loading = ref(false)
const errorMsg = ref('')
const result = ref(null)

const pullingPrices = ref(false)
const pullPricesMsg = ref('')
const pullPricesError = ref('')

const checkingPrices = ref(false)
const checkPricesError = ref('')
const priceCheckDone = ref(false)
const checkedModelCount = ref(0)
const missingModels = ref([])
const exprModels = ref([])
// 日志里去重后的分组标识，选定日志后由 /api/log-groups 带回，用于渲染勾选列表。
const groups = ref([])
const loadingGroups = ref(false)
const groupsError = ref('')
// selectedDomesticGroups 是勾选为「国产/站内定价」的分组名集合。
const selectedDomesticGroups = ref({})
// domesticModelPrefixes 兜住按分组名勾不准的情况：整组里只有部分模型是站内定价时，
// 用模型名前缀排除更精确。分组勾选覆盖大多数场景，这里保持成可选的补充项。
const domesticModelPrefixes = ref('')
const manualPrices = ref({})

async function pullDbPrices() {
  pullingPrices.value = true
  pullPricesMsg.value = ''
  pullPricesError.value = ''
  try {
    const resp = await fetch('/api/pull-db-prices', { method: 'POST' })
    const data = await resp.json()
    if (!resp.ok) {
      if (resp.status === 401) authenticated.value = false
      pullPricesError.value = data.error || `拉取失败（${resp.status}）`
      return
    }
    const fetchedAt = new Date(data.fetchedAt).toLocaleString('zh-CN')
    pullPricesMsg.value = `已拉取 ${data.modelCount} 个模型价格，时间：${fetchedAt}`
  } catch (err) {
    pullPricesError.value = '请求失败：' + err.message
  } finally {
    pullingPrices.value = false
  }
}

const userDiscountForm = ref({ username: '', userId: '' })
const pullingUserDiscount = ref(false)
const userDiscountError = ref('')
const userDiscountCurrent = ref(null)
const userDiscountHistory = ref([])

const derivedFromLabels = {
  group_group_ratio: '专属倍率',
  group_ratio: '通用倍率',
  fallback_default: '兜底默认（未配置，倍率 1）',
}
function derivedFromLabel(key) {
  return derivedFromLabels[key] || key
}

async function pullUserDiscount() {
  userDiscountError.value = ''
  const username = userDiscountForm.value.username.trim()
  const userId = userDiscountForm.value.userId
  if (!username && userId === '') {
    userDiscountError.value = '请填写用户名或用户 ID'
    return
  }

  pullingUserDiscount.value = true
  try {
    const resp = await fetch('/api/pull-user-discount', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        username,
        userId: userId === '' ? null : Number(userId),
      }),
    })
    const data = await resp.json()
    if (!resp.ok) {
      if (resp.status === 401) authenticated.value = false
      userDiscountError.value = data.error || `拉取失败（${resp.status}）`
      return
    }
    userDiscountCurrent.value = data.current
    userDiscountHistory.value = data.history || []
  } catch (err) {
    userDiscountError.value = '请求失败：' + err.message
  } finally {
    pullingUserDiscount.value = false
  }
}

function onFileChange(e) {
  const file = e.target.files && e.target.files[0]
  selectedFileName.value = file ? file.name : ''
  // 选了文件就把分组列出来，不必先点「检查价格覆盖」——勾选是出账前的事，
  // 和价格检查是两件事。
  if (file) loadGroups()
}

function fmtNum(v) {
  if (v === undefined || v === null) return '-'
  return Number(v).toLocaleString('zh-CN', { maximumFractionDigits: 2 })
}

function fmtMoney(v) {
  if (v === undefined || v === null) return '-'
  return Number(v).toLocaleString('zh-CN', { minimumFractionDigits: 4, maximumFractionDigits: 4 })
}

const hasMissingPrice = computed(
  () => result.value && result.value.summary.missingPriceModels && result.value.summary.missingPriceModels.length > 0
)

const selectedDomesticGroupCount = computed(
  () => Object.values(selectedDomesticGroups.value).filter(Boolean).length
)

function clearDomesticGroups() {
  selectedDomesticGroups.value = {}
}

// appendSourceFields 把当前选择的日志来源（上传文件 或 服务器路径）写入 FormData，
// 供生成账单和检查价格覆盖两个请求共用。返回 false 表示校验未通过（已写入 errorMsg）。
function appendSourceFields(fd, errRef) {
  if (sourceMode.value === 'upload') {
    const file = fileInput.value && fileInput.value.files && fileInput.value.files[0]
    if (!file) {
      errRef.value = '请先选择日志文件（.xlsx / .csv / .tsv）'
      return false
    }
    fd.append('file', file)
  } else {
    if (!serverPath.value.trim()) {
      errRef.value = '请填写服务器上的源文件路径，或点击「浏览服务器文件」选择'
      return false
    }
    fd.append('serverPath', serverPath.value.trim())
  }
  return true
}

// 导出日志明细：默认取上个月整月（00:00:00 至 23:59:59），这是出账最常用的区间。
// 用本地时间拼接，因为 datetime-local 的取值本身就是「无时区的墙钟时间」，
// 时区解释统一交给后端按 +08:00 处理。
function defaultExportRange() {
  const now = new Date()
  const firstOfThisMonth = new Date(now.getFullYear(), now.getMonth(), 1)
  const lastMonthEnd = new Date(firstOfThisMonth.getTime() - 24 * 3600 * 1000)
  const firstOfLastMonth = new Date(lastMonthEnd.getFullYear(), lastMonthEnd.getMonth(), 1)
  const fmt = (d, h, m, s) => {
    const p = (n) => String(n).padStart(2, '0')
    return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}T${p(h)}:${p(m)}:${p(s)}`
  }
  return {
    startAt: fmt(firstOfLastMonth, 0, 0, 0),
    endAt: fmt(lastMonthEnd, 23, 59, 59),
  }
}

const maxExportDays = 92
const exporting = ref(false)
const exportError = ref('')
const exportResult = ref(null)
const exportForm = ref({
  usernames: '',
  userIds: '',
  includeUserId: false,
  ...defaultExportRange(),
})

// 出账前的渠道倍率预检：读一遍将要出账的日志，看用到的渠道有没有维护倍率。
// 放在出账之前，是为了避免「生成完账单才发现有渠道没维护」白跑一遍。
const checkingChannels = ref(false)
const checkChannelsError = ref('')
const checkChannelsDone = ref(false)
const checkChannelsMsg = ref('')
// 日志里用到的渠道 + 各自填写的倍率，可就地补录。
const usedChannels = ref([])
const usedRatioDraft = ref({})
const savingUsedRatios = ref(false)

// usedMissingCount 数的是**服务端真的没维护**的渠道，用来提示还要补几个。
// 不能用它决定保存按钮——它有值就等于「已维护」了，按钮会消失。
const usedMissingCount = computed(
  () => usedChannels.value.filter((c) => c.upstreamRatio === null || c.upstreamRatio === undefined).length
)

// usedUnsavedCount 数的是「填了但还没提交」的渠道，和 saveUsedRatios 实际会提交的条数一致。
// 保存按钮必须看这个：否则把待补录的框填满后 missingCount 归零、按钮消失，
// 用户会以为已经保存好了，实际一个字节都没写进 PG。
const usedUnsavedCount = computed(
  () => usedChannels.value.filter((c) => usedDirty(c)).length
)

// usedDirty 判断「这个渠道填了但还没提交」。
//
// 注意 v-model 绑在 type="number" 的输入框上时，Vue 会自动套 .number 修饰符，
// 值随用户输入在 string / number 之间变（空串仍是 ''）。所以统一 String() 归一化，
// 不能直接 .trim()——那会在用户输入数字的那一瞬间抛 TypeError。
// 这个函数在模板渲染期被调用（:class / v-if），抛异常会让 Vue 卸载整棵组件树，
// 表现是整页白屏，而不是某个输入框报错。
function usedDirty(c) {
  const raw = String(usedRatioDraft.value[c.channelId] ?? '').trim()
  if (raw === '') return false
  const num = Number(raw)
  if (!Number.isFinite(num) || num < 0) return false
  const before = c.upstreamRatio === null || c.upstreamRatio === undefined ? null : c.upstreamRatio
  return before === null || num !== before
}

async function checkChannels() {
  checkChannelsError.value = ''
  checkChannelsMsg.value = ''
  checkChannelsDone.value = false
  usedChannels.value = []
  usedRatioDraft.value = {}

  const fd = new FormData()
  if (!appendSourceFields(fd, checkChannelsError)) return

  checkingChannels.value = true
  try {
    const resp = await fetch('/api/check-channels', { method: 'POST', body: fd })
    const data = await resp.json()
    if (!resp.ok) {
      if (resp.status === 401) authenticated.value = false
      checkChannelsError.value = data.error || `检查失败（${resp.status}）`
      return
    }
    checkChannelsDone.value = true

    if (!data.hasChannelColumn) {
      // 日志没有 channel_id：不是"检查没通过"，而是这份日志做不了成本估算。
      checkChannelsMsg.value = data.message || '这份日志没有 channel_id 列，无法估算成本。'
      return
    }

    usedChannels.value = data.usedChannels || []
    const draft = {}
    for (const c of usedChannels.value) {
      draft[c.channelId] = c.upstreamRatio === null || c.upstreamRatio === undefined ? '' : String(c.upstreamRatio)
    }
    usedRatioDraft.value = draft

    if (data.missingCount === 0 && data.unknownCount === 0) {
      checkChannelsMsg.value = `日志用到的 ${usedChannels.value.length} 个渠道都已维护倍率，可以生成成本表。`
    } else {
      checkChannelsMsg.value = ''
    }
    if (data.channelTableEmpty) {
      checkChannelsMsg.value = '本地渠道清单还是空的，建议先点上面的「拉取渠道清单」把渠道名称补全。'
    }
  } catch (err) {
    checkChannelsError.value = '检查失败：' + err.message
  } finally {
    checkingChannels.value = false
  }
}

// 就地补录：只提交有改动、且有值的项。
async function saveUsedRatios() {
  checkChannelsError.value = ''
  checkChannelsMsg.value = ''
  const items = []
  for (const c of usedChannels.value) {
    // 同 usedDirty：type=number 的 v-model 会给到 number，必须 String() 归一化后再 trim。
    const raw = String(usedRatioDraft.value[c.channelId] ?? '').trim()
    if (raw === '') continue
    const num = Number(raw)
    if (!Number.isFinite(num) || num < 0) {
      checkChannelsError.value = `渠道 ${c.channelId} 的倍率必须是非负数字`
      return
    }
    // 用数值比较，避免 "0.60" 与已存的 0.6 被当成改动而重复提交。
    const before = c.upstreamRatio === null || c.upstreamRatio === undefined ? null : c.upstreamRatio
    if (before !== null && before === num) continue
    items.push({ channelId: c.channelId, upstreamRatio: num, note: '' })
  }
  if (items.length === 0) {
    checkChannelsMsg.value = '没有新的倍率需要保存。'
    return
  }

  savingUsedRatios.value = true
  try {
    const resp = await fetch('/api/channel-ratios', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ items }),
    })
    const data = await resp.json()
    if (!resp.ok) {
      if (resp.status === 401) authenticated.value = false
      checkChannelsError.value = data.error || `保存失败（${resp.status}）`
      return
    }
    checkChannelsMsg.value = `已保存 ${data.saved} 个渠道的倍率，可以生成成本表了。`
    // 保存后刷新两处清单：预检结果与下方维护卡片。
    await checkChannels()
  } catch (err) {
    checkChannelsError.value = '保存失败：' + err.message
  } finally {
    savingUsedRatios.value = false
  }
}

// 渠道成本倍率维护：拉取业务库渠道清单，人工填上游倍率。
// 出账结果里提示「有渠道未维护倍率」时，切到渠道页而不是原地滚动——
// 那边有完整的清单与保存按钮，比在出账页里就地改清楚。
function focusChannelCard() {
  activePage.value = 'channels'
  // 切过去后刷新一次，保证看到的是最新状态。
  nextTick(() => {
    const child = channelRatiosRef.value
    if (child && typeof child.loadChannels === 'function') child.loadChannels()
  })
}

async function exportLogs() {
  exportError.value = ''
  exportResult.value = null

  if (!exportForm.value.usernames.trim() && !exportForm.value.userIds.trim()) {
    exportError.value = '请至少填写一个客户账号或用户 ID'
    return
  }
  if (!exportForm.value.startAt || !exportForm.value.endAt) {
    exportError.value = '请填写开始时间与结束时间'
    return
  }

  const fd = new FormData()
  fd.append('usernames', exportForm.value.usernames)
  fd.append('userIds', exportForm.value.userIds)
  fd.append('startAt', exportForm.value.startAt)
  fd.append('endAt', exportForm.value.endAt)
  fd.append('includeUserId', String(exportForm.value.includeUserId))

  exporting.value = true
  try {
    const resp = await fetch('/api/export-logs', { method: 'POST', body: fd })
    const data = await resp.json()
    if (!resp.ok) {
      if (resp.status === 401) authenticated.value = false
      exportError.value = data.error || `导出失败（${resp.status}）`
      return
    }
    exportResult.value = data
    // 刚导出的文件直接出现在下面的列表里，不用再点一次刷新。
    loadDataLogs()
  } catch (err) {
    exportError.value = '导出失败：' + err.message
  } finally {
    exporting.value = false
  }
}

// 把导出结果回填成账单的输入路径，省去再上传一次。
function useExportAsBillInput() {
  if (!exportResult.value) return
  serverPath.value = exportResult.value.exportedPath
  sourceMode.value = 'server'
  loadGroups()
}

// 已导出的日志文件列表：查看与手动清理 data 目录。
const dataLogs = ref([])
const dataLogDir = ref('')
const dataLogsLoaded = ref(false)
const loadingDataLogs = ref(false)
const dataLogsError = ref('')
const dataLogsMessage = ref('')
const deletingLogs = ref(false)
const selectedDataLogs = ref([])

const allDataLogsSelected = computed(
  () => dataLogs.value.length > 0 && selectedDataLogs.value.length === dataLogs.value.length
)

function fmtSize(bytes) {
  if (bytes === undefined || bytes === null) return '-'
  const kb = bytes / 1024
  if (kb < 1024) return `${kb.toFixed(1)} KB`
  return `${(kb / 1024).toFixed(1)} MB`
}

async function loadDataLogs() {
  dataLogsError.value = ''
  dataLogsMessage.value = ''
  loadingDataLogs.value = true
  try {
    const resp = await fetch('/api/data-logs')
    const data = await resp.json()
    if (!resp.ok) {
      if (resp.status === 401) authenticated.value = false
      dataLogsError.value = data.error || `读取失败（${resp.status}）`
      return
    }
    dataLogs.value = data.files || []
    dataLogDir.value = data.dataDir || ''
    dataLogsLoaded.value = true
    // 列表刷新后丢掉已经不存在的勾选项，避免删到别人。
    const present = new Set(dataLogs.value.map((f) => f.name))
    selectedDataLogs.value = selectedDataLogs.value.filter((n) => present.has(n))
  } catch (err) {
    dataLogsError.value = '读取失败：' + err.message
  } finally {
    loadingDataLogs.value = false
  }
}

function toggleAllDataLogs(checked) {
  selectedDataLogs.value = checked ? dataLogs.value.map((f) => f.name) : []
}

// 删除是不可逆操作，逐个确认并明确列出文件名——批量静默删除日志太危险。
async function deleteSelectedLogs() {
  const names = [...selectedDataLogs.value]
  if (names.length === 0) return

  const list = names.length <= 5 ? names.map((n) => `· ${n}`).join('\n') : `· ${names.slice(0, 5).join('\n· ')}\n…… 共 ${names.length} 个`
  if (typeof confirm === 'function' && !confirm(`确认删除以下 ${names.length} 个日志文件？此操作不可恢复。\n\n${list}`)) {
    return
  }

  dataLogsError.value = ''
  dataLogsMessage.value = ''
  deletingLogs.value = true
  const failed = []
  let okCount = 0
  try {
    for (const name of names) {
      const fd = new FormData()
      fd.append('name', name)
      const resp = await fetch('/api/delete-log-file', { method: 'POST', body: fd })
      if (!resp.ok) {
        const data = await resp.json().catch(() => ({}))
        if (resp.status === 401) authenticated.value = false
        failed.push(`${name}：${data.error || resp.status}`)
        continue
      }
      okCount++
    }
  } catch (err) {
    failed.push('请求失败：' + err.message)
  } finally {
    deletingLogs.value = false
  }

  if (failed.length > 0) {
    dataLogsError.value = `删除完成 ${okCount} 个，失败 ${failed.length} 个：${failed.join('；')}`
  } else {
    dataLogsMessage.value = `已删除 ${okCount} 个文件。`
  }
  selectedDataLogs.value = []
  await loadDataLogs()
}

function useDataLogAsInput(f) {
  serverPath.value = f.path
  sourceMode.value = 'server'
  loadGroups()
}

// loadGroups 读取当前所选日志的 group 列去重结果并列出勾选框。
// 选定日志后自动调用（换文件/改服务器路径都会重新拉），不依赖价格检查。
async function loadGroups() {
  groupsError.value = ''
  const fd = new FormData()
  if (!appendSourceFields(fd, groupsError)) return

  loadingGroups.value = true
  try {
    const resp = await fetch('/api/log-groups', { method: 'POST', body: fd })
    const data = await resp.json()
    if (!resp.ok) {
      if (resp.status === 401) authenticated.value = false
      groupsError.value = data.error || `读取分组失败（${resp.status}）`
      return
    }
    groups.value = data.groups || []
    // 保留已勾选、且这次日志里仍然存在的分组，重跑不用重勾。
    const nextSelected = {}
    for (const g of groups.value) {
      if (selectedDomesticGroups.value[g]) nextSelected[g] = true
    }
    selectedDomesticGroups.value = nextSelected
  } catch (err) {
    groupsError.value = '读取分组失败：' + err.message
  } finally {
    loadingGroups.value = false
  }
}

async function checkMissingPrices() {
  checkPricesError.value = ''
  priceCheckDone.value = false

  const fd = new FormData()
  if (!appendSourceFields(fd, checkPricesError)) return
  fd.append('priceSource', form.value.priceSource)
  if (form.value.exchangeRate !== '') fd.append('exchangeRate', String(form.value.exchangeRate))

  checkingPrices.value = true
  try {
    const resp = await fetch('/api/check-prices', { method: 'POST', body: fd })
    const data = await resp.json()
    if (!resp.ok) {
      if (resp.status === 401) authenticated.value = false
      checkPricesError.value = data.error || `检查失败（${resp.status}）`
      return
    }
    checkedModelCount.value = data.modelCount || 0
    missingModels.value = data.missingModels || []
    exprModels.value = data.exprModels || []
    groups.value = data.groups || []
    // 已勾选过、这次日志里仍然存在的分组保留勾选状态，重新检查不用重勾。
    const nextSelected = {}
    for (const g of groups.value) {
      if (selectedDomesticGroups.value[g]) nextSelected[g] = true
    }
    selectedDomesticGroups.value = nextSelected
    const nextManual = {}
    for (const model of missingModels.value) {
      nextManual[model] = manualPrices.value[model] || { input: '', output: '' }
    }
    manualPrices.value = nextManual
    priceCheckDone.value = true
  } catch (err) {
    checkPricesError.value = '检查失败：' + err.message
  } finally {
    checkingPrices.value = false
  }
}

async function handleSubmit() {
  errorMsg.value = ''
  result.value = null

  const fd = new FormData()
  if (!appendSourceFields(fd, errorMsg)) return
  if (form.value.month !== '') fd.append('month', String(form.value.month))
  if (form.value.year !== '') fd.append('year', String(form.value.year))
  if (form.value.exchangeRate !== '') fd.append('exchangeRate', String(form.value.exchangeRate))
  if (form.value.discount !== '') fd.append('discount', String(form.value.discount))
  fd.append('priceSource', form.value.priceSource)
  fd.append('sanitizedLog', String(form.value.sanitizedLog))
  fd.append('sanitizedFormat', form.value.sanitizedFormat)
  fd.append('includeBillingParams', String(form.value.includeBillingParams))
  fd.append('keepLog', String(form.value.keepLog))
  fd.append('generateCost', String(form.value.generateCost))

  const manualEntries = {}
  for (const [model, p] of Object.entries(manualPrices.value)) {
    if (p.input !== '' && p.output !== '' && p.input !== undefined && p.output !== undefined) {
      manualEntries[model] = { inputPerM: Number(p.input), outputPerM: Number(p.output) }
    }
  }
  if (Object.keys(manualEntries).length > 0) {
    fd.append('manualPrices', JSON.stringify(manualEntries))
  }
  // 国产/站内定价标识：勾选的分组名 + 手填的模型名前缀，合并后交给后端
  // （后端统一做去空白、去重与前缀匹配）。
  const domesticMarkers = [
    ...Object.keys(selectedDomesticGroups.value).filter((g) => selectedDomesticGroups.value[g]),
    ...domesticModelPrefixes.value.split(/[\n,，]/).map((s) => s.trim()).filter(Boolean),
  ]
  if (domesticMarkers.length > 0) {
    fd.append('domesticMarkers', domesticMarkers.join('\n'))
  }

  loading.value = true
  try {
    const resp = await fetch('/api/bill', { method: 'POST', body: fd })
    const data = await resp.json()
    if (!resp.ok) {
      if (resp.status === 401) authenticated.value = false
      errorMsg.value = data.error || `请求失败（${resp.status}）`
      return
    }
    result.value = data
  } catch (err) {
    errorMsg.value = '请求失败：' + err.message
  } finally {
    loading.value = false
  }
}
</script>

<template>
  <div class="page" v-if="authChecked && !authenticated">
    <h1>钛动账单工具</h1>
    <form class="card login-card" @submit.prevent="handleLogin">
      <h2>登录</h2>
      <div class="field">
        <label>用户名</label>
        <input v-model="loginForm.username" type="text" autocomplete="username" />
      </div>
      <div class="field">
        <label>密码</label>
        <input v-model="loginForm.password" type="password" autocomplete="current-password" />
      </div>
      <button type="submit" :disabled="loginLoading">{{ loginLoading ? '登录中…' : '登录' }}</button>
      <p class="error" v-if="loginError">{{ loginError }}</p>
    </form>
  </div>

  <div class="page" v-else-if="authChecked">
    <div class="topbar">
      <h1>钛动账单工具</h1>
      <button type="button" class="btn-logout" @click="handleLogout">退出登录</button>
    </div>
    <p class="subtitle">上传日志（xlsx / csv / tsv），生成账单与脱敏日志</p>

    <div class="page-tabs">
      <button
        type="button"
        :class="{ active: activePage === 'bill' }"
        @click="activePage = 'bill'"
      >生成账单</button>
      <button
        type="button"
        :class="{ active: activePage === 'channels' }"
        @click="activePage = 'channels'"
      >渠道成本倍率</button>
    </div>

    <!-- 渠道成本倍率：独立页面 -->
    <ChannelRatios
      v-if="activePage === 'channels'"
      ref="channelRatiosRef"
      @unauthorized="authenticated = false"
    />

    <template v-if="activePage === 'bill'">
    <div class="field source-switch">
      <label>日志来源</label>
      <div class="source-tabs">
        <button
          type="button"
          :class="{ active: sourceMode === 'upload' }"
          @click="sourceMode = 'upload'"
        >上传文件</button>
        <button
          type="button"
          :class="{ active: sourceMode === 'server' }"
          @click="sourceMode = 'server'"
        >服务器路径</button>
      </div>
    </div>

    <form class="card" @submit.prevent="handleMerge">
      <h2>日志合并</h2>
      <p class="hint">
        日志来源有多个文件时，先在这里按顺序合成一个日志，合并结果写入服务器 data 目录。
        各文件的列名需与第一个文件一致（列顺序可以不同，会按列名自动对齐）。
      </p>

      <div class="field" v-if="sourceMode === 'upload'">
        <label>日志文件（可多选，至少两个）</label>
        <input ref="mergeInput" type="file" accept=".xlsx,.csv,.tsv" multiple @change="onMergeFileChange" />
        <span class="hint" v-if="mergeSelectedNames.length">
          已选择 {{ mergeSelectedNames.length }} 个：{{ mergeSelectedNames.join('、') }}
        </span>
      </div>

      <div class="field" v-else>
        <label>服务器文件路径（每行一个，至少两个）</label>
        <div class="path-row">
          <textarea
            v-model="mergeServerPaths"
            rows="3"
            placeholder="logs/8月上旬日志.xlsx&#10;logs/8月下旬日志.xlsx"
          ></textarea>
          <button type="button" class="btn-browse" @click="openMergeBrowser">浏览服务器文件</button>
        </div>
        <span class="hint">
          相对路径基于浏览根目录（BILL_BROWSE_ROOT），也可填写该目录下的绝对路径；
          点右侧按钮可进目录连续勾选多个文件
        </span>
      </div>

      <div class="grid">
        <div class="field">
          <label>合并结果格式</label>
          <select v-model="mergeForm.format">
            <option value="xlsx">xlsx（单表最多约 104 万行，超出会自动拆分多个 sheet）</option>
            <option value="csv">csv（纯文本，无行数上限，适合超大日志）</option>
            <option value="tsv">tsv（纯文本，无行数上限，适合超大日志）</option>
          </select>
        </div>
      </div>

      <div class="checkboxes">
        <label><input v-model="mergeForm.dedupe" type="checkbox" /> 去除完全重复的行</label>
      </div>

      <button type="submit" :disabled="merging">{{ merging ? '合并中…' : '合并日志' }}</button>
      <p class="error" v-if="mergeError">{{ mergeError }}</p>

      <div v-if="mergeResult">
        <p>
          合并 {{ mergeResult.inputCount }} 个文件（共 {{ fmtNum(mergeResult.inputRows) }} 行），
          结果 {{ fmtNum(mergeResult.rowCount) }} 行<template v-if="mergeResult.droppedRows > 0">，去重丢弃 {{ fmtNum(mergeResult.droppedRows) }} 行</template>。
        </p>
        <p class="hint">已写入：{{ mergeResult.mergedPath }}</p>
        <div class="downloads">
          <a class="btn" :href="mergeResult.mergedUrl">下载合并日志：{{ mergeResult.mergedFileName }}</a>
          <button type="button" class="btn-browse" @click="useMergedAsBillInput">作为账单输入</button>
        </div>
      </div>
    </form>

    <form class="card" @submit.prevent="pullUserDiscount">
      <h2>客户折扣核对</h2>
      <p class="hint">
        按用户名或用户 ID 拉取该用户在业务系统里配置的分组倍率，换算成折扣，存一份快照到本地数据库，
        便于核对系统折扣与线下报价折扣是否一致。每次点击「拉取」才会连一次业务数据库（只读查询）。
      </p>

      <div class="grid">
        <div class="field">
          <label>用户名</label>
          <input v-model="userDiscountForm.username" type="text" placeholder="用户名或用户 ID 至少填一项" />
        </div>
        <div class="field">
          <label>用户 ID</label>
          <input v-model="userDiscountForm.userId" type="number" placeholder="用户名或用户 ID 至少填一项" />
        </div>
      </div>

      <button type="submit" :disabled="pullingUserDiscount">{{ pullingUserDiscount ? '拉取中…' : '拉取折扣' }}</button>
      <p class="error" v-if="userDiscountError">{{ userDiscountError }}</p>

      <div v-if="userDiscountCurrent">
        <p>
          用户：{{ userDiscountCurrent.username }}（ID {{ userDiscountCurrent.userId }}） ｜
          分组：{{ userDiscountCurrent.userGroup }} ｜
          倍率：{{ userDiscountCurrent.groupRatio }} ｜
          折扣：{{ userDiscountCurrent.discount.toFixed(3) }} ｜
          命中规则：{{ derivedFromLabel(userDiscountCurrent.derivedFrom) }}
        </p>
        <p class="hint">拉取时间：{{ new Date(userDiscountCurrent.fetchedAt).toLocaleString('zh-CN') }}</p>
      </div>

      <table v-if="userDiscountHistory.length > 0">
        <thead>
          <tr>
            <th>拉取时间</th>
            <th>分组</th>
            <th>倍率</th>
            <th>折扣</th>
            <th>命中规则</th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="h in userDiscountHistory" :key="h.fetchedAt">
            <td>{{ new Date(h.fetchedAt).toLocaleString('zh-CN') }}</td>
            <td>{{ h.userGroup }}</td>
            <td>{{ h.groupRatio }}</td>
            <td>{{ h.discount.toFixed(3) }}</td>
            <td>{{ derivedFromLabel(h.derivedFrom) }}</td>
          </tr>
        </tbody>
      </table>
    </form>

    <form class="card" @submit.prevent="exportLogs">
      <h2>导出日志明细</h2>
      <p class="hint">
        按时间段 + 客户账号直接从业务数据库导出消费日志，等价于在服务器上手动执行
        <code>mysql -e "SELECT ... " &gt; xxx.tsv</code>，省去人工导出步骤。
        导出文件落在服务器 data 目录，可直接作为「生成账单」或「日志合并」的输入。
      </p>
      <p class="hint">
        与人工导出的区别：本功能会在末尾追加 <code>other</code> 列（缓存、阶梯计费、工具调用信息都在里面；
        人工 SQL 里没有它，所以那样导出的日志算出来缓存永远是 0）。<code>other</code> 含原始请求信息，
        属敏感数据，导出文件请留在服务器、不要直接交给客户。
      </p>

      <div class="grid">
        <div class="field">
          <label>客户账号（一行一个，或用逗号分隔）</label>
          <textarea
            v-model="exportForm.usernames"
            rows="3"
            placeholder="a37836323&#10;test02"
          ></textarea>
        </div>
        <div class="field">
          <label>用户 ID（可选，一行一个）</label>
          <textarea
            v-model="exportForm.userIds"
            rows="3"
            placeholder="留空则只按账号筛选"
          ></textarea>
        </div>
      </div>

      <div class="grid">
        <div class="field">
          <label>开始时间</label>
          <input v-model="exportForm.startAt" type="datetime-local" step="1" />
        </div>
        <div class="field">
          <label>结束时间</label>
          <input v-model="exportForm.endAt" type="datetime-local" step="1" />
        </div>
      </div>
      <span class="hint">
        精确到秒，时间按北京时间（+08:00）解释；起止两个时刻都算在内（与人工导出的 BETWEEN 一致）。
        单次最多导出 {{ maxExportDays }} 天。
      </span>

      <div class="checkboxes">
        <label><input v-model="exportForm.includeUserId" type="checkbox" /> 追加 user_id 列（跨账号排查用；username 为空的老日志靠它定位）</label>
      </div>

      <button type="submit" :disabled="exporting">
        {{ exporting ? '导出中…' : '从数据库导出日志' }}
      </button>
      <p class="error" v-if="exportError">{{ exportError }}</p>

      <div v-if="exportResult">
        <p>已导出 {{ fmtNum(exportResult.rowCount) }} 行，耗时 {{ exportResult.elapsedSeconds.toFixed(1) }} 秒。</p>
        <p v-if="exportResult.rowCount === 0" class="hint">
          没有查到任何日志。请检查账号是否正确、时间段是否选错——0 行不算错误，但通常说明条件有问题。
        </p>
        <div class="path-row">
          <a class="btn" :href="exportResult.exportedUrl">下载：{{ exportResult.exportedFileName }}</a>
          <button type="button" class="btn-browse" @click="useExportAsBillInput">作为账单输入</button>
        </div>
        <p class="hint">文件位置：{{ exportResult.exportedPath }}</p>
      </div>
    </form>

    <div class="card">
      <h2>已导出的日志文件</h2>
      <p class="hint">
        列出服务器 data 目录里由本工具导出的日志，可勾选清理。这里只会列出并允许删除
        「日志查询_」开头的文件——账单模板、报价表、价格缓存不在清理范围内。
      </p>

      <div class="path-row">
        <button type="button" class="btn-browse" @click="loadDataLogs" :disabled="loadingDataLogs">
          {{ loadingDataLogs ? '读取中…' : '刷新列表' }}
        </button>
        <button
          type="button"
          class="btn-browse"
          v-if="selectedDataLogs.length > 0"
          @click="deleteSelectedLogs"
          :disabled="deletingLogs"
        >
          {{ deletingLogs ? '删除中…' : `删除选中（${selectedDataLogs.length}）` }}
        </button>
      </div>
      <p class="error" v-if="dataLogsError">{{ dataLogsError }}</p>
      <span class="hint" v-if="dataLogsMessage">{{ dataLogsMessage }}</span>
      <p class="hint" v-if="dataLogDir">目录：{{ dataLogDir }}</p>

      <table v-if="dataLogs.length > 0">
        <thead>
          <tr>
            <th>
              <input
                type="checkbox"
                :checked="allDataLogsSelected"
                @change="toggleAllDataLogs($event.target.checked)"
              />
            </th>
            <th>文件名</th>
            <th>大小</th>
            <th>导出时间</th>
            <th>操作</th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="f in dataLogs" :key="f.name">
            <td>
              <input type="checkbox" v-model="selectedDataLogs" :value="f.name" />
            </td>
            <td>{{ f.name }}</td>
            <td>{{ fmtSize(f.sizeBytes) }}</td>
            <td>{{ new Date(f.modifiedAt).toLocaleString('zh-CN') }}</td>
            <td>
              <button type="button" class="btn-link" @click="useDataLogAsInput(f)">作为账单输入</button>
            </td>
          </tr>
        </tbody>
      </table>
      <span class="hint" v-else-if="!loadingDataLogs && dataLogsLoaded">
        目录里还没有导出的日志文件。
      </span>
      <span class="hint" v-else-if="!dataLogsLoaded">
        点「刷新列表」查看已导出的日志文件。
      </span>
    </div>

    <form class="card" @submit.prevent="handleSubmit">
      <div class="field" v-if="sourceMode === 'upload'">
        <label>日志文件</label>
        <input ref="fileInput" type="file" accept=".xlsx,.csv,.tsv" @change="onFileChange" />
        <span class="hint" v-if="selectedFileName">已选择：{{ selectedFileName }}</span>
      </div>

      <div class="field" v-else>
        <label>服务器文件路径（绝对或相对路径）</label>
        <div class="path-row">
          <input
            v-model="serverPath"
            type="text"
            placeholder="例如 logs/2026-08.xlsx 或 /data/logs/2026-08.xlsx"
            @blur="loadGroups"
          />
          <button type="button" class="btn-browse" @click="openBrowser">浏览服务器文件</button>
        </div>
        <span class="hint">相对路径基于服务器配置的浏览根目录（BILL_BROWSE_ROOT），也可填写该目录下的绝对路径</span>
      </div>

      <div class="grid">
        <div class="field">
          <label>账期月份</label>
          <input v-model="form.month" type="number" min="1" max="12" placeholder="从文件名/日志推断" />
        </div>
        <div class="field">
          <label>账期年份</label>
          <input v-model="form.year" type="number" placeholder="从日志推断" />
        </div>
        <div class="field">
          <label>汇率（美元→人民币）</label>
          <input v-model="form.exchangeRate" type="number" step="0.01" />
        </div>
        <div class="field">
          <label>强制统一折扣（可选）</label>
          <input v-model="form.discount" type="number" step="0.001" placeholder="留空则按分组自动反推" />
        </div>
      </div>

      <div class="field">
        <label>国产/站内定价标识（可选）</label>
        <span class="hint">
          勾选的分组不参与折扣反推，折扣改取站点实际计费倍率，并在账单备注里要求人工确认；
          走站内表达式（billing_expr）计费的分组本来就不会被反推，这里只用来兜住模型名认不出的站内定价。
        </span>
        <div class="group-picker" v-if="groups.length > 0">
          <label class="group-item" v-for="g in groups" :key="g">
            <input type="checkbox" v-model="selectedDomesticGroups[g]" />
            <span>{{ g }}</span>
          </label>
          <div class="group-actions">
            <button type="button" class="btn-link" @click="clearDomesticGroups">清空勾选</button>
            <span class="hint" v-if="selectedDomesticGroupCount > 0">
              已勾选 {{ selectedDomesticGroupCount }} 个分组
            </span>
          </div>
        </div>
        <span class="hint" v-else-if="loadingGroups">正在读取日志分组…</span>
        <p class="error" v-else-if="groupsError">{{ groupsError }}</p>
        <span class="hint" v-else>
          选定日志文件（或填好服务器路径）后，这里会自动列出该日志里的分组供勾选。
        </span>
        <input
          v-model="domesticModelPrefixes"
          type="text"
          placeholder="补充：模型名前缀，多个用逗号分隔（如 doubao,ernie）"
        />
      </div>
      <div class="field">
        <label>模型单价来源</label>
        <select v-model="form.priceSource">
          <option value="official">内置官方价（覆盖不到的模型自动回退报价表）</option>
          <option value="price_table">人工维护报价表（data/price_table.xlsx 优先）</option>
          <option value="db">业务数据库实时价格（需先手动拉取）</option>
        </select>
        <div class="path-row" v-if="form.priceSource === 'db'">
          <button type="button" class="btn-browse" @click="pullDbPrices" :disabled="pullingPrices">
            {{ pullingPrices ? '拉取中…' : '拉取最新数据库价格' }}
          </button>
        </div>
        <span class="hint" v-if="form.priceSource === 'db' && pullPricesMsg">{{ pullPricesMsg }}</span>
        <p class="error" v-if="form.priceSource === 'db' && pullPricesError">{{ pullPricesError }}</p>
        <div class="path-row">
          <button type="button" class="btn-browse" @click="checkMissingPrices" :disabled="checkingPrices">
            {{ checkingPrices ? '检查中…' : '检查模型价格覆盖' }}
          </button>
        </div>
        <p class="error" v-if="checkPricesError">{{ checkPricesError }}</p>
        <span class="hint" v-if="priceCheckDone && missingModels.length === 0">
          已检查 {{ checkedModelCount }} 个模型，当前价格来源均能匹配到定价。
        </span>        <span class="hint" v-if="priceCheckDone && exprModels.length > 0">
          其中 {{ exprModels.length }} 个模型由阶梯计费表达式定价（不依赖 ModelRatio/ModelPrice），详见下方。
        </span>
      </div>

      <div class="card" v-if="priceCheckDone && exprModels.length > 0">
        <h3>阶梯表达式定价的模型（{{ exprModels.length }}）</h3>
        <p class="hint">
          这些模型的价格由 option 表的 billing_expr 表达式算出，刊例价与单价均取自表达式，
          「缺少定价」检查不适用。同一模型跨档位时单价列取当月实际命中的档位。
        </p>
        <table>
          <thead>
            <tr>
              <th>模型</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="model in exprModels" :key="model">
              <td>{{ model }}</td>
            </tr>
          </tbody>
        </table>
      </div>

      <div class="card" v-if="priceCheckDone && missingModels.length > 0">
        <h3>缺少定价的模型（{{ missingModels.length }} / {{ checkedModelCount }}）</h3>
        <p class="hint">可在下方手动填写单价（$/MTok）补全；留空的模型仍按现有规则处理（无价则总金额/结算美金为 0）。</p>
        <table>
          <thead>
            <tr>
              <th>模型</th>
              <th>输入单价 ($/MTok)</th>
              <th>输出单价 ($/MTok)</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="model in missingModels" :key="model">
              <td>{{ model }}</td>
              <td><input v-model="manualPrices[model].input" type="number" step="0.01" min="0" /></td>
              <td><input v-model="manualPrices[model].output" type="number" step="0.01" min="0" /></td>
            </tr>
          </tbody>
        </table>
      </div>

      <div class="checkboxes">
        <label><input v-model="form.sanitizedLog" type="checkbox" /> 生成脱敏日志</label>
        <label><input v-model="form.keepLog" type="checkbox" /> 附带原日志到账单文件</label>
      </div>

      <div class="field" v-if="form.sanitizedLog">
        <label>脱敏日志格式</label>
        <select v-model="form.sanitizedFormat">
          <option value="xlsx">xlsx（单表最多约 104 万行，超出会自动拆分多个 sheet）</option>
          <option value="csv">csv（纯文本，无行数上限，适合超大日志）</option>
          <option value="tsv">tsv（纯文本，无行数上限，适合超大日志）</option>
        </select>
        <label><input v-model="form.includeBillingParams" type="checkbox" /> 附带计费参数列（模型/分组倍率等内部参数，默认不导出）</label>
        <label><input v-model="form.generateCost" type="checkbox" /> 生成成本表（账单全部列 + 渠道/上游折扣/上游成本，需先维护渠道倍率）</label>
      </div>

      <!-- 出账前预检渠道倍率：避免生成完账单才发现有渠道没维护 -->
      <div class="field">
        <label>渠道成本倍率</label>
        <span class="hint">
          勾选「生成成本表」后，建议先点检查：它会读一遍当前日志，列出里面用到的渠道，
          没维护倍率的可以就地补录，补完再生成账单。
        </span>
        <div class="path-row">
          <button type="button" class="btn-browse" @click="checkChannels" :disabled="checkingChannels">
            {{ checkingChannels ? '检查中…' : '检查渠道倍率维护情况' }}
          </button>
          <button
            type="button"
            class="btn-browse"
            v-if="usedUnsavedCount > 0"
            @click="saveUsedRatios"
            :disabled="savingUsedRatios"
          >
            {{ savingUsedRatios ? '保存中…' : `保存补录的倍率（${usedUnsavedCount}）` }}
          </button>
        </div>
        <p class="error" v-if="checkChannelsError">{{ checkChannelsError }}</p>
        <span class="hint" v-if="checkChannelsMsg">{{ checkChannelsMsg }}</span>

        <div v-if="checkChannelsDone && usedChannels.length > 0">
          <p v-if="usedMissingCount > 0 && usedUnsavedCount > 0" class="error">
            日志用到的渠道里有 {{ usedMissingCount }} 个还没维护倍率，你已填了 {{ usedUnsavedCount }} 个但尚未保存——
            未维护的渠道成本列会留空、不计入合计。点旁边的按钮保存，再生成账单。
          </p>
          <p v-else-if="usedMissingCount > 0" class="error">
            日志用到的渠道里有 {{ usedMissingCount }} 个还没维护倍率——未维护的渠道成本列会留空、不计入合计。
            在下面补录后保存，再生成账单即可。
          </p>
          <p v-else-if="usedUnsavedCount > 0" class="error">
            有 {{ usedUnsavedCount }} 个倍率已填写但尚未保存，保存后才能用于生成成本表。
          </p>
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
              <tr
                v-for="c in usedChannels"
                :key="c.channelId"
                :class="{
                  'row-missing': c.upstreamRatio === null || c.upstreamRatio === undefined,
                  'row-unsaved': usedDirty(c),
                }"
              >
                <td>{{ c.channelId }}</td>
                <td>{{ c.name }}</td>
                <td>
                  <input
                    v-model="usedRatioDraft[c.channelId]"
                    type="number"
                    step="0.01"
                    min="0"
                    placeholder="未维护"
                    class="ratio-input"
                  />
                </td>
                <td>
                  <span v-if="c.upstreamRatio === null || c.upstreamRatio === undefined">未维护</span>
                  <span v-else>已维护</span>
                  <!-- 「填了未保存」必须与「从未维护」分开：以前状态列读的是输入框草稿，
                       一敲键盘就显示「已维护」，用户以为存好了，实际没提交。 -->
                  <span v-if="usedDirty(c)" class="unsaved-tag">填了未保存</span>
                </td>
              </tr>
            </tbody>
          </table>
        </div>
      </div>

      <button type="submit" :disabled="loading">{{ loading ? '生成中…' : '生成账单' }}</button>
      <p class="error" v-if="errorMsg">{{ errorMsg }}</p>
    </form>

    <div class="card" v-if="result">
      <h2>结果</h2>
      <p>
        账期：{{ result.summary.year }}-{{ String(result.summary.month).padStart(2, '0') }} ｜
        原始行数：{{ fmtNum(result.summary.rowCount) }} ｜
        含缓存行：{{ fmtNum(result.summary.cacheHitRows) }} ｜
        含 web_search 行：{{ fmtNum(result.summary.webSearchRows) }}
      </p>
      <p>
        结算人民币合计：¥{{ fmtMoney(result.summary.settleCnyTotal) }} ｜
        总金额人民币合计：¥{{ fmtMoney(result.summary.listCnyTotal) }} ｜
        综合折扣：{{ result.summary.overallDiscount.toFixed(3) }}
      </p>
      <p class="warning" v-if="hasMissingPrice">
        以下 (模型/分组) 缺少定价，账单中「一致性」列会标为「否」：
        {{ result.summary.missingPriceModels.join('，') }}
      </p>

      <div class="downloads">
        <a class="btn" :href="result.billUrl">下载账单：{{ result.billFileName }}</a>
        <a class="btn" v-if="result.sanitizedUrl" :href="result.sanitizedUrl">下载脱敏日志：{{ result.sanitizedFileName }}</a>
        <a class="btn" v-if="result.costUrl" :href="result.costUrl">下载成本表：{{ result.costFileName }}</a>
      </div>

      <!-- 成本表被拦下：账单已生成，只是有渠道没维护倍率。不是错误，给出补录入口。 -->
      <div v-if="result.costBlocked" class="cost-blocked">
        <p class="error">
          成本表未生成：有渠道还没维护上游倍率。账单已正常生成，补录后重新生成即可。
        </p>
        <p v-if="result.missingChannels && result.missingChannels.length > 0">
          <strong>待补录渠道</strong>：
          <span v-for="c in result.missingChannels" :key="c.channelId">
            {{ c.name }}（{{ c.channelId }}）
          </span>
        </p>
        <button type="button" class="btn-browse" @click="focusChannelCard">去维护渠道倍率</button>
      </div>

      <!-- 未知渠道提示放在拦截框外面：这类渠道无法补录，成本表照常生成，
           但用户得知道成本表里哪几个渠道号的成本列是空的。 -->
      <p
        v-if="result.unknownChannelIds && result.unknownChannelIds.length > 0"
        class="hint cost-note"
      >
        有 {{ result.unknownChannelIds.length }} 个渠道号在渠道表里查不到（{{
          result.unknownChannelIds.join('，')
        }}）——这些渠道多半已在业务库被删除，无法维护倍率，成本表里对应的成本列会留空。
      </p>
      <table>
        <thead>
          <tr>
            <th>模型</th>
            <th>分组</th>
            <th>未命中</th>
            <th>缓存读</th>
            <th>输出</th>
            <th>写5m</th>
            <th>写1h</th>
            <th>quota</th>
            <th>结算(¥)</th>
            <th>总金额(¥)</th>
            <th>折扣</th>
            <th>行数</th>
            <th>定价</th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="row in result.summary.rows" :key="row.model + '/' + row.group">
            <td>{{ row.model }}</td>
            <td>{{ row.group }}</td>
            <td>{{ fmtNum(row.uncached) }}</td>
            <td>{{ fmtNum(row.cacheRead) }}</td>
            <td>{{ fmtNum(row.output) }}</td>
            <td>{{ fmtNum(row.cacheWrite5m) }}</td>
            <td>{{ fmtNum(row.cacheWrite1h) }}</td>
            <td>{{ fmtNum(row.quota) }}</td>
            <td>{{ fmtMoney(row.settleCny) }}</td>
            <td>{{ fmtMoney(row.listCny) }}</td>
            <td>{{ row.discount.toFixed(3) }}</td>
            <td>{{ row.rows }}</td>
            <td>{{ row.hasPrice ? '是' : '否' }}</td>
          </tr>
        </tbody>
      </table>
    </div>

    <div class="modal-mask" v-if="browserOpen" @click.self="closeBrowser">
      <div class="modal-box">
        <div class="modal-header">
          <h3>{{ browserMode === 'multi' ? '选择要合并的服务器文件（可多选）' : '选择服务器文件' }}</h3>
          <button type="button" class="btn-close" @click="closeBrowser">×</button>
        </div>
        <p class="hint">根目录：{{ browserRoot }}　当前：/{{ browserPath }}</p>
        <p class="error" v-if="browserError">{{ browserError }}</p>
        <div class="browser-list">
          <div class="browser-row" v-if="browserPath" @click="loadBrowse(browserParent)">📁 ..（上一级）</div>
          <div
            class="browser-row"
            v-for="entry in browserEntries"
            :key="entry.path"
            :class="{ picked: browserMode === 'multi' && !entry.isDir && isPicked(entry) }"
            @click="pickEntry(entry)"
          >
            <span>
              <input
                v-if="browserMode === 'multi' && !entry.isDir"
                type="checkbox"
                :checked="isPicked(entry)"
                @click.stop="pickEntry(entry)"
              />
              {{ entry.isDir ? '📁' : '📄' }} {{ entry.name }}
            </span>
            <span class="browser-meta" v-if="!entry.isDir">{{ entry.modTime }}</span>
          </div>
          <p class="hint" v-if="!browserLoading && browserEntries.length === 0">（空目录）</p>
          <p class="hint" v-if="browserLoading">加载中…</p>
        </div>
        <div class="modal-footer" v-if="browserMode === 'multi'">
          <span class="hint">已勾选 {{ browserPicked.length }} 个文件</span>
          <div>
            <button type="button" class="btn-browse" @click="closeBrowser">取消</button>
            <button type="button" @click="confirmBrowserPick" :disabled="browserPicked.length === 0">
              加入路径列表
            </button>
          </div>
        </div>
      </div>
    </div>
    </template>
  </div>
</template>

<style scoped>
.page {
  max-width: 1080px;
  margin: 0 auto;
  padding: 24px;
  font-family: -apple-system, 'Segoe UI', 'Microsoft YaHei', sans-serif;
  color: #1f2328;
}
.subtitle {
  color: #666;
  margin-top: -8px;
}
.topbar {
  display: flex;
  align-items: center;
  justify-content: space-between;
}
.btn-logout {
  background: #f0f4ff;
  color: #2c6ef2;
  border: 1px solid #c7d7fb;
  border-radius: 6px;
  padding: 6px 14px;
  cursor: pointer;
  font-size: 13px;
}
.login-card {
  max-width: 360px;
  margin: 60px auto 0;
}
.source-switch {
  margin-bottom: 12px;
}
.source-tabs {
  display: flex;
  gap: 8px;
}
.source-tabs button {
  background: #f5f6f8;
  color: #444;
  border: 1px solid #d8d8d8;
  border-radius: 6px;
  padding: 6px 14px;
  cursor: pointer;
  font-size: 13px;
}
.source-tabs button.active {
  background: #2c6ef2;
  color: #fff;
  border-color: #2c6ef2;
}
.page-tabs {
  display: flex;
  gap: 8px;
  margin-bottom: 16px;
  border-bottom: 1px solid #e3e6eb;
}
.page-tabs button {
  background: none;
  border: none;
  border-bottom: 2px solid transparent;
  padding: 8px 4px;
  margin-bottom: -1px;
  font-size: 14px;
  color: #555;
  cursor: pointer;
}
.page-tabs button.active {
  color: #2c6ef2;
  border-bottom-color: #2c6ef2;
  font-weight: 600;
}
.ratio-input {
  width: 100px;
}
.row-missing {
  background: #fff8e1;
}
.cost-blocked {
  margin-top: 10px;
  padding: 8px 10px;
  border: 1px solid #f0c36d;
  border-radius: 4px;
  background: #fffdf5;
}
.cost-note {
  margin-top: 10px;
}
/* 填了但还没保存的行：与「从未维护」区分开。
   以前两者共用一个底色，用户填完以为已生效，实际一个字节都没提交。 */
.row-unsaved {
  background: #eef4ff;
}
.unsaved-tag {
  display: inline-block;
  margin-left: 6px;
  padding: 0 6px;
  border-radius: 3px;
  background: #e3ecff;
  color: #2c6ef2;
  font-size: 12px;
}
.group-picker {
  display: flex;
  flex-wrap: wrap;
  gap: 8px 16px;
  padding: 8px 10px;
  margin: 6px 0;
  border: 1px solid #ddd;
  border-radius: 4px;
  max-height: 180px;
  overflow-y: auto;
}
.group-item {
  display: inline-flex;
  align-items: center;
  gap: 4px;
  font-size: 13px;
  color: #444;
  cursor: pointer;
}
.group-actions {
  flex-basis: 100%;
  display: flex;
  align-items: center;
  gap: 10px;
}
.btn-link {
  background: none;
  border: none;
  padding: 0;
  color: #2c6ef2;
  font-size: 12px;
  cursor: pointer;
  text-decoration: underline;
}
textarea {
  width: 100%;
  padding: 6px 8px;
  border: 1px solid #ccc;
  border-radius: 4px;
  font-family: inherit;
  font-size: 13px;
  resize: vertical;
  box-sizing: border-box;
}
.path-row {
  display: flex;
  gap: 8px;
  align-items: flex-start;
}
.path-row input {
  flex: 1;
  padding: 6px 8px;
  border: 1px solid #ccc;
  border-radius: 4px;
}
.path-row textarea {
  flex: 1;
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
.modal-mask {
  position: fixed;
  inset: 0;
  background: rgba(0, 0, 0, 0.4);
  display: flex;
  align-items: center;
  justify-content: center;
  z-index: 100;
}
.modal-box {
  background: #fff;
  border-radius: 8px;
  padding: 20px;
  width: 520px;
  max-width: 90vw;
  max-height: 70vh;
  display: flex;
  flex-direction: column;
}
.modal-header {
  display: flex;
  align-items: center;
  justify-content: space-between;
  margin-bottom: 8px;
}
.modal-header h3 {
  margin: 0;
}
.btn-close {
  background: none;
  border: none;
  font-size: 20px;
  line-height: 1;
  cursor: pointer;
  color: #666;
}
.browser-list {
  overflow-y: auto;
  border: 1px solid #e2e2e2;
  border-radius: 6px;
}
.browser-row {
  display: flex;
  justify-content: space-between;
  padding: 8px 12px;
  cursor: pointer;
  font-size: 13px;
  border-bottom: 1px solid #f0f0f0;
}
.browser-row:hover {
  background: #f7f8fa;
}
.browser-row.picked {
  background: #eef4ff;
}
.browser-row input[type='checkbox'] {
  margin-right: 4px;
  cursor: pointer;
}
.modal-footer {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  margin-top: 12px;
}
.modal-footer button {
  margin-left: 8px;
}
.browser-meta {
  color: #999;
  font-size: 12px;
}
.card {
  background: #fff;
  border: 1px solid #e2e2e2;
  border-radius: 8px;
  padding: 20px;
  margin-bottom: 20px;
}
.field {
  display: flex;
  flex-direction: column;
  gap: 4px;
  margin-bottom: 12px;
}
.field label {
  font-size: 13px;
  color: #444;
}
.field input {
  padding: 6px 8px;
  border: 1px solid #ccc;
  border-radius: 4px;
}
.field select {
  padding: 6px 8px;
  border: 1px solid #ccc;
  border-radius: 4px;
  max-width: 420px;
}
.grid {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(200px, 1fr));
  gap: 8px 16px;
}
.checkboxes {
  display: flex;
  gap: 20px;
  margin: 8px 0 16px;
  font-size: 14px;
}
.hint {
  font-size: 12px;
  color: #888;
}
button {
  background: #2c6ef2;
  color: #fff;
  border: none;
  border-radius: 6px;
  padding: 8px 20px;
  cursor: pointer;
  font-size: 14px;
}
button:disabled {
  background: #9db8ee;
  cursor: not-allowed;
}
.error {
  color: #d92626;
  margin-top: 8px;
}
.warning {
  color: #b8720b;
  background: #fff7e6;
  border: 1px solid #ffe1a8;
  padding: 8px 12px;
  border-radius: 6px;
}
.downloads {
  display: flex;
  gap: 12px;
  margin: 12px 0;
}
.btn {
  display: inline-block;
  background: #f0f4ff;
  color: #2c6ef2;
  border: 1px solid #c7d7fb;
  border-radius: 6px;
  padding: 6px 14px;
  text-decoration: none;
  font-size: 14px;
}
table {
  width: 100%;
  border-collapse: collapse;
  font-size: 13px;
  margin-top: 12px;
}
th, td {
  border: 1px solid #e2e2e2;
  padding: 6px 8px;
  text-align: right;
}
th:nth-child(1), th:nth-child(2), td:nth-child(1), td:nth-child(2) {
  text-align: left;
}
td input {
  width: 100%;
  box-sizing: border-box;
  padding: 4px 6px;
  border: 1px solid #ccc;
  border-radius: 4px;
  text-align: right;
}
thead {
  background: #f7f8fa;
}
</style>
