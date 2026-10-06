<script setup>
import { computed, onMounted, ref, watch } from 'vue'

// 客户折扣维护：针对某个客户的某个分组，手工指定结算折扣。
//
// 存在的理由：有些客户的折扣是线下临时谈的，没及时更新到 new-api 的分组倍率里，
// 出账时按倍率反推出来的折扣与报给客户的折扣对不上。这里按「客户 + 分组」存一份真值，
// 出账时覆盖反推（见后端 DiscountOverrides）。
//
// 页面上把「自动折扣」与「手工折扣」并列显示，就是为了让这个对不上能被一眼看见——
// 只显示一列的话，结算人员没有任何依据判断该不该动手填。

const emit = defineEmits(['unauthorized'])

const customers = ref([])
const logFiles = ref([])
const groups = ref([])
const saved = ref([])

const selectedCustomer = ref(0)
const selectedLog = ref('')

const loadingCustomers = ref(false)
const loadingLogs = ref(false)
const parsing = ref(false)
const saving = ref(false)
const error = ref('')
const message = ref('')

// discountDraft / noteDraft 分组名 → 输入框里的原文与备注。
const discountDraft = ref({})
const noteDraft = ref({})

const customerName = computed(() => {
  const c = customers.value.find((x) => x.id === selectedCustomer.value)
  return c ? c.name : ''
})

const maintainedCount = computed(() => groups.value.filter((g) => isDirty(g)).length)

// isDirty 与 saveDiscounts 的判据必须一致，否则按钮上的数字与实际提交数会对不上。
//
// 手工折扣输入框是文本框（要接受「6折」这种写法），v-model 给到的一律是 string，
// 但仍然统一 String() 归一化——空值可能是 undefined，直接 .trim() 会抛。
// 本函数在模板渲染期被调用（:class），一旦抛异常 Vue 会卸载整个组件，表现为页面空白。
function isDirty(g) {
  const now = String(discountDraft.value[g.groupKey] ?? '').trim()
  // 服务端给的 manualDiscount 是数字（0.45），回填时用户看到的是小数形式，
  // 所以拿数字比数字，避免「0.45」与「6折」被误判成改动而反复提示未保存。
  const before = g.manualDiscount === null || g.manualDiscount === undefined ? '' : String(g.manualDiscount)
  const noteNow = noteDraft.value[g.groupKey] || ''
  return now !== before || noteNow !== (g.note || '')
}

function syncDraft(list) {
  const dd = {}
  const nd = {}
  for (const g of list) {
    dd[g.groupKey] =
      g.manualDiscount === null || g.manualDiscount === undefined ? '' : String(g.manualDiscount)
    nd[g.groupKey] = g.note || ''
  }
  discountDraft.value = dd
  noteDraft.value = nd
}

async function loadCustomers() {
  loadingCustomers.value = true
  try {
    const resp = await fetch('/api/customers')
    const data = await resp.json()
    if (!resp.ok) {
      if (resp.status === 401) emit('unauthorized')
      error.value = data.error || `读取客户失败（${resp.status}）`
      return
    }
    customers.value = data.customers || []
  } catch (err) {
    error.value = '读取客户失败：' + err.message
  } finally {
    loadingCustomers.value = false
  }
}

async function loadLogFiles() {
  loadingLogs.value = true
  try {
    const resp = await fetch('/api/data-logs')
    const data = await resp.json()
    if (!resp.ok) {
      if (resp.status === 401) emit('unauthorized')
      error.value = data.error || `读取日志列表失败（${resp.status}）`
      return
    }
    logFiles.value = data.files || []
  } catch (err) {
    error.value = '读取日志列表失败：' + err.message
  } finally {
    loadingLogs.value = false
  }
}

// loadGroups 拉取某个客户的手工折扣，并在选了日志时解析出该日志里的分组。
//
// 解析放后端做：分组名要不要带倍率后缀、自动折扣怎么算，都必须与出账同源，
// 前端自己拼一份迟早与账单不一致。
async function loadGroups() {
  error.value = ''
  message.value = ''
  if (!selectedCustomer.value) {
    error.value = '请先选择一个客户'
    return
  }

  parsing.value = true
  try {
    const q = new URLSearchParams({ customerId: String(selectedCustomer.value) })
    if (selectedLog.value) q.set('logPath', selectedLog.value)
    const resp = await fetch('/api/group-discounts?' + q.toString())
    const data = await resp.json()
    if (!resp.ok) {
      if (resp.status === 401) emit('unauthorized')
      error.value = data.error || `解析失败（${resp.status}）`
      return
    }
    saved.value = data.saved || []
    const parsed = data.groups || []

    // 没选日志时，把**已维护的折扣**直接摆出来供二次修改。
    //
    // 这一步是补一个真实的缺口：解析分组依赖一份日志，而日志是临时文件
    // （dataDir 里的导出件会被清理）。日志没了，之前维护过的折扣在页面上
    // 就完全看不见，也就无从修改——其实数据好好存在库里。
    // 这类行的「自动折扣」「倍率」列留空（没有日志就算不出来），
    // 所以模板里对它们只显示手工折扣与备注两列。
    if (!selectedLog.value) {
      groups.value = saved.value.map((r) => ({
        groupKey: r.groupKey,
        displayGroup: r.groupKey,
        groupRatio: 0,
        autoDiscount: null,
        autoSource: '',
        models: [],
        rows: 0,
        manualDiscount: r.discount,
        note: r.note || '',
        savedOnly: true,
      }))
      syncDraft(groups.value)
      if (groups.value.length === 0) {
        message.value = '该客户还没有维护任何手工折扣，选一份日志解析出分组后即可填写。'
      } else {
        message.value = `已维护 ${groups.value.length} 条手工折扣，可直接修改；选一份日志可同时看到自动折扣作对照。`
      }
      return
    }

    groups.value = parsed
    syncDraft(groups.value)
  } catch (err) {
    error.value = '解析失败：' + err.message
  } finally {
    parsing.value = false
  }
}

// saveDiscounts 整表提交：引擎侧是「覆盖式保存」，清空某个输入框即撤销该分组的手工折扣。
//
// 折扣文本原样发给后端解析（6折 / 60% / 0.6 都行）——前端不自己算，
// 否则两边解析规则一旦有差，页面显示 0.6 而账单按 6 算，是最难发现的一类错。
async function saveDiscounts() {
  error.value = ''
  message.value = ''
  if (!selectedCustomer.value) {
    error.value = '请先选择一个客户'
    return
  }

  const items = groups.value.map((g) => ({
    groupKey: g.groupKey,
    // 空串 = 撤销；传 null 让后端走「撤销」分支，而不是当成折扣 0。
    discount: String(discountDraft.value[g.groupKey] ?? '').trim() || null,
    note: noteDraft.value[g.groupKey] || '',
  }))

  saving.value = true
  try {
    const resp = await fetch('/api/save-group-discounts', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ customerId: selectedCustomer.value, items }),
    })
    const data = await resp.json()
    if (!resp.ok) {
      if (resp.status === 401) emit('unauthorized')
      error.value = data.error || `保存失败（${resp.status}）`
      return
    }
    saved.value = data.saved || []
    message.value = `已保存 ${data.count} 条手工折扣。`
    // 重新解析一遍：让表格里的「手工折扣」列反映落库后的权威值，
    // 而不是用户刚敲进去的原文（比如填「6折」保存后应显示 0.6）。
    await loadGroups()
  } catch (err) {
    error.value = '保存失败：' + err.message
  } finally {
    saving.value = false
  }
}

function fmtDiscount(v) {
  if (v === null || v === undefined) return '—'
  return Number(v).toFixed(3)
}

// hasManual 看服务端存的现状（g.manualDiscount），不是输入框草稿——
// 行高亮的含义是「这个分组已经被人工干预过」，一敲键盘就变绿会让人以为已经存好了。
function hasManual(g) {
  return g.manualDiscount !== null && g.manualDiscount !== undefined
}

onMounted(() => {
  loadCustomers()
  loadLogFiles()
})

// 切换客户就拉一次已维护的折扣（不带日志）——用户打开页面第一件事通常是
// 「看看这个客户现在填的是什么」，不该要求他先找一份日志。
watch(selectedCustomer, (id) => {
  if (id > 0) loadGroups()
})
</script>

<template>
  <div class="card">
    <h2>客户折扣维护</h2>
    <p class="hint">
      有些客户的折扣是线下谈定的，没来得及更新到 new-api 的分组倍率里。那类客户的账单
      按倍率反推出来的折扣是错的，在这里按「客户 + 分组」填上实际谈定的折扣，
      出账时会用它覆盖自动反推。
    </p>
    <p class="hint">
      选择客户与一份覆盖该时段的日志后点「解析分组」，表格会列出该日志里出现过的分组，
      并把<strong>当前会自动算成的折扣</strong>与手工值并列显示，便于对照。
      折扣可填 <code>6折</code>、<code>60%</code> 或 <code>0.6</code>；清空表示恢复自动折扣。
    </p>

    <div class="path-row">
      <label class="field">
        客户
        <select v-model.number="selectedCustomer" :disabled="loadingCustomers">
          <option :value="0">请选择</option>
          <option v-for="c in customers" :key="c.id" :value="c.id">{{ c.name }}</option>
        </select>
      </label>
      <label class="field grow">
        日志
        <select v-model="selectedLog" :disabled="loadingLogs">
          <option value="">（可选）选择一份已导出的日志</option>
          <option v-for="f in logFiles" :key="f.path" :value="f.path">{{ f.name }}</option>
        </select>
      </label>
      <button type="button" class="btn-browse" @click="loadGroups" :disabled="parsing">
        {{ parsing ? '解析中…' : '解析分组' }}
      </button>
      <button
        type="button"
        class="btn-browse"
        @click="saveDiscounts"
        :disabled="saving || groups.length === 0"
      >
        {{ saving ? '保存中…' : `保存折扣${maintainedCount > 0 ? `（${maintainedCount}）` : ''}` }}
      </button>
    </div>

    <p class="error" v-if="error">{{ error }}</p>
    <span class="hint" v-if="message">{{ message }}</span>

    <table v-if="groups.length > 0">
      <thead>
        <tr>
          <th>分组</th>
          <th>倍率</th>
          <th>当前自动折扣</th>
          <th>折扣来源</th>
          <th>手工折扣</th>
          <th>备注</th>
          <th>模型</th>
        </tr>
      </thead>
      <tbody>
        <tr v-for="g in groups" :key="g.groupKey" :class="{ 'row-maintained': hasManual(g), 'row-unsaved': isDirty(g) }">
          <td class="left">{{ g.displayGroup }}</td>
          <td>{{ g.groupRatio || '—' }}</td>
          <!-- savedOnly（没选日志、只是把库里已维护的摆出来）时自动折扣算不出来：
               它依赖日志的分组倍率与金额。写明「选日志后可见」而不是显示一个 0 或 —，
               否则会被读成「这个分组自动折扣是 0」。 -->
          <td v-if="g.savedOnly" class="hint">选日志后可见</td>
          <td v-else>{{ fmtDiscount(g.autoDiscount) }}</td>
          <td class="left source">{{ g.savedOnly ? '—' : g.autoSource }}</td>
          <td>
            <input
              v-model="discountDraft[g.groupKey]"
              type="text"
              placeholder="如 6折 / 0.6"
              class="discount-input"
            />
          </td>
          <td><input v-model="noteDraft[g.groupKey]" type="text" placeholder="如 9月线下谈定" /></td>
          <td class="left models">{{ g.savedOnly ? '—' : g.models.join('、') }}</td>
        </tr>
      </tbody>
    </table>
    <span class="hint" v-else-if="!parsing && selectedCustomer && selectedLog">
      该日志里没有解析出分组，确认这份日志是否包含 group 列。
    </span>
    <span class="hint" v-else-if="!selectedCustomer">先选择一个客户。</span>
    <span class="hint" v-else-if="!selectedLog">
      上面是该客户**已维护的**手工折扣（直接读库，不需要日志，可直接改）。
      选一份日志后点「解析分组」，还能看到各分组的自动反推折扣作对照。
    </span>
  </div>
</template>

<style scoped>
/* 与其他页面组件一样，依赖的观感类必须在本组件内重声明一份：
   Vue 的 scoped 样式只对子组件根元素生效，不会作用到它内部的节点。 */

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
  align-items: flex-end;
  flex-wrap: wrap;
  margin: 12px 0;
}

.field {
  display: flex;
  flex-direction: column;
  gap: 4px;
  font-size: 12px;
  color: #666;
}

.field.grow {
  min-width: 320px;
  flex: 1;
}

select {
  padding: 6px 8px;
  border: 1px solid #ccc;
  border-radius: 4px;
  font-size: 13px;
  background: #fff;
  max-width: 100%;
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

th:first-child,
td.left {
  text-align: left;
}

td.source {
  color: #888;
  font-size: 12px;
}

td.models {
  max-width: 260px;
  overflow-wrap: anywhere;
}

input {
  padding: 4px 6px;
  border: 1px solid #ccc;
  border-radius: 4px;
  font-size: 13px;
  box-sizing: border-box;
  width: 100%;
}

.discount-input {
  min-width: 90px;
}

.error {
  color: #d92626;
  margin-top: 8px;
}

/* 已维护手工折扣的行：与「只有自动折扣」的行区分开，
   让结算人员一眼看出哪些分组已经被人工干预过。 */
.row-maintained {
  background: #f3fbf5;
}

/* 改了但还没保存的行。 */
.row-unsaved {
  background: #eef4ff;
}

code {
  background: #f5f5f5;
  padding: 1px 4px;
  border-radius: 3px;
  font-size: 12px;
}
</style>
