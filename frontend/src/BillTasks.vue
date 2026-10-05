<script setup>
import { computed, onMounted, ref } from 'vue'

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
  generateCost: true,
})

// 勾选与批量执行结果
const selectedTasks = ref([])
const runResults = ref([])
const validationResults = ref([])
// runResults 里带下载链接的那几条，用于展示
const runFailures = computed(() => runResults.value.filter((r) => !r.ok))
const runSuccesses = computed(() => runResults.value.filter((r) => r.ok))

const copyState = ref('')
const copyRef = ref(null)

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

async function copyText(text) {
  if (!text) return
  try {
    if (navigator.clipboard && window.isSecureContext) {
      await navigator.clipboard.writeText(text)
      copyState.value = 'ok'
      setTimeout(() => { copyState.value = '' }, 2000)
      return
    }
  } catch (err) {
    // 落到手动选中
  }
  // http 部署下剪贴板 API 不可用（只在安全上下文存在），退化为选中文本。
  const el = copyRef.value
  if (el && window.getSelection) {
    const range = document.createRange()
    range.selectNodeContents(el)
    const sel = window.getSelection()
    sel.removeAllRanges()
    sel.addRange(range)
  }
  copyState.value = 'fail'
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

      <div class="checkboxes">
        <label><input v-model="planForm.generateSanitized" type="checkbox" /> 生成脱敏日志</label>
        <label><input v-model="planForm.generateCost" type="checkbox" /> 生成成本利润表</label>
      </div>

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
        <div v-if="r.costSummary" class="cost-summary">
          <div class="cost-summary-head">
            <strong>{{ r.taskName || ('任务 ' + r.taskId) }} 成本利润摘要</strong>
            <button type="button" class="btn-browse" @click="copyText(r.costSummary)">
              {{ copyState === 'ok' ? '已复制 ✓' : copyState === 'fail' ? '复制失败，请手动选中' : '复制' }}
            </button>
          </div>
          <pre ref="copyRef" class="cost-summary-text">{{ r.costSummary }}</pre>
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
