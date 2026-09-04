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

const browserOpen = ref(false)
const browserLoading = ref(false)
const browserError = ref('')
const browserRoot = ref('')
const browserPath = ref('')
const browserParent = ref('')
const browserEntries = ref([])

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
  browserOpen.value = true
  loadBrowse('')
}

function closeBrowser() {
  browserOpen.value = false
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
  priceSource: 'official',
  sanitizedLog: true,
  sanitizedFormat: 'xlsx',
  keepLog: false,
})

const loading = ref(false)
const errorMsg = ref('')
const result = ref(null)

const pullingPrices = ref(false)
const pullPricesMsg = ref('')
const pullPricesError = ref('')

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

async function handleSubmit() {
  errorMsg.value = ''
  result.value = null

  const fd = new FormData()
  if (sourceMode.value === 'upload') {
    const file = fileInput.value && fileInput.value.files && fileInput.value.files[0]
    if (!file) {
      errorMsg.value = '请先选择日志文件（.xlsx / .csv / .tsv）'
      return
    }
    fd.append('file', file)
  } else {
    if (!serverPath.value.trim()) {
      errorMsg.value = '请填写服务器上的源文件路径，或点击「浏览服务器文件」选择'
      return
    }
    fd.append('serverPath', serverPath.value.trim())
  }
  if (form.value.month !== '') fd.append('month', String(form.value.month))
  if (form.value.year !== '') fd.append('year', String(form.value.year))
  if (form.value.exchangeRate !== '') fd.append('exchangeRate', String(form.value.exchangeRate))
  if (form.value.discount !== '') fd.append('discount', String(form.value.discount))
  fd.append('priceSource', form.value.priceSource)
  fd.append('sanitizedLog', String(form.value.sanitizedLog))
  fd.append('sanitizedFormat', form.value.sanitizedFormat)
  fd.append('keepLog', String(form.value.keepLog))

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


    <form class="card" @submit.prevent="handleSubmit">
      <div class="field">
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
          <h3>选择服务器文件</h3>
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
            @click="entry.isDir ? loadBrowse(entry.path) : pickFile(entry)"
          >
            <span>{{ entry.isDir ? '📁' : '📄' }} {{ entry.name }}</span>
            <span class="browser-meta" v-if="!entry.isDir">{{ entry.modTime }}</span>
          </div>
          <p class="hint" v-if="!browserLoading && browserEntries.length === 0">（空目录）</p>
          <p class="hint" v-if="browserLoading">加载中…</p>
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
.path-row {
  display: flex;
  gap: 8px;
}
.path-row input {
  flex: 1;
  padding: 6px 8px;
  border: 1px solid #ccc;
  border-radius: 4px;
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
thead {
  background: #f7f8fa;
}
</style>
