<script setup>
import { ref, computed, onMounted } from 'vue'

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
}

const form = ref({
  month: '',
  year: '',
  exchangeRate: 7,
  discount: '',
  priceSource: 'db',
  sanitizedLog: true,
  sanitizedFormat: 'tsv',
  keepLog: false,
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

function onFileChange(e) {
  const file = e.target.files && e.target.files[0]
  selectedFileName.value = file ? file.name : ''
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
  fd.append('keepLog', String(form.value.keepLog))

  const manualEntries = {}
  for (const [model, p] of Object.entries(manualPrices.value)) {
    if (p.input !== '' && p.output !== '' && p.input !== undefined && p.output !== undefined) {
      manualEntries[model] = { inputPerM: Number(p.input), outputPerM: Number(p.output) }
    }
  }
  if (Object.keys(manualEntries).length > 0) {
    fd.append('manualPrices', JSON.stringify(manualEntries))
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

    <form class="card" @submit.prevent="handleSubmit">
      <div class="field" v-if="sourceMode === 'upload'">
        <label>日志文件</label>
        <input ref="fileInput" type="file" accept=".xlsx,.csv,.tsv" @change="onFileChange" />
        <span class="hint" v-if="selectedFileName">已选择：{{ selectedFileName }}</span>
      </div>

      <div class="field" v-else>
        <label>服务器文件路径（绝对或相对路径）</label>
        <div class="path-row">
          <input v-model="serverPath" type="text" placeholder="例如 logs/2026-08.xlsx 或 /data/logs/2026-08.xlsx" />
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
        </span>
        <span class="hint" v-if="priceCheckDone && exprModels.length > 0">
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
      </div>

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
