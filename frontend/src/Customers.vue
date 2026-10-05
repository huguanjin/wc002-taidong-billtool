<script setup>
import { computed, onMounted, ref } from 'vue'

// 客户信息维护：客户名称 + 名下的业务库账号列表。
//
// 这张表是「一键出账任务」的前置：任务只需要选客户，账号列表从这里取，
// 不用每次出账再从别处翻一串 username 手敲。
//
// 从 App.vue 拆成独立组件（同 ChannelRatios.vue 的做法）：页面切换仍靠
// App.vue 的 activePage，这个组件只管自己这块。

const emit = defineEmits(['unauthorized'])

const customers = ref([])
const loaded = ref(false)
const loading = ref(false)
const saving = ref(false)
const deleting = ref(false)
const error = ref('')
const message = ref('')

// 编辑态表单。id 为 0/空表示新增。
const draft = ref({ id: 0, name: '', usernames: '', note: '' })
const editingID = ref(0)

const isEditing = computed(() => editingID.value > 0)

// 账号个数实时显示：用户填完能立刻确认切出来是几个，而不是保存后才发现分隔符写错。
const draftAccountCount = computed(() => parseAccounts(draft.value.usernames).length)

// parseAccounts 与后端 billing.SplitAccountList 同一套分隔符口径
// （换行 / 逗号 / 分号 / 空白），去重保序。
function parseAccounts(raw) {
  const parts = String(raw || '').split(/[\n\r,，;；\t ]+/)
  const seen = new Set()
  const out = []
  for (const p of parts) {
    const t = p.trim()
    if (!t || seen.has(t)) continue
    seen.add(t)
    out.push(t)
  }
  return out
}

function accountList(c) {
  return parseAccounts(c.usernames)
}

async function loadCustomers() {
  error.value = ''
  loading.value = true
  try {
    const resp = await fetch('/api/customers')
    const data = await resp.json()
    if (!resp.ok) {
      if (resp.status === 401) emit('unauthorized')
      error.value = data.error || `读取失败（${resp.status}）`
      return
    }
    customers.value = data.customers || []
    loaded.value = true
  } catch (err) {
    error.value = '读取失败：' + err.message
  } finally {
    loading.value = false
  }
}

function resetDraft() {
  draft.value = { id: 0, name: '', usernames: '', note: '' }
  editingID.value = 0
}

function startEdit(c) {
  error.value = ''
  message.value = ''
  draft.value = { id: c.id, name: c.name, usernames: c.usernames || '', note: c.note || '' }
  editingID.value = c.id
}

async function saveCustomer() {
  error.value = ''
  message.value = ''
  const name = draft.value.name.trim()
  if (!name) {
    error.value = '客户名称不能为空'
    return
  }
  if (parseAccounts(draft.value.usernames).length === 0) {
    error.value = '至少要填一个业务库账号，否则出账任务查不到该客户的日志'
    return
  }

  saving.value = true
  try {
    const resp = await fetch('/api/customers', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        id: draft.value.id || 0,
        name,
        usernames: draft.value.usernames,
        note: draft.value.note,
      }),
    })
    const data = await resp.json()
    if (!resp.ok) {
      if (resp.status === 401) emit('unauthorized')
      error.value = data.error || `保存失败（${resp.status}）`
      return
    }
    message.value = isEditing.value ? `已更新客户「${name}」` : `已新增客户「${name}」`
    resetDraft()
    await loadCustomers()
  } catch (err) {
    error.value = '保存失败：' + err.message
  } finally {
    saving.value = false
  }
}

// 删除客户会连带删掉他的历史账单任务（数据库上是 ON DELETE CASCADE）。
// 所以确认框必须把条数说清楚，不能让人无感地删掉历史。
async function deleteCustomer(c) {
  error.value = ''
  message.value = ''
  const n = c.taskCount || 0
  const warn = n > 0
    ? `确定删除客户「${c.name}」？\n\n该客户有 ${n} 条历史账单任务记录，会一并删除且无法恢复。\n（账单文件本身不受影响）`
    : `确定删除客户「${c.name}」？`
  if (!window.confirm(warn)) return

  deleting.value = true
  try {
    const resp = await fetch('/api/delete-customer', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ id: c.id }),
    })
    const data = await resp.json()
    if (!resp.ok) {
      if (resp.status === 401) emit('unauthorized')
      error.value = data.error || `删除失败（${resp.status}）`
      return
    }
    message.value = `已删除客户「${c.name}」`
    if (editingID.value === c.id) resetDraft()
    await loadCustomers()
  } catch (err) {
    error.value = '删除失败：' + err.message
  } finally {
    deleting.value = false
  }
}

onMounted(loadCustomers)

// 从别处切过来时刷新一次，保证看到的是最新状态。
defineExpose({ loadCustomers })
</script>

<template>
  <div class="card">
    <h2>客户信息维护</h2>
    <p class="hint">
      维护客户名称与其名下的业务库账号。账单任务只需选客户，账号列表从这里取，
      不用每次出账再手敲一串用户名。
      账号可换行、逗号或空格分隔，重复的会自动去掉。
    </p>

    <div class="form-grid">
      <label>
        <span>客户名称</span>
        <input v-model="draft.name" type="text" placeholder="例如：示例科技" />
      </label>
      <label class="full">
        <span>
          业务库账号
          <em class="count" v-if="draftAccountCount > 0">已识别 {{ draftAccountCount }} 个</em>
        </span>
        <textarea
          v-model="draft.usernames"
          rows="3"
          placeholder="一行一个，或用逗号分隔，例如：&#10;a37836323&#10;test02"
        ></textarea>
      </label>
      <label class="full">
        <span>备注（可选）</span>
        <input v-model="draft.note" type="text" placeholder="例如：对接人、合同编号" />
      </label>
    </div>

    <div class="path-row">
      <button type="button" class="btn-primary" @click="saveCustomer" :disabled="saving">
        {{ saving ? '保存中…' : isEditing ? '保存修改' : '新增客户' }}
      </button>
      <button type="button" class="btn-browse" @click="resetDraft" v-if="isEditing">
        取消编辑
      </button>
      <button type="button" class="btn-browse" @click="loadCustomers" :disabled="loading">
        {{ loading ? '读取中…' : '刷新' }}
      </button>
    </div>

    <p class="error" v-if="error">{{ error }}</p>
    <span class="hint" v-if="message">{{ message }}</span>

    <table v-if="customers.length > 0">
      <thead>
        <tr>
          <th>客户名称</th>
          <th>业务库账号</th>
          <th>备注</th>
          <th>账单任务</th>
          <th>操作</th>
        </tr>
      </thead>
      <tbody>
        <tr v-for="c in customers" :key="c.id" :class="{ 'row-editing': editingID === c.id }">
          <td class="name">{{ c.name }}</td>
          <td class="accounts">
            <span v-for="u in accountList(c)" :key="u" class="tag">{{ u }}</span>
            <span v-if="accountList(c).length === 0" class="hint">未配置账号</span>
          </td>
          <td>{{ c.note }}</td>
          <td>{{ c.taskCount || 0 }}</td>
          <td class="ops">
            <button type="button" class="btn-link" @click="startEdit(c)">编辑</button>
            <button type="button" class="btn-link danger" @click="deleteCustomer(c)" :disabled="deleting">
              删除
            </button>
          </td>
        </tr>
      </tbody>
    </table>
    <span class="hint" v-else-if="!loading && loaded">
      还没有客户，先在上面新增一个。
    </span>
    <span class="hint" v-else-if="!loaded">正在读取客户列表…</span>
  </div>
</template>

<style scoped>
/* 本组件自带一份样式副本：Vue 的 scoped 样式只作用于子组件根元素，
   不会穿透到内部节点，所以父组件 App.vue 里定义的 .card / table / .hint
   在这个组件的内部节点上都不生效。 */

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

.form-grid {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(240px, 1fr));
  gap: 10px 16px;
  margin: 12px 0;
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
.form-grid textarea {
  padding: 5px 7px;
  border: 1px solid #ccc;
  border-radius: 4px;
  font-size: 13px;
  font-family: inherit;
  box-sizing: border-box;
}
.form-grid textarea {
  resize: vertical;
}
.count {
  font-style: normal;
  color: #2c6ef2;
  margin-left: 6px;
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
  text-align: left;
  vertical-align: top;
}
th {
  background: #fafbfc;
  font-weight: 600;
}
td.name {
  font-weight: 600;
}
td.ops {
  white-space: nowrap;
}
.tag {
  display: inline-block;
  margin: 0 4px 4px 0;
  padding: 1px 7px;
  border-radius: 10px;
  background: #eef4ff;
  color: #2c6ef2;
  font-size: 12px;
}
.row-editing {
  background: #fff8e1;
}
.error {
  color: #d92626;
  margin-top: 8px;
}
</style>
