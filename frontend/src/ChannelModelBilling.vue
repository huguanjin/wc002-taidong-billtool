<script setup>
import { computed, onMounted, ref } from 'vue'

// 上游计费方式维护：按「渠道 + 模型」记录上游是按次还是按量，按次的要填单次费用。
//
// 数据存在本地 PostgreSQL（channel_model_billing 表），账单任务勾了
// 「成本严格区分按次计费模型」时读取。这里有两个视角：
//   1. 已维护清单：库里现在存着什么，可改、可删、可手工新增；
//   2. 按日志核对：选一份已导出的日志，看它用到的「带按次迹象」的渠道 + 模型哪些还没维护，就地补录。
// 两处共用同一套草稿与保存逻辑（草稿键是「渠道号|模型」）。

const emit = defineEmits(['unauthorized'])

const MODE_LABEL = { none: '未维护', per_call: '按次', per_token: '按量' }

const items = ref([]) // 已维护清单
const loading = ref(false)
const error = ref('')
const message = ref('')
const saving = ref(false)

// 草稿：键 → { mode: '' | 'per_call' | 'per_token', fee: '' }
const draft = ref({})

// 手工新增
const adding = ref({ channelId: '', model: '', mode: 'per_call', fee: '' })

// 按日志核对
const logFiles = ref([])
const selectedLog = ref('')
const checking = ref(false)
const checkError = ref('')
const status = ref(null) // { items, pendingCount, totalRows }

function keyOf(it) {
  return `${it.channelId}|${it.model}`
}

// 行当前已存的方式：清单项用 mode；核对项用 status（none = 未维护 → 空串）。
// 以下函数都在渲染期调用，必须带兜底、不能抛异常。
function savedMode(it) {
  const m = it.mode ?? it.status
  return m === 'per_call' || m === 'per_token' ? m : ''
}

function draftOf(it) {
  return draft.value[keyOf(it)] || { mode: savedMode(it), fee: it.perCallCny ? String(it.perCallCny) : '' }
}

function setDraft(it, patch) {
  draft.value = { ...draft.value, [keyOf(it)]: { ...draftOf(it), ...patch } }
}

function feeNumber(d) {
  return Number(String(d.fee ?? '').trim())
}

function isDirty(it) {
  const d = draftOf(it)
  if (d.mode === '') return false
  if (d.mode !== savedMode(it)) return true
  return d.mode === 'per_call' && feeNumber(d) !== Number(it.perCallCny || 0)
}

// 清单与核对结果里是同一个 (渠道,模型) 时共享草稿，改一处另一处同步变化；
// 所以待保存数按键去重，与 saveItems 实际提交的条数一致。
const allRows = computed(() => [...items.value, ...(status.value?.items || [])])
const pendingCount = computed(() => {
  const seen = new Set()
  for (const it of allRows.value) if (isDirty(it)) seen.add(keyOf(it))
  return seen.size
})

// toPayload 把一行草稿整理成接口项；按次的单次费用必须大于 0（0 会被读成上游免费）。
function toPayload(it) {
  const d = draftOf(it)
  if (d.mode === 'per_call') {
    const fee = feeNumber(d)
    if (!Number.isFinite(fee) || fee <= 0) {
      return { error: `渠道 ${it.channelId} 的模型 ${it.model}：按次的单次费用必须是大于 0 的数字` }
    }
    return { item: { channelId: it.channelId, model: it.model, mode: 'per_call', perCallCny: fee } }
  }
  return { item: { channelId: it.channelId, model: it.model, mode: 'per_token', perCallCny: 0 } }
}

function saveItems() {
  const seen = new Set()
  const out = []
  for (const it of allRows.value) {
    const k = keyOf(it)
    if (seen.has(k) || !isDirty(it)) continue
    seen.add(k)
    const { item, error: err } = toPayload(it)
    if (err) return { error: err }
    out.push(item)
  }
  return { items: out }
}

async function post(url, body) {
  const resp = await fetch(url, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  })
  const data = await resp.json()
  if (!resp.ok) {
    if (resp.status === 401) emit('unauthorized')
    throw new Error(data.error || `请求失败（${resp.status}）`)
  }
  return data
}

async function loadItems() {
  loading.value = true
  try {
    const resp = await fetch('/api/channel-model-billing')
    const data = await resp.json()
    if (!resp.ok) {
      if (resp.status === 401) emit('unauthorized')
      error.value = data.error || `读取失败（${resp.status}）`
      return
    }
    items.value = data.items || []
  } catch (err) {
    error.value = '读取失败：' + err.message
  } finally {
    loading.value = false
  }
}

async function loadLogs() {
  try {
    const resp = await fetch('/api/data-logs')
    const data = await resp.json()
    if (resp.ok) logFiles.value = data.files || []
    else if (resp.status === 401) emit('unauthorized')
  } catch (err) {
    checkError.value = '读取日志列表失败：' + err.message
  }
}

async function checkLog() {
  checkError.value = ''
  status.value = null
  if (!selectedLog.value) {
    checkError.value = '请先选择一份日志'
    return
  }
  checking.value = true
  try {
    const resp = await fetch('/api/channel-model-billing-status?' + new URLSearchParams({ logPath: selectedLog.value }))
    const data = await resp.json()
    if (!resp.ok) {
      if (resp.status === 401) emit('unauthorized')
      checkError.value = data.error || `核对失败（${resp.status}）`
      return
    }
    status.value = data
  } catch (err) {
    checkError.value = '核对失败：' + err.message
  } finally {
    checking.value = false
  }
}

// reloadAfterChange 保存/删除后刷新清单，并在有核对结果时用同一份日志重新核对，
// 让「未维护」的数量立刻反映新状态。
async function reloadAfterChange() {
  draft.value = {}
  await loadItems()
  if (status.value && selectedLog.value) await checkLog()
}

async function save() {
  error.value = ''
  message.value = ''
  const { items: list, error: err } = saveItems()
  if (err) {
    error.value = err
    return
  }
  if (list.length === 0) return
  saving.value = true
  try {
    const data = await post('/api/save-channel-model-billing', { items: list })
    message.value = `已保存 ${data.saved} 项`
    await reloadAfterChange()
  } catch (err2) {
    error.value = '保存失败：' + err2.message
  } finally {
    saving.value = false
  }
}

async function remove(it) {
  if (!window.confirm(`删除渠道 ${it.channelId} 的模型 ${it.model} 的上游计费方式？\n（删除后，下次严格出账会再次要求维护）`)) return
  error.value = ''
  message.value = ''
  try {
    await post('/api/save-channel-model-billing', {
      items: [{ channelId: it.channelId, model: it.model, delete: true }],
    })
    message.value = '已删除'
    await reloadAfterChange()
  } catch (err) {
    error.value = '删除失败：' + err.message
  }
}

async function addOne() {
  error.value = ''
  message.value = ''
  const channelId = Number(String(adding.value.channelId ?? '').trim())
  const model = String(adding.value.model ?? '').trim()
  if (!Number.isInteger(channelId) || channelId <= 0 || model === '') {
    error.value = '请填写渠道号（正整数）与模型名'
    return
  }
  let item
  if (adding.value.mode === 'per_call') {
    const fee = Number(String(adding.value.fee ?? '').trim())
    if (!Number.isFinite(fee) || fee <= 0) {
      error.value = '按次的单次费用必须是大于 0 的数字'
      return
    }
    item = { channelId, model, mode: 'per_call', perCallCny: fee }
  } else {
    item = { channelId, model, mode: 'per_token', perCallCny: 0 }
  }
  saving.value = true
  try {
    await post('/api/save-channel-model-billing', { items: [item] })
    message.value = '已添加'
    adding.value = { channelId: '', model: '', mode: adding.value.mode, fee: '' }
    await reloadAfterChange()
  } catch (err) {
    error.value = '添加失败：' + err.message
  } finally {
    saving.value = false
  }
}

function money(v) {
  if (v === null || v === undefined) return '—'
  return '¥' + v
}

onMounted(() => {
  loadItems()
  loadLogs()
})

defineExpose({ loadItems })
</script>

<template>
  <div class="card">
    <h2>上游计费方式维护</h2>
    <p class="hint">
      账单任务勾了「成本严格区分按次计费模型」时，成本不再一律按上游倍率估算：
      每个「<strong>渠道 + 模型</strong>」要说明上游是按次还是按量收费。
      <strong>同一个模型在不同渠道要分别维护</strong>——例如 gpt-image-2 在渠道 A 每次 0.1、渠道 B 每次 0.2、渠道 C 按量。
    </p>
    <ul class="hint">
      <li><strong>按次</strong>：填上游每次调用的实际费用（额度值，即 quota ÷ 500000，数值上等于人民币）。
        该渠道上这个模型的每一行都按「调用次数 × 单次费用」估成本，不再要求该渠道有上游倍率。</li>
      <li><strong>按量</strong>：沿用「渠道成本倍率」页里该渠道的上游倍率估算。</li>
    </ul>
    <p class="hint">维护结果存在本地 PostgreSQL，任务执行时直接读取；这里改了，下次执行就按新的算。</p>

    <p class="error" v-if="error">{{ error }}</p>
    <span class="hint" v-if="message">{{ message }}</span>

    <!-- ① 按日志核对 -->
    <h3 class="sub-title">按日志核对</h3>
    <p class="hint">
      选一份已导出的日志，列出它用到的、带按次迹象（站内按次计价，或图片生成/编辑请求）以及已维护过的「渠道 + 模型」，
      <strong>未维护的排在最前</strong>，可直接在表里补录。「未维护」数为 0 就说明用这份日志严格出账不会再被拦下。
    </p>
    <div class="path-row">
      <select v-model="selectedLog">
        <option value="">选择一份已导出的日志</option>
        <option v-for="f in logFiles" :key="f.path" :value="f.path">{{ f.name }}</option>
      </select>
      <button type="button" class="btn-browse" @click="checkLog" :disabled="checking">
        {{ checking ? '核对中…' : '核对' }}
      </button>
      <button type="button" class="btn-browse" @click="save" :disabled="saving || pendingCount === 0">
        {{ saving ? '保存中…' : `保存${pendingCount > 0 ? `（${pendingCount}）` : ''}` }}
      </button>
    </div>
    <p class="error" v-if="checkError">{{ checkError }}</p>
    <template v-if="status">
      <span class="hint">
        共 {{ status.items.length }} 项，<strong :class="{ warn: status.pendingCount > 0 }">未维护 {{ status.pendingCount }} 项</strong>
        （日志 {{ status.totalRows }} 行）。
      </span>
      <table v-if="status.items.length > 0">
        <thead>
          <tr>
            <th>渠道</th>
            <th>模型</th>
            <th>分组</th>
            <th>次数</th>
            <th>站内金额</th>
            <th>站内每次均价</th>
            <th>状态</th>
            <th>上游计费方式</th>
            <th>单次费用（额度/次）</th>
            <th>按次估算成本</th>
          </tr>
        </thead>
        <tbody>
          <tr
            v-for="it in status.items"
            :key="'st-' + it.channelId + '|' + it.model"
            :class="{ 'row-missing': it.status === 'none', 'row-unsaved': isDirty(it) }"
          >
            <td>{{ it.channelId }} {{ it.channelName }}</td>
            <td class="left">{{ it.model }}</td>
            <td>{{ (it.groups || []).join('、') }}</td>
            <td>{{ it.units }}</td>
            <td>¥{{ it.amountCny }}</td>
            <td>{{ it.avgSiteCny ? it.avgSiteCny : '—' }}</td>
            <td>{{ MODE_LABEL[it.status] || it.status }}</td>
            <td>
              <select :value="draftOf(it).mode" @change="setDraft(it, { mode: $event.target.value })">
                <option value="">请选择</option>
                <option value="per_call">按次</option>
                <option value="per_token">按量</option>
              </select>
            </td>
            <td>
              <input
                v-if="draftOf(it).mode === 'per_call'"
                :value="draftOf(it).fee"
                @input="setDraft(it, { fee: $event.target.value })"
                type="number"
                step="0.0001"
                min="0"
                placeholder="必填"
                class="ratio-input"
              />
              <span v-else class="hint inline">—</span>
            </td>
            <td>{{ it.status === 'per_call' ? money(it.estCostCny) : '—' }}</td>
          </tr>
        </tbody>
      </table>
      <span class="hint" v-else>这份日志里没有带按次迹象的模型，也没有已维护项命中。</span>
    </template>

    <!-- ② 已维护清单 -->
    <h3 class="sub-title">已维护清单</h3>
    <div class="path-row">
      <input v-model="adding.channelId" type="number" min="1" placeholder="渠道号" class="ratio-input" />
      <input v-model="adding.model" type="text" placeholder="模型名，如 gpt-image-2" />
      <select v-model="adding.mode">
        <option value="per_call">按次</option>
        <option value="per_token">按量</option>
      </select>
      <input
        v-if="adding.mode === 'per_call'"
        v-model="adding.fee"
        type="number"
        step="0.0001"
        min="0"
        placeholder="单次费用（额度/次）"
        class="ratio-input"
      />
      <button type="button" class="btn-browse" @click="addOne" :disabled="saving">手工添加</button>
      <button type="button" class="btn-browse" @click="loadItems" :disabled="loading">
        {{ loading ? '读取中…' : '刷新' }}
      </button>
    </div>
    <span class="hint">日志里没有按次迹象、但你知道某渠道上游对某模型按次收费时，用「手工添加」指定。</span>
    <table v-if="items.length > 0">
      <thead>
        <tr>
          <th>渠道</th>
          <th>模型</th>
          <th>上游计费方式</th>
          <th>单次费用（额度/次）</th>
          <th>最后更新</th>
          <th>操作</th>
        </tr>
      </thead>
      <tbody>
        <tr v-for="it in items" :key="'it-' + it.channelId + '|' + it.model" :class="{ 'row-unsaved': isDirty(it) }">
          <td>{{ it.channelId }} {{ it.channelName }}</td>
          <td class="left">{{ it.model }}</td>
          <td>
            <select :value="draftOf(it).mode" @change="setDraft(it, { mode: $event.target.value })">
              <option value="per_call">按次</option>
              <option value="per_token">按量</option>
            </select>
          </td>
          <td>
            <input
              v-if="draftOf(it).mode === 'per_call'"
              :value="draftOf(it).fee"
              @input="setDraft(it, { fee: $event.target.value })"
              type="number"
              step="0.0001"
              min="0"
              class="ratio-input"
            />
            <span v-else class="hint inline">—</span>
          </td>
          <td>{{ it.updatedAt ? String(it.updatedAt).slice(0, 19).replace('T', ' ') : '—' }}</td>
          <td class="center"><button type="button" class="btn-browse" @click="remove(it)">删除</button></td>
        </tr>
      </tbody>
    </table>
    <span class="hint" v-else-if="!loading">还没有维护过。</span>
    <div class="path-row" v-if="items.length > 0">
      <button type="button" class="btn-browse" @click="save" :disabled="saving || pendingCount === 0">
        {{ saving ? '保存中…' : `保存修改${pendingCount > 0 ? `（${pendingCount}）` : ''}` }}
      </button>
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
ul.hint {
  margin: 4px 0;
  padding-left: 20px;
}
.path-row {
  display: flex;
  gap: 8px;
  align-items: center;
  flex-wrap: wrap;
  margin: 10px 0;
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
}
td.left {
  text-align: left;
}
td.center {
  text-align: center;
}
th {
  background: #fafbfc;
  font-weight: 600;
}
input,
select {
  padding: 4px 6px;
  border: 1px solid #ccc;
  border-radius: 4px;
  font-size: 13px;
  box-sizing: border-box;
}
.error {
  color: #d92626;
  margin-top: 8px;
}
.warn {
  color: #d92626;
}
.row-missing {
  background: #fff8e1;
}
.row-unsaved {
  background: #eef4ff;
}
.ratio-input {
  width: 110px;
}
.sub-title {
  margin: 24px 0 4px;
  font-size: 15px;
}
.hint.inline {
  display: inline;
}
</style>
