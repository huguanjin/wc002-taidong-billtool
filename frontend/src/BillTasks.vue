<script setup>
import { computed, onMounted, ref } from 'vue'

// 账单导出任务：选客户 + 账期，一键导出账单 / 脱敏日志 / 成本利润表，并把数字沉淀下来。
//
// 产物本身仍是一次性的（落在 jobDir，6 小时后自动清理），这里**只沉淀数字**：
// 账期、结算额、成本、利润。这样「这个月导了几张账单、成本多少利润多少」
// 不依赖任何文件是否还在磁盘上。

const emit = defineEmits(['unauthorized'])

const customers = ref([])
const tasks = ref([])
const summaries = ref([])
const settings = ref(null)
const jobRetentionHours = ref(6)

const loading = ref(false)
const running = ref(false)
const savingSettings = ref(false)
const showSettings = ref(false)
const error = ref('')
const message = ref('')

// 执行表单
const runForm = ref({ customerId: 0, period: '', generateSanitized: true, generateCost: true })
// 上一次执行的结果（用于显示下载链接）
const runResult = ref(null)
const copyState = ref('')

const settingsDraft = ref({
  priceSource: 'db',
  exchangeRate: 7,
  discount: '',
  domesticMarkers: '',
  sanitizedFormat: 'tsv',
})

const hasCustomers = computed(() => customers.value.length > 0)

// 汇总里有没有「缺成本」的任务——决定要不要显示解释性提示。
const hasMissingCost = computed(() =>
  summaries.value.some((s) => (s.missingCostCount || 0) > 0 || (s.partialCostCount || 0) > 0)
)

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

  // 首次进入时用后端给的「上月」做默认账期，避免前端自己算时区算错
  // （服务器在 UTC、浏览器在东八区，两边各算一次必然有一边是错的）。
  if (!runForm.value.period && data.defaultYear && data.defaultMonth) {
    runForm.value.period = `${data.defaultYear}-${String(data.defaultMonth).padStart(2, '0')}`
  }
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

async function runTask() {
  error.value = ''
  message.value = ''
  runResult.value = null
  copyState.value = ''

  if (!runForm.value.customerId) {
    error.value = '请选择客户'
    return
  }
  const period = String(runForm.value.period || '').trim()
  if (!/^\d{4}-\d{2}$/.test(period)) {
    error.value = '请选择账期月份'
    return
  }
  const [y, m] = period.split('-').map((v) => Number(v))

  running.value = true
  try {
    const resp = await fetch('/api/run-bill-task', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        customerId: runForm.value.customerId,
        year: y,
        month: m,
        generateSanitized: runForm.value.generateSanitized,
        generateCost: runForm.value.generateCost,
      }),
    })
    const data = await resp.json()
    if (!resp.ok) {
      if (resp.status === 401) emit('unauthorized')
      error.value = data.error || `执行失败（${resp.status}）`
      return
    }
    runResult.value = data
    message.value = data.summaryPersisted === false
      ? '任务已执行、文件已生成，但统计数字未能写入数据库，请检查 PostgreSQL'
      : '任务执行完成，账单已生成'
    await loadTasks()
  } catch (err) {
    error.value = '执行失败：' + err.message
  } finally {
    running.value = false
  }
}

// 重跑：把明细行里的客户和账期填回执行表单，再点一次一键执行即可。
// 不做自动执行——产出文件需要用户明确动作，不该点一下就默默跑几十秒。
function refillRun(row) {
  runForm.value.customerId = row.customerId
  runForm.value.period = `${row.periodYear}-${String(row.periodMonth).padStart(2, '0')}`
  message.value = '已填入该任务参数，点「一键开始任务」重新生成文件'
  runResult.value = null
  window.scrollTo({ top: 0, behavior: 'smooth' })
}

async function deleteTask(row) {
  error.value = ''
  message.value = ''
  if (!window.confirm(`确定删除「${row.customerName} ${periodLabel(row)}」这条任务记录？\n（只删统计记录，不影响已生成的文件）`)) return
  try {
    const resp = await fetch('/api/delete-bill-task', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ id: row.id }),
    })
    const data = await resp.json()
    if (!resp.ok) {
      if (resp.status === 401) emit('unauthorized')
      error.value = data.error || `删除失败（${resp.status}）`
      return
    }
    message.value = '任务记录已删除'
    await loadTasks()
  } catch (err) {
    error.value = '删除失败：' + err.message
  }
}

const copyRef = ref(null)

async function copyCostSummary() {
  const text = runResult.value && runResult.value.costSummary
  if (!text) return
  try {
    if (navigator.clipboard && window.isSecureContext) {
      await navigator.clipboard.writeText(text)
      copyState.value = 'ok'
      setTimeout(() => { copyState.value = '' }, 2000)
      return
    }
  } catch (err) {
    // 落到下面的手动选中路径
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

function periodLabel(row) {
  return `${row.periodYear}-${String(row.periodMonth).padStart(2, '0')}`
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
      选客户 + 账期，一键导出该客户的账单、脱敏日志与成本利润表，
      同时把账期、结算额、成本、利润记录到本地数据库，供下方按月汇总。
    </p>
    <p class="hint">
      生成的文件只保留 {{ jobRetentionHours }} 小时，过期后自动清理——
      没来得及下载就点该行的「重新执行」，数字记录不受影响。
    </p>

    <!-- 默认出账参数 -->
    <div class="section-head">
      <strong>默认出账参数</strong>
      <button type="button" class="btn-link" @click="showSettings = !showSettings">
        {{ showSettings ? '收起' : '展开修改' }}
      </button>
      <span class="hint inline" v-if="settings">
        当前：单价来源 {{ settings.priceSource }} ｜ 汇率 {{ settings.exchangeRate }} ｜
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
      <p class="hint">
        单价来源选「数据库实时价格」前，需先在「生成账单」页点过「拉取最新数据库价格」。
      </p>
    </div>

    <!-- 执行任务 -->
    <div class="run-box">
      <div class="form-grid">
        <label>
          <span>客户</span>
          <select v-model.number="runForm.customerId">
            <option :value="0" disabled>请选择客户</option>
            <option v-for="c in customers" :key="c.id" :value="c.id">
              {{ c.name }}（{{ (c.usernames || '').split(/[\n\r,，;；\t ]+/).filter(Boolean).length }} 个账号）
            </option>
          </select>
        </label>
        <label>
          <span>账期月份</span>
          <input v-model="runForm.period" type="month" />
        </label>
      </div>
      <div class="checkboxes">
        <label><input v-model="runForm.generateSanitized" type="checkbox" /> 生成脱敏日志</label>
        <label><input v-model="runForm.generateCost" type="checkbox" /> 生成成本利润表</label>
      </div>
      <div class="path-row">
        <button type="button" class="btn-primary" @click="runTask" :disabled="running">
          {{ running ? '任务执行中…' : '一键开始任务' }}
        </button>
        <span class="hint inline" v-if="running">
          正在导出日志并出账，日志量大时可能需要几十秒，请不要关闭页面。
        </span>
      </div>
      <p class="hint" v-if="!hasCustomers">
        还没有客户，请先到「客户信息」页新增客户并填好业务库账号。
      </p>
    </div>

    <p class="error" v-if="error">{{ error }}</p>
    <span class="hint" v-if="message">{{ message }}</span>

    <!-- 执行结果 -->
    <div v-if="runResult" class="run-result">
      <div class="downloads">
        <a class="btn" :href="runResult.billUrl">下载账单：{{ runResult.billFileName }}</a>
        <a class="btn" v-if="runResult.sanitizedUrl" :href="runResult.sanitizedUrl">
          下载脱敏日志：{{ runResult.sanitizedFileName }}
        </a>
        <a class="btn" v-if="runResult.costUrl" :href="runResult.costUrl">
          下载成本利润表：{{ runResult.costFileName }}
        </a>
      </div>
      <p class="hint">
        以上文件只保留 {{ jobRetentionHours }} 小时，请及时下载。源日志：{{ runResult.logPath }}（{{
          fmtNum(runResult.logRowCount)
        }} 行）
      </p>

      <div v-if="runResult.costBlocked" class="cost-blocked">
        <p class="error">成本利润表未生成：有渠道还没维护上游倍率。账单已生成，补录后重新执行即可。</p>
        <p v-if="runResult.missingChannels && runResult.missingChannels.length > 0">
          <strong>待补录渠道</strong>：
          <span v-for="c in runResult.missingChannels" :key="c.channelId">{{ c.name }}（{{ c.channelId }}）</span>
        </p>
        <p class="hint">本次任务的成本与利润会留空记录，补齐渠道倍率后重新执行即可补上。</p>
      </div>

      <div v-if="runResult.costSummary" class="cost-summary">
        <div class="cost-summary-head">
          <strong>成本利润摘要</strong>
          <button type="button" class="btn-browse" @click="copyCostSummary">
            {{ copyState === 'ok' ? '已复制 ✓' : copyState === 'fail' ? '复制失败，请手动选中' : '复制' }}
          </button>
        </div>
        <pre ref="copyRef" class="cost-summary-text">{{ runResult.costSummary }}</pre>
      </div>
    </div>
  </div>

  <!-- 按月汇总 -->
  <div class="card">
    <h2>按月汇总</h2>
    <p class="hint">
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
            <span v-if="s.missingCostCount > 0" class="tag warn">{{ s.missingCostCount }} 个任务无成本</span>
            <span v-if="s.partialCostCount > 0" class="tag warn">{{ s.partialCostCount }} 个任务成本不全</span>
            <span v-if="s.missingCostCount === 0 && s.partialCostCount === 0" class="tag ok">成本完整</span>
          </td>
        </tr>
      </tbody>
    </table>
    <span class="hint" v-else-if="!loading">还没有任务记录，执行一次任务后这里会出现汇总。</span>

    <p class="hint" v-if="hasMissingCost">
      「无成本」表示该任务没有生成成本利润表（未勾选，或渠道倍率没维护被拦）；
      「成本不全」表示部分渠道没维护倍率、成本只覆盖了一部分行。
      这两种情况下的利润**只覆盖有成本的那部分**，不等于整体毛利。
    </p>

    <!-- 任务明细 -->
    <h3 v-if="tasks.length > 0">任务明细</h3>
    <table v-if="tasks.length > 0">
      <thead>
        <tr>
          <th>客户</th>
          <th>账期</th>
          <th>结算额(¥)</th>
          <th>成本(¥)</th>
          <th>利润(¥)</th>
          <th>日志行数</th>
          <th>生成时间</th>
          <th>操作</th>
        </tr>
      </thead>
      <tbody>
        <tr v-for="t in tasks" :key="t.id">
          <td>{{ t.customerName }}</td>
          <td>{{ periodLabel(t) }}</td>
          <td>{{ fmtMoney(t.settleCny) }}</td>
          <td>
            <span v-if="t.costCny === null || t.costCny === undefined" class="tag warn">未核算</span>
            <span v-else>{{ fmtMoney(t.costCny) }}</span>
          </td>
          <td :class="{ negative: t.profitCny !== null && t.profitCny < 0 }">
            {{ fmtMoney(t.profitCny) }}
          </td>
          <td>{{ fmtNum(t.rowCount) }}</td>
          <td>{{ fmtTime(t.generatedAt) }}</td>
          <td class="ops">
            <button type="button" class="btn-link" @click="refillRun(t)">重新执行</button>
            <button type="button" class="btn-link danger" @click="deleteTask(t)">删除</button>
          </td>
        </tr>
      </tbody>
    </table>
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
.card h3 {
  margin-bottom: 6px;
  font-size: 15px;
}

.hint {
  font-size: 12px;
  color: #888;
  display: block;
}
.hint.inline {
  display: inline;
  margin-left: 8px;
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

.settings-box {
  padding: 12px;
  border: 1px solid #e2e2e2;
  border-radius: 6px;
  background: #fafbfc;
  margin-bottom: 14px;
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

.run-box {
  padding: 12px;
  border: 1px solid #d6e2f7;
  border-radius: 6px;
  background: #f7faff;
  margin-top: 14px;
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

.run-result {
  margin-top: 14px;
  padding-top: 12px;
  border-top: 1px solid #eee;
}
.downloads {
  display: flex;
  gap: 10px;
  flex-wrap: wrap;
  margin-bottom: 8px;
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

.cost-blocked {
  margin-top: 10px;
  padding: 8px 10px;
  border: 1px solid #f0c36d;
  border-radius: 4px;
  background: #fffdf5;
}

.cost-summary {
  margin-top: 12px;
  padding: 10px 12px;
  border: 1px solid #d6e2f7;
  border-radius: 6px;
  background: #f7faff;
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
th:nth-child(1),
th:nth-child(2),
td:nth-child(1),
td:nth-child(2) {
  text-align: left;
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

.error {
  color: #d92626;
  margin-top: 8px;
}
</style>
