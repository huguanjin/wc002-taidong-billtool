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
function isDirty(c) {
  const raw = (ratioDraft.value[c.channelId] ?? '').trim()
  const before =
    c.upstreamRatio === null || c.upstreamRatio === undefined ? '' : String(c.upstreamRatio)
  const noteBefore = c.note || ''
  const noteNow = ratioNotes.value[c.channelId] || ''
  return raw !== before || noteNow !== noteBefore
}

function syncChannelDraft(list) {
  const rd = {}
  const rn = {}
  for (const c of list) {
    rd[c.channelId] =
      c.upstreamRatio === null || c.upstreamRatio === undefined ? '' : String(c.upstreamRatio)
    rn[c.channelId] = c.note || ''
  }
  ratioDraft.value = rd
  ratioNotes.value = rn
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
    const raw = (ratioDraft.value[c.channelId] ?? '').trim()
    const before =
      c.upstreamRatio === null || c.upstreamRatio === undefined ? '' : String(c.upstreamRatio)
    const noteBefore = c.note || ''
    const noteNow = ratioNotes.value[c.channelId] || ''
    if (raw === before && noteNow === noteBefore) continue

    if (raw === '') {
      // 清空表示「取消维护」，发 null。
      items.push({ channelId: c.channelId, upstreamRatio: null, note: noteNow })
      continue
    }
    const num = Number(raw)
    if (!Number.isFinite(num) || num < 0) {
      channelsError.value = `渠道 ${c.channelId} 的倍率必须是非负数字`
      return
    }
    items.push({ channelId: c.channelId, upstreamRatio: num, note: noteNow })
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

// 首次进入该页时自动读一次本地清单；没拉取过会提示去拉取。
onMounted(loadChannels)

// 从出账页跳过来时刷新一次，保证看到的是最新状态。
defineExpose({ loadChannels })
</script>

<template>
  <div class="card">
    <h2>渠道成本倍率维护</h2>
    <p class="hint">
      拉取业务库 channels 表的渠道清单，为每个渠道填一个上游分组倍率。
      成本表按「官方刊例 × (上游倍率 ÷ 7)」估算上游成本，与站内折扣同一套换算基准。
      只读业务库，倍率只存在本地 PostgreSQL，不会回写。
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
</style>
