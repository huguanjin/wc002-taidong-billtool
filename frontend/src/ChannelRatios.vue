<script setup>
import { computed, onMounted, ref } from 'vue'

// 渠道成本倍率维护：从业务库拉取渠道清单，人工为每个渠道维护上游分组倍率。
//
// 从 App.vue 拆出来独立成页：这张卡片有拉取/刷新/保存三组按钮、一张可编辑的宽表格，
// 和出账表单挤在同一页时两边都不好看。拆成组件后 App.vue 只保留页面切换。

const emit = defineEmits(['unauthorized'])

const channels = ref([])
const channelsLoaded = ref(false)
const loadingChannels = ref(false)
const pullingChannels = ref(false)
const savingChannels = ref(false)
const channelsError = ref('')
const channelsMessage = ref('')

// ratioDraft 渠道ID → 输入框里的倍率文本（空串表示未维护）。
const ratioDraft = ref({})
const ratioNotes = ref({})
// domesticDraft 渠道ID → 是否国模渠道的勾选草稿。
//
// 国模渠道承接的是站上按人民币报价的国产模型（站点充值 1 元 = 1 美金，1 倍率分组即原价），
// 它的上游倍率按「折扣」理解：0.4 就是 4 折。其余渠道按「每美金刊例的成本」理解：
// 折扣 = 倍率 ÷ 7，0.4 只是 0.57 折。同一个数差 7 倍，所以必须有这个标识。
const domesticDraft = ref({})
// 「折合官方折扣」的换算基数，由后端随清单带回（见 loadChannels），页面里不另写一个裸 7。
// 7 只是响应缺字段时的兜底。
const discountBaseFactor = ref(7)

// missingChannelCount / maintainedCount 读的是**服务端**的维护状态（c.upstreamRatio），
// 不是输入框里的草稿——否则一敲键盘「已维护」就涨上去，用户会以为已经存好了。
const missingChannelCount = computed(
  () => channels.value.filter((c) => c.upstreamRatio === null || c.upstreamRatio === undefined).length
)

const maintainedCount = computed(
  () => channels.value.filter((c) => c.upstreamRatio !== null && c.upstreamRatio !== undefined).length
)

// pendingCount 数的是「填了但还没保存」的渠道，与 saveChannelRatios 会提交的条数一致。
const pendingCount = computed(() => channels.value.filter((c) => isDirty(c)).length)

// isDirty 必须与 saveChannelRatios 的判据保持一致，否则按钮上的数字会和实际提交数对不上。
//
// 注意 v-model 绑在 type="number" 的输入框上时，Vue 会自动套 .number 修饰符，
// 值随用户输入在 string / number 之间变（空串仍是 ''）。所以统一 String() 归一化，
// 不能直接 .trim()——那会在用户输入数字的那一瞬间抛 TypeError。
// 本函数在模板渲染期被调用（:class），抛异常会让 Vue 卸载整个组件，表现为页面空白。
function isDirty(c) {
  const raw = String(ratioDraft.value[c.channelId] ?? '').trim()
  const before =
    c.upstreamRatio === null || c.upstreamRatio === undefined ? '' : String(c.upstreamRatio)
  // 数字归一化后再比，避免 "0.60" 与 0.6 被当成改动。
  const normalize = (v) => {
    const t = String(v ?? '').trim()
    if (t === '') return ''
    const n = Number(t)
    return Number.isFinite(n) ? String(n) : t
  }
  return (
    normalize(raw) !== normalize(before) ||
    (ratioNotes.value[c.channelId] || '') !== (c.note || '') ||
    // 国模标识也算改动：只改这个勾、不动倍率，同样要能保存。
    !!domesticDraft.value[c.channelId] !== !!c.isDomestic
  )
}

function syncChannelDraft(list) {
  const rd = {}
  const rn = {}
  const dd = {}
  for (const c of list) {
    rd[c.channelId] =
      c.upstreamRatio === null || c.upstreamRatio === undefined ? '' : String(c.upstreamRatio)
    rn[c.channelId] = c.note || ''
    dd[c.channelId] = !!c.isDomestic
  }
  ratioDraft.value = rd
  ratioNotes.value = rn
  domesticDraft.value = dd
}

// discountText 把当前草稿里的倍率折合成「官方人民币刊例的几折」，随输入实时变化。
// 这一列存在的理由就是让那 7 倍的差别肉眼可见：同样填 0.4，国模渠道是 4 折，海外渠道是 0.57 折。
// 在渲染期被调用，不能抛异常——取值一律 String() 归一并带兜底。
function discountText(c) {
  const raw = String(ratioDraft.value[c.channelId] ?? '').trim()
  if (raw === '') return '—'
  const num = Number(raw)
  if (!Number.isFinite(num)) return '—'
  const base = discountBaseFactor.value > 0 ? discountBaseFactor.value : 7
  const discount = domesticDraft.value[c.channelId] ? num : num / base
  return `${Math.round(discount * 1000) / 100} 折`
}

async function loadChannels() {
  channelsError.value = ''
  loadingChannels.value = true
  try {
    const resp = await fetch('/api/channels')
    const data = await resp.json()
    if (!resp.ok) {
      if (resp.status === 401) emit('unauthorized')
      channelsError.value = data.error || `读取失败（${resp.status}）`
      return
    }
    channels.value = data.channels || []
    if (Number(data.discountBaseFactor) > 0) discountBaseFactor.value = Number(data.discountBaseFactor)
    channelsLoaded.value = true
    syncChannelDraft(channels.value)
  } catch (err) {
    channelsError.value = '读取失败：' + err.message
  } finally {
    loadingChannels.value = false
  }
}

async function pullChannels() {
  channelsError.value = ''
  channelsMessage.value = ''
  pullingChannels.value = true
  try {
    const resp = await fetch('/api/pull-channels', { method: 'POST' })
    const data = await resp.json()
    if (!resp.ok) {
      if (resp.status === 401) emit('unauthorized')
      channelsError.value = data.error || `拉取失败（${resp.status}）`
      return
    }
    channels.value = data.channels || []
    channelsLoaded.value = true
    syncChannelDraft(channels.value)
    channelsMessage.value = `已拉取 ${data.pulled} 个渠道。`
  } catch (err) {
    channelsError.value = '拉取失败：' + err.message
  } finally {
    pullingChannels.value = false
  }
}

// 只提交「有改动」的项：把整个清单发回去会把没碰过的渠道也写成当前值，
// 万一别处改过就被这里覆盖了。
async function saveChannelRatios() {
  channelsError.value = ''
  channelsMessage.value = ''
  const items = []
  for (const c of channels.value) {
    // 同 isDirty：type=number 的 v-model 会给到 number，必须 String() 归一化后再 trim。
    const raw = String(ratioDraft.value[c.channelId] ?? '').trim()
    const noteBefore = c.note || ''
    const noteNow = ratioNotes.value[c.channelId] || ''
    // 用 isDirty 统一判据，免得这里的比较与按钮上的计数漂移。
    if (!isDirty(c)) continue

    // 国模标识每条都带上（显式 true / false）：后端把没传的当作「保持原值」，
    // 而这里的草稿初值就取自库里，带上不会改到没碰过的渠道。
    const isDomestic = !!domesticDraft.value[c.channelId]
    if (raw === '') {
      // 清空表示「取消维护」，发 null。
      items.push({ channelId: c.channelId, upstreamRatio: null, note: noteNow, isDomestic })
      continue
    }
    const num = Number(raw)
    if (!Number.isFinite(num) || num < 0) {
      channelsError.value = `渠道 ${c.channelId} 的倍率必须是非负数字`
      return
    }
    items.push({ channelId: c.channelId, upstreamRatio: num, note: noteNow, isDomestic })
  }

  if (items.length === 0) {
    channelsMessage.value = '没有改动需要保存。'
    return
  }

  savingChannels.value = true
  try {
    const resp = await fetch('/api/channel-ratios', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ items }),
    })
    const data = await resp.json()
    if (!resp.ok) {
      if (resp.status === 401) emit('unauthorized')
      channelsError.value = data.error || `保存失败（${resp.status}）`
      return
    }
    channelsMessage.value = `已保存 ${data.saved} 个渠道的倍率。`
    await loadChannels()
  } catch (err) {
    channelsError.value = '保存失败：' + err.message
  } finally {
    savingChannels.value = false
  }
}

// ---- 按次计费模型的上游计费方式（任务勾了「成本严格区分按次计费模型」时才会用到）----
//
// 平时由任务被拦下时就地补录；这里用来回看、改单次费用、或删掉重填。
// 草稿键是「渠道号|模型」，值是 { mode, fee }。
const perCallItems = ref([])
const perCallDraft = ref({})
const perCallError = ref('')
const perCallMessage = ref('')
const savingPerCall = ref(false)

// 手工添加：日志里没有按次迹象、但你知道某渠道上游对某模型按次收费时，直接在这里指定。
const newPerCall = ref({ channelId: '', model: '', mode: 'per_call', fee: '' })

async function addPerCall() {
  perCallError.value = ''
  perCallMessage.value = ''
  const channelId = Number(String(newPerCall.value.channelId ?? '').trim())
  const model = String(newPerCall.value.model ?? '').trim()
  if (!Number.isInteger(channelId) || channelId <= 0 || model === '') {
    perCallError.value = '请填写渠道号（正整数）与模型名'
    return
  }
  let item
  if (newPerCall.value.mode === 'per_call') {
    const fee = Number(String(newPerCall.value.fee ?? '').trim())
    if (!Number.isFinite(fee) || fee <= 0) {
      perCallError.value = '按次的单次费用必须是大于 0 的数字'
      return
    }
    item = { channelId, model, mode: 'per_call', perCallCny: fee }
  } else {
    item = { channelId, model, mode: 'per_token', perCallCny: 0 }
  }
  savingPerCall.value = true
  try {
    const resp = await fetch('/api/save-channel-model-billing', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ items: [item] }),
    })
    const data = await resp.json()
    if (!resp.ok) {
      if (resp.status === 401) emit('unauthorized')
      perCallError.value = data.error || `添加失败（${resp.status}）`
      return
    }
    perCallMessage.value = '已添加'
    newPerCall.value = { channelId: '', model: '', mode: newPerCall.value.mode, fee: '' }
    await loadPerCall()
  } catch (err) {
    perCallError.value = '添加失败：' + err.message
  } finally {
    savingPerCall.value = false
  }
}

function perCallKey(it) {
  return `${it.channelId}|${it.model}`
}

// 渲染期调用，必须带兜底、不能抛异常。
function perCallDraftOf(it) {
  return perCallDraft.value[perCallKey(it)] || { mode: it.mode || 'per_token', fee: String(it.perCallCny ?? '') }
}

function setPerCall(it, patch) {
  perCallDraft.value = { ...perCallDraft.value, [perCallKey(it)]: { ...perCallDraftOf(it), ...patch } }
}

function perCallDirty(it) {
  const d = perCallDraftOf(it)
  if (d.mode !== it.mode) return true
  return d.mode === 'per_call' && Number(String(d.fee ?? '').trim()) !== Number(it.perCallCny)
}

const perCallPending = computed(() => perCallItems.value.filter((it) => perCallDirty(it)).length)

async function loadPerCall() {
  perCallError.value = ''
  try {
    const resp = await fetch('/api/channel-model-billing')
    const data = await resp.json()
    if (!resp.ok) {
      if (resp.status === 401) emit('unauthorized')
      perCallError.value = data.error || `读取失败（${resp.status}）`
      return
    }
    perCallItems.value = data.items || []
    perCallDraft.value = {}
  } catch (err) {
    perCallError.value = '读取失败：' + err.message
  }
}

// perCallSaveItems 只提交有改动的；按次的单次费用必须大于 0（0 会被读成上游免费）。
function perCallSaveItems() {
  const items = []
  for (const it of perCallItems.value) {
    if (!perCallDirty(it)) continue
    const d = perCallDraftOf(it)
    if (d.mode === 'per_call') {
      const fee = Number(String(d.fee ?? '').trim())
      if (!Number.isFinite(fee) || fee <= 0) {
        return { error: `渠道 ${it.channelId} 的模型 ${it.model}：按次的单次费用必须是大于 0 的数字` }
      }
      items.push({ channelId: it.channelId, model: it.model, mode: 'per_call', perCallCny: fee })
    } else {
      items.push({ channelId: it.channelId, model: it.model, mode: 'per_token', perCallCny: 0 })
    }
  }
  return { items }
}

async function savePerCall() {
  perCallError.value = ''
  perCallMessage.value = ''
  const { items, error } = perCallSaveItems()
  if (error) {
    perCallError.value = error
    return
  }
  if (items.length === 0) return
  savingPerCall.value = true
  try {
    const resp = await fetch('/api/save-channel-model-billing', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ items }),
    })
    const data = await resp.json()
    if (!resp.ok) {
      if (resp.status === 401) emit('unauthorized')
      perCallError.value = data.error || `保存失败（${resp.status}）`
      return
    }
    perCallMessage.value = `已保存 ${data.saved} 项`
    await loadPerCall()
  } catch (err) {
    perCallError.value = '保存失败：' + err.message
  } finally {
    savingPerCall.value = false
  }
}

async function deletePerCall(it) {
  if (!window.confirm(`删除渠道 ${it.channelId} 的模型 ${it.model} 的上游计费方式？\n（删除后，下次严格出账会再次要求维护）`)) return
  perCallError.value = ''
  perCallMessage.value = ''
  try {
    const resp = await fetch('/api/save-channel-model-billing', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ items: [{ channelId: it.channelId, model: it.model, delete: true }] }),
    })
    const data = await resp.json()
    if (!resp.ok) {
      if (resp.status === 401) emit('unauthorized')
      perCallError.value = data.error || `删除失败（${resp.status}）`
      return
    }
    perCallMessage.value = '已删除'
    await loadPerCall()
  } catch (err) {
    perCallError.value = '删除失败：' + err.message
  }
}

// 首次进入该页时自动读一次本地清单；没拉取过会提示去拉取。
onMounted(() => {
  loadChannels()
  loadPerCall()
})

// 从出账页跳过来时刷新一次，保证看到的是最新状态。
defineExpose({ loadChannels })
</script>

<template>
  <div class="card">
    <h2>渠道成本倍率维护</h2>
    <p class="hint">
      拉取业务库 channels 表的渠道清单，为每个渠道填一个上游倍率，并标明它是不是<strong>国模渠道</strong>。
      成本利润表按「官方刊例 × 上游折扣」估算上游成本，折扣怎么从倍率换算取决于这个标识：
    </p>
    <ul class="hint">
      <li><strong>国模渠道</strong>（承接站上按人民币报价的国产模型）：倍率就是折扣，<strong>0.4 = 4 折</strong>。
        站点充值 1 元 = 1 美金，国产模型 1 倍率分组即原价。</li>
      <li><strong>其余渠道</strong>：倍率是「每美金刊例的成本」，折扣 = 倍率 ÷ {{ discountBaseFactor }}，
        同样的 0.4 只是 <strong>0.57 折</strong>。</li>
    </ul>
    <p class="hint">
      同一个数差 {{ discountBaseFactor }} 倍，标错一个渠道成本就整个失真——「折合官方折扣」列会随你的修改实时变化，
      保存前请对一眼。只读业务库，倍率与标识只存在本地 PostgreSQL，不会回写。
    </p>
    <p class="hint">
      未维护倍率的渠道不会被估算——成本列留空并排除在合计之外，而不是按 0 算
      （那会让成本虚低、毛利虚高）。
    </p>

    <div class="path-row">
      <button type="button" class="btn-browse" @click="pullChannels" :disabled="pullingChannels">
        {{ pullingChannels ? '拉取中…' : '拉取渠道清单' }}
      </button>
      <button type="button" class="btn-browse" @click="loadChannels" :disabled="loadingChannels">
        {{ loadingChannels ? '读取中…' : '刷新' }}
      </button>
      <button
        type="button"
        class="btn-browse"
        @click="saveChannelRatios"
        :disabled="savingChannels || pendingCount === 0"
      >
        {{ savingChannels ? '保存中…' : `保存倍率${pendingCount > 0 ? `（${pendingCount}）` : ''}` }}
      </button>
    </div>
    <p class="error" v-if="channelsError">{{ channelsError }}</p>
    <span class="hint" v-if="channelsMessage">{{ channelsMessage }}</span>
    <span class="hint" v-if="pendingCount > 0">
      有 {{ pendingCount }} 个改动尚未保存，点「保存倍率」提交。
    </span>
    <span class="hint" v-if="channelsLoaded">
      共 {{ channels.length }} 个渠道，已维护 {{ maintainedCount }} 个<template
        v-if="missingChannelCount > 0"
        >，还有 {{ missingChannelCount }} 个未维护</template
      >。
    </span>

    <table v-if="channels.length > 0">
      <thead>
        <tr>
          <th>渠道 ID</th>
          <th>渠道名称</th>
          <th>类型</th>
          <th>状态</th>
          <th>上游倍率</th>
          <th>国模渠道</th>
          <th>折合官方折扣</th>
          <th>备注</th>
        </tr>
      </thead>
      <tbody>
        <tr
          v-for="c in channels"
          :key="c.channelId"
          :class="{ 'row-missing': !c.upstreamRatio, 'row-unsaved': isDirty(c) }"
        >
          <td>{{ c.channelId }}</td>
          <td>{{ c.name }}</td>
          <td>{{ c.channelType }}</td>
          <td>{{ c.status === 1 ? '启用' : c.status }}</td>
          <td>
            <input
              v-model="ratioDraft[c.channelId]"
              type="number"
              step="0.01"
              min="0"
              placeholder="未维护"
              class="ratio-input"
            />
          </td>
          <td class="center">
            <input v-model="domesticDraft[c.channelId]" type="checkbox" title="国模渠道：倍率按折扣理解（0.4 = 4 折）" />
          </td>
          <td>{{ discountText(c) }}</td>
          <td><input v-model="ratioNotes[c.channelId]" type="text" placeholder="可选" /></td>
        </tr>
      </tbody>
    </table>
    <span class="hint" v-else-if="!loadingChannels && channelsLoaded">
      本地还没有渠道清单，先点「拉取渠道清单」。
    </span>
    <span class="hint" v-else-if="!channelsLoaded">
      正在读取本地渠道清单…
    </span>

    <h3 class="sub-title">按次计费模型的上游计费方式</h3>
    <p class="hint">
      账单任务勾了「成本严格区分按次计费模型」时，站内按次卖的模型需要在这里（或执行被拦下时就地）告诉我们上游怎么收费：
      <strong>按次</strong>填上游每次调用的实际费用（额度值，即 quota ÷ 500000，数值上等于人民币），成本 = 次数 × 单次费用；
      <strong>按量</strong>沿用上面的上游倍率估算。<strong>同一个模型在不同渠道要分别维护</strong>
      （例如渠道 A 每次 0.1、渠道 B 每次 0.2、渠道 C 按量）。维护成按次后，该渠道上这个模型的每一行都按次估算。
    </p>
    <div class="path-row">
      <input v-model="newPerCall.channelId" type="number" min="1" placeholder="渠道号" class="ratio-input" />
      <input v-model="newPerCall.model" type="text" placeholder="模型名，如 gpt-image-2" />
      <select v-model="newPerCall.mode">
        <option value="per_call">按次</option>
        <option value="per_token">按量</option>
      </select>
      <input
        v-if="newPerCall.mode === 'per_call'"
        v-model="newPerCall.fee"
        type="number"
        step="0.0001"
        min="0"
        placeholder="单次费用（额度/次）"
        class="ratio-input"
      />
      <button type="button" class="btn-browse" @click="addPerCall" :disabled="savingPerCall">添加</button>
    </div>
    <div class="path-row">
      <button type="button" class="btn-browse" @click="savePerCall" :disabled="savingPerCall || perCallPending === 0">
        {{ savingPerCall ? '保存中…' : `保存${perCallPending > 0 ? `（${perCallPending}）` : ''}` }}
      </button>
    </div>
    <p class="error" v-if="perCallError">{{ perCallError }}</p>
    <span class="hint" v-if="perCallMessage">{{ perCallMessage }}</span>
    <table v-if="perCallItems.length > 0">
      <thead>
        <tr>
          <th>渠道</th>
          <th>模型</th>
          <th>上游计费方式</th>
          <th>单次费用（额度/次）</th>
          <th>操作</th>
        </tr>
      </thead>
      <tbody>
        <tr v-for="it in perCallItems" :key="it.channelId + '|' + it.model" :class="{ 'row-unsaved': perCallDirty(it) }">
          <td>{{ it.channelId }} {{ it.channelName }}</td>
          <td>{{ it.model }}</td>
          <td>
            <select :value="perCallDraftOf(it).mode" @change="setPerCall(it, { mode: $event.target.value })">
              <option value="per_call">按次</option>
              <option value="per_token">按量</option>
            </select>
          </td>
          <td>
            <input
              v-if="perCallDraftOf(it).mode === 'per_call'"
              :value="perCallDraftOf(it).fee"
              @input="setPerCall(it, { fee: $event.target.value })"
              type="number"
              step="0.0001"
              min="0"
              class="ratio-input"
            />
            <span v-else class="hint inline">—</span>
          </td>
          <td class="center"><button type="button" class="btn-browse" @click="deletePerCall(it)">删除</button></td>
        </tr>
      </tbody>
    </table>
    <span class="hint" v-else>还没有维护过。</span>
  </div>
</template>

<style scoped>
/* 这张卡片从 App.vue 拆出来独立成页，所以原来靠父组件 scoped 样式提供的观感
   （.card / table / .hint / .btn-browse 等）必须在本组件内重declare 一份——
   Vue 的 scoped 样式只对子组件的根元素生效，不会作用到它内部的节点。 */

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

.path-row {
  display: flex;
  gap: 8px;
  align-items: flex-start;
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

th {
  background: #fafbfc;
  font-weight: 600;
}

input {
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

/* 未维护倍率的行高亮，让需要补录的渠道一眼可见。 */
.row-missing {
  background: #fff8e1;
}

/* 填了但还没保存的行：与「从未维护」区分开。
   以前两者长得一样，用户填完以为已生效，实际一个字节都没提交。 */
.row-unsaved {
  background: #eef4ff;
}

.ratio-input {
  width: 100px;
}

td.center {
  text-align: center;
}

.sub-title {
  margin: 24px 0 4px;
  font-size: 15px;
}

/* 说明里的两条口径是并列的要点，不要被全局的 .hint 压成一行。 */
ul.hint {
  margin: 4px 0;
  padding-left: 20px;
}
</style>
