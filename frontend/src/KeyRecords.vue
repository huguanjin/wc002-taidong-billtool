<script setup>
import { computed, onMounted, ref } from 'vue'

// 上 key 记录管理：每条记录对应「某个台子下某个渠道」的消耗、成本与结算。
// 台子从台子管理里选（可按 ID / 地址 / 对接人 / 模型模糊搜索），选中后自动带出对接人。
// 金额类字段按「消耗 × 单价、金额 ÷ 汇率」自动预填，但可手工改；
// 之后再改消耗/单价/汇率时会重新计算。

const emit = defineEmits(['unauthorized'])

const records = ref([])
const stages = ref([])
const loaded = ref(false)
const loading = ref(false)
const saving = ref(false)
const deleting = ref(false)
const error = ref('')
const message = ref('')
const keyword = ref('')
const showForm = ref(false)
const formError = ref('')

const emptyDraft = () => ({
  id: 0,
  stageId: 0,
  stageAddress: '',
  stageContact: '',
  channelId: '',
  supplier: '',
  consumption: '',
  costUnitPrice: '',
  costCny: '',
  fxRate: '',
  costUsdt: '',
  stageUnitPrice: '',
  settleCny: '',
  settleUsdt: '',
  alive: '',
  notes: '',
})
const draft = ref(emptyDraft())
const isEditing = computed(() => draft.value.id > 0)

const NUM_FIELDS = [
  'consumption', 'costUnitPrice', 'costCny', 'fxRate', 'costUsdt',
  'stageUnitPrice', 'settleCny', 'settleUsdt',
]

const num = (v) => {
  const n = Number(v)
  return Number.isFinite(n) ? n : 0
}
const round4 = (n) => Math.round(n * 10000) / 10000
const fmt = (n) => String(round4(num(n)))
// 预填值为 0 时留空，避免输入框里出现一堆没意义的 0。
const prefill = (n) => (n > 0 ? String(round4(n)) : '')

// ---- 自动计算 ----

function recalcCost() {
  const cny = num(draft.value.consumption) * num(draft.value.costUnitPrice)
  draft.value.costCny = prefill(cny)
  recalcCostUsdt()
}
function recalcCostUsdt() {
  const fx = num(draft.value.fxRate)
  draft.value.costUsdt = fx > 0 ? prefill(num(draft.value.costCny) / fx) : ''
}
function recalcSettle() {
  const cny = num(draft.value.consumption) * num(draft.value.stageUnitPrice)
  draft.value.settleCny = prefill(cny)
  recalcSettleUsdt()
}
function recalcSettleUsdt() {
  const fx = num(draft.value.fxRate)
  draft.value.settleUsdt = fx > 0 ? prefill(num(draft.value.settleCny) / fx) : ''
}
function onConsumption() {
  recalcCost()
  recalcSettle()
}
function onFx() {
  recalcCostUsdt()
  recalcSettleUsdt()
}

// ---- 台子下拉（模糊搜索） ----

const stageQuery = ref('')
const stageOpen = ref(false)

const stageOptions = computed(() => {
  const k = stageQuery.value.trim().toLowerCase()
  const list = stages.value.filter((s) => {
    if (!k) return true
    return [String(s.id), s.address, s.contact, ...(s.models || []).map((m) => m.model)]
      .join('\n')
      .toLowerCase()
      .includes(k)
  })
  return list.slice(0, 50)
})

function onStageInput() {
  stageOpen.value = true
  // 重新输入即视为取消已选的台子，必须再从下拉里选一个。
  draft.value.stageId = 0
  draft.value.stageAddress = ''
}

function pickStage(s) {
  draft.value.stageId = s.id
  draft.value.stageAddress = s.address
  draft.value.stageContact = s.contact || ''
  stageQuery.value = s.address
  stageOpen.value = false
}

// ---- 列表 ----

const filtered = computed(() => {
  const k = keyword.value.trim().toLowerCase()
  if (!k) return records.value
  return records.value.filter((r) =>
    [String(r.id), r.stageAddress, r.stageContact, r.channelId, r.supplier, r.alive, r.notes]
      .join('\n')
      .toLowerCase()
      .includes(k),
  )
})

const totals = computed(() => {
  const t = { consumption: 0, costCny: 0, costUsdt: 0, settleCny: 0, settleUsdt: 0 }
  for (const r of filtered.value) {
    t.consumption += r.consumption
    t.costCny += r.costCny
    t.costUsdt += r.costUsdt
    t.settleCny += r.settleCny
    t.settleUsdt += r.settleUsdt
  }
  return t
})

async function api(url, options) {
  const resp = await fetch(url, options)
  const data = await resp.json().catch(() => ({}))
  if (!resp.ok) {
    if (resp.status === 401) emit('unauthorized')
    throw new Error(data.error || `请求失败（${resp.status}）`)
  }
  return data
}

async function loadRecords() {
  error.value = ''
  loading.value = true
  try {
    const data = await api('/api/key-records')
    records.value = data.records || []
    loaded.value = true
  } catch (err) {
    error.value = '读取失败：' + err.message
  } finally {
    loading.value = false
  }
}

async function loadStages() {
  try {
    const data = await api('/api/key-stages')
    stages.value = data.stages || []
  } catch (err) {
    formError.value = '读取台子列表失败：' + err.message
  }
}

function resetDraft() {
  draft.value = emptyDraft()
  stageQuery.value = ''
  stageOpen.value = false
}

function openCreate() {
  message.value = ''
  formError.value = ''
  resetDraft()
  showForm.value = true
  loadStages()
}

function closeForm() {
  showForm.value = false
  formError.value = ''
  resetDraft()
}

function startEdit(r) {
  message.value = ''
  formError.value = ''
  const d = emptyDraft()
  Object.assign(d, {
    id: r.id,
    stageId: r.stageId,
    stageAddress: r.stageAddress,
    stageContact: r.stageContact || '',
    channelId: r.channelId || '',
    supplier: r.supplier || '',
    alive: r.alive || '',
    notes: r.notes || '',
  })
  for (const f of NUM_FIELDS) d[f] = r[f] > 0 ? String(r[f]) : ''
  draft.value = d
  stageQuery.value = r.stageAddress
  stageOpen.value = false
  showForm.value = true
  loadStages()
}

async function saveRecord() {
  formError.value = ''
  if (!draft.value.stageAddress) {
    formError.value = '请从下拉列表中选择台子'
    return
  }
  const body = { ...draft.value }
  for (const f of NUM_FIELDS) {
    if (num(body[f]) < 0) {
      formError.value = '金额、单价、汇率、消耗都不能为负数'
      return
    }
    body[f] = num(body[f])
  }
  saving.value = true
  try {
    await api('/api/key-records', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    })
    message.value = isEditing.value ? '已更新记录' : '已新增记录'
    closeForm()
    await loadRecords()
  } catch (err) {
    formError.value = '保存失败：' + err.message
  } finally {
    saving.value = false
  }
}

async function deleteRecord(r) {
  error.value = ''
  message.value = ''
  if (!window.confirm(`确定删除记录 #${r.id}（${r.stageAddress} / 渠道 ${r.channelId || '-'}）？删除后无法恢复。`)) return
  deleting.value = true
  try {
    await api('/api/delete-key-record', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ id: r.id }),
    })
    message.value = `已删除记录 #${r.id}`
    await loadRecords()
  } catch (err) {
    error.value = '删除失败：' + err.message
  } finally {
    deleting.value = false
  }
}

onMounted(loadRecords)
defineExpose({ loadRecords })
</script>

<template>
  <div class="card">
    <div class="toolbar">
      <button type="button" class="btn-primary" @click="openCreate">+ 新建记录</button>
      <button type="button" class="btn-browse" @click="loadRecords" :disabled="loading">
        {{ loading ? '读取中…' : '刷新' }}
      </button>
      <input v-model="keyword" type="text" class="search" placeholder="搜索 序号 / 台子 / 对接人 / 渠道ID / 供应商 / 存活 / 备注" />
    </div>

    <p class="error" v-if="error">{{ error }}</p>
    <span class="hint" v-if="message">{{ message }}</span>

    <p class="totals" v-if="filtered.length > 0">
      共 {{ filtered.length }} 条　消耗 {{ fmt(totals.consumption) }} 刀　
      成本 ¥{{ fmt(totals.costCny) }} / {{ fmt(totals.costUsdt) }} U　
      结算 ¥{{ fmt(totals.settleCny) }} / {{ fmt(totals.settleUsdt) }} U
    </p>

    <div class="table-wrap" v-if="filtered.length > 0">
      <table>
        <thead>
          <tr>
            <th>序号</th>
            <th>台子地址</th>
            <th>台子对接人</th>
            <th>渠道ID</th>
            <th>资源供应商</th>
            <th>渠道最新消耗（刀）</th>
            <th>渠道成本单价（元/刀）</th>
            <th>渠道成本金额（元）</th>
            <th>U汇率（元/U）</th>
            <th>渠道成本金额（U）</th>
            <th>台子渠道单价（元/刀）</th>
            <th>台子结算金额（元）</th>
            <th>台子结算金额（U）</th>
            <th>渠道存活情况</th>
            <th>备注事项</th>
            <th>操作</th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="r in filtered" :key="r.id">
            <td>{{ r.id }}</td>
            <td class="wrap">{{ r.stageAddress }}</td>
            <td class="wrap pre">{{ r.stageContact }}</td>
            <td class="wrap">{{ r.channelId }}</td>
            <td class="wrap">{{ r.supplier }}</td>
            <td class="num">{{ fmt(r.consumption) }}</td>
            <td class="num">{{ fmt(r.costUnitPrice) }}</td>
            <td class="num">{{ fmt(r.costCny) }}</td>
            <td class="num">{{ fmt(r.fxRate) }}</td>
            <td class="num">{{ fmt(r.costUsdt) }}</td>
            <td class="num">{{ fmt(r.stageUnitPrice) }}</td>
            <td class="num">{{ fmt(r.settleCny) }}</td>
            <td class="num">{{ fmt(r.settleUsdt) }}</td>
            <td class="wrap">{{ r.alive }}</td>
            <td class="wrap pre">{{ r.notes }}</td>
            <td class="ops">
              <button type="button" class="btn-link" @click="startEdit(r)">编辑</button>
              <button type="button" class="btn-link danger" @click="deleteRecord(r)" :disabled="deleting">删除</button>
            </td>
          </tr>
        </tbody>
      </table>
    </div>
    <span class="hint" v-else-if="loaded && records.length > 0">没有匹配的记录。</span>
    <span class="hint" v-else-if="!loading && loaded">还没有记录，点击上方「新建记录」添加。</span>
    <span class="hint" v-else-if="!loaded">正在读取记录…</span>

    <div class="modal-mask" v-if="showForm">
      <div class="modal" role="dialog" aria-modal="true">
        <div class="modal-head">
          <h3>{{ isEditing ? `编辑记录 #${draft.id}` : '新建记录' }}</h3>
          <button type="button" class="modal-close" @click="closeForm" aria-label="关闭">×</button>
        </div>

        <div class="modal-body">
          <div class="form-grid">
            <div class="full combo">
              <span class="label">台子地址<em class="req">*</em>（输入关键字搜索台子管理里的记录）</span>
              <input
                v-model="stageQuery"
                type="text"
                placeholder="输入 ID / 地址 / 对接人 / 模型 搜索并选择"
                @input="onStageInput"
                @focus="stageOpen = true"
                @blur="stageOpen = false"
              />
              <ul class="combo-list" v-if="stageOpen">
                <li
                  v-for="s in stageOptions"
                  :key="s.id"
                  @mousedown.prevent="pickStage(s)"
                >
                  <span class="combo-id">#{{ s.id }}</span>
                  <span>{{ s.address }}</span>
                  <span class="combo-sub" v-if="s.contact">{{ s.contact }}</span>
                </li>
                <li class="combo-empty" v-if="stageOptions.length === 0">没有匹配的台子</li>
              </ul>
            </div>
            <label class="full">
              <span>台子对接人（选台子后自动带出，可改）</span>
              <input v-model="draft.stageContact" type="text" />
            </label>

            <label>
              <span>渠道ID</span>
              <input v-model="draft.channelId" type="text" />
            </label>
            <label>
              <span>资源供应商</span>
              <input v-model="draft.supplier" type="text" />
            </label>

            <label>
              <span>渠道最新消耗（刀）</span>
              <input v-model="draft.consumption" type="number" min="0" step="any" @input="onConsumption" />
            </label>
            <label>
              <span>U汇率（元/U）</span>
              <input v-model="draft.fxRate" type="number" min="0" step="any" @input="onFx" />
            </label>

            <label>
              <span>渠道成本单价（元/刀）</span>
              <input v-model="draft.costUnitPrice" type="number" min="0" step="any" @input="recalcCost" />
            </label>
            <label>
              <span>渠道成本金额（元）</span>
              <input v-model="draft.costCny" type="number" min="0" step="any" @input="recalcCostUsdt" />
            </label>
            <label>
              <span>渠道成本金额（U）</span>
              <input v-model="draft.costUsdt" type="number" min="0" step="any" />
            </label>
            <span></span>

            <label>
              <span>台子渠道单价（元/刀）</span>
              <input v-model="draft.stageUnitPrice" type="number" min="0" step="any" @input="recalcSettle" />
            </label>
            <label>
              <span>台子结算金额（元）</span>
              <input v-model="draft.settleCny" type="number" min="0" step="any" @input="recalcSettleUsdt" />
            </label>
            <label>
              <span>台子结算金额（U）</span>
              <input v-model="draft.settleUsdt" type="number" min="0" step="any" />
            </label>
            <span></span>

            <label class="full">
              <span>渠道存活情况</span>
              <input v-model="draft.alive" type="text" list="alive-options" placeholder="例如：存活 / 已封 / 额度耗尽" />
              <datalist id="alive-options">
                <option value="存活"></option>
                <option value="已封"></option>
                <option value="额度耗尽"></option>
              </datalist>
            </label>
            <label class="full">
              <span>备注事项</span>
              <textarea v-model="draft.notes" rows="3"></textarea>
            </label>
          </div>
          <p class="hint">金额会按「消耗 × 单价」「金额 ÷ 汇率」自动预填，也可以直接手改。</p>
          <p class="error" v-if="formError">{{ formError }}</p>
        </div>

        <div class="modal-foot">
          <button type="button" class="btn-browse" @click="closeForm" :disabled="saving">取消</button>
          <button type="button" class="btn-primary" @click="saveRecord" :disabled="saving">
            {{ saving ? '保存中…' : '保存' }}
          </button>
        </div>
      </div>
    </div>
  </div>
</template>

<style scoped>
.card {
  background: #fff;
  border: 1px solid #e2e2e2;
  border-radius: 8px;
  padding: 20px;
  margin-bottom: 20px;
}
.hint {
  font-size: 12px;
  color: #888;
  display: block;
}
.toolbar {
  display: flex;
  gap: 8px;
  align-items: center;
  flex-wrap: wrap;
  margin-bottom: 10px;
}
.search {
  flex: 1;
  min-width: 220px;
}
.totals {
  font-size: 13px;
  color: #444;
  background: #f6f9ff;
  border: 1px solid #dbe6fb;
  border-radius: 6px;
  padding: 6px 10px;
  margin: 8px 0;
}
.form-grid {
  display: grid;
  grid-template-columns: repeat(2, minmax(0, 1fr));
  gap: 10px 16px;
  margin: 12px 0 6px;
}
.form-grid label,
.combo {
  display: flex;
  flex-direction: column;
  gap: 4px;
  font-size: 13px;
  color: #444;
}
.full {
  grid-column: 1 / -1;
}
.req {
  color: #d92626;
  font-style: normal;
  margin-left: 2px;
}
input[type='text'],
input[type='number'],
textarea {
  padding: 5px 7px;
  border: 1px solid #ccc;
  border-radius: 4px;
  font-size: 13px;
  font-family: inherit;
  box-sizing: border-box;
  width: 100%;
}
textarea {
  resize: vertical;
}
.combo {
  position: relative;
}
.combo-list {
  position: absolute;
  top: 100%;
  left: 0;
  right: 0;
  z-index: 10;
  margin: 2px 0 0;
  padding: 4px 0;
  list-style: none;
  background: #fff;
  border: 1px solid #c7d7fb;
  border-radius: 6px;
  box-shadow: 0 6px 18px rgba(0, 0, 0, 0.12);
  max-height: 220px;
  overflow-y: auto;
}
.combo-list li {
  display: flex;
  gap: 8px;
  align-items: baseline;
  padding: 6px 10px;
  cursor: pointer;
  font-size: 13px;
}
.combo-list li:hover {
  background: #eaf1ff;
}
.combo-list li.combo-empty {
  color: #999;
  cursor: default;
}
.combo-list li.combo-empty:hover {
  background: none;
}
.combo-id {
  color: #2c6ef2;
  font-weight: 600;
}
.combo-sub {
  color: #888;
  font-size: 12px;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
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
.btn-browse:disabled,
.btn-primary:disabled,
.btn-link:disabled {
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
.table-wrap {
  overflow-x: auto;
  margin-top: 8px;
}
table {
  width: 100%;
  min-width: 1500px;
  border-collapse: collapse;
  font-size: 13px;
}
th,
td {
  border: 1px solid #e2e2e2;
  padding: 6px 8px;
  text-align: left;
  vertical-align: top;
}
th {
  background: #fafbfc;
  font-weight: 600;
  white-space: nowrap;
}
td.wrap {
  word-break: break-all;
}
td.pre {
  white-space: pre-wrap;
}
td.num {
  text-align: right;
  white-space: nowrap;
}
td.ops {
  white-space: nowrap;
}
.modal-mask {
  position: fixed;
  inset: 0;
  background: rgba(0, 0, 0, 0.45);
  display: flex;
  align-items: center;
  justify-content: center;
  padding: 24px;
  z-index: 1000;
}
.modal {
  background: #fff;
  border-radius: 10px;
  width: 100%;
  max-width: 720px;
  max-height: 90vh;
  display: flex;
  flex-direction: column;
  box-shadow: 0 10px 40px rgba(0, 0, 0, 0.25);
}
.modal-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: 14px 20px;
  border-bottom: 1px solid #eee;
}
.modal-head h3 {
  margin: 0;
  font-size: 16px;
}
.modal-close {
  background: none;
  border: none;
  font-size: 24px;
  line-height: 1;
  color: #888;
  cursor: pointer;
}
.modal-body {
  padding: 4px 20px 12px;
  overflow-y: auto;
}
.modal-foot {
  display: flex;
  justify-content: flex-end;
  gap: 8px;
  padding: 12px 20px;
  border-top: 1px solid #eee;
}
.error {
  color: #d92626;
  margin-top: 8px;
}
@media (max-width: 600px) {
  .form-grid {
    grid-template-columns: 1fr;
  }
}
</style>
