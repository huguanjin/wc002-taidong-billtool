<script setup>
import { computed, onMounted, ref } from 'vue'

// 台子管理：台子编号、地址、对接人、需求模型及对应价格、注意事项。
// 需求模型与价格成对维护（一行一个模型），不用一段自由文本，
// 这样每个模型的价格一眼能对上，也方便以后按模型检索。

const emit = defineEmits(['unauthorized'])

const stages = ref([])
const loaded = ref(false)
const loading = ref(false)
const saving = ref(false)
const deleting = ref(false)
const error = ref('')
const message = ref('')
const keyword = ref('')
// 表单弹窗：新增和编辑共用；表单内的错误单独显示，避免被弹窗遮住的列表区域吞掉。
const showForm = ref(false)
const formError = ref('')

const emptyDraft = () => ({ id: 0, address: '', contact: '', models: [{ model: '', price: '' }], notes: '' })
const draft = ref(emptyDraft())
const isEditing = computed(() => draft.value.id > 0)

const filtered = computed(() => {
  const k = keyword.value.trim().toLowerCase()
  if (!k) return stages.value
  return stages.value.filter((s) =>
    [String(s.id), s.address, s.contact, s.notes, ...s.models.map((m) => m.model)]
      .join('\n')
      .toLowerCase()
      .includes(k),
  )
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

async function loadStages() {
  error.value = ''
  loading.value = true
  try {
    const data = await api('/api/key-stages')
    stages.value = data.stages || []
    loaded.value = true
  } catch (err) {
    error.value = '读取失败：' + err.message
  } finally {
    loading.value = false
  }
}

function resetDraft() {
  draft.value = emptyDraft()
}

function openCreate() {
  message.value = ''
  formError.value = ''
  resetDraft()
  showForm.value = true
}

function closeForm() {
  showForm.value = false
  formError.value = ''
  resetDraft()
}

function startEdit(s) {
  message.value = ''
  formError.value = ''
  draft.value = {
    id: s.id,
    address: s.address,
    contact: s.contact || '',
    models: s.models.length > 0 ? s.models.map((m) => ({ ...m })) : [{ model: '', price: '' }],
    notes: s.notes || '',
  }
  showForm.value = true
}

function addModelRow() {
  draft.value.models.push({ model: '', price: '' })
}

function removeModelRow(i) {
  draft.value.models.splice(i, 1)
  if (draft.value.models.length === 0) addModelRow()
}

async function saveStage() {
  formError.value = ''
  if (!draft.value.address.trim()) {
    formError.value = '台子地址不能为空'
    return
  }
  saving.value = true
  try {
    await api('/api/key-stages', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(draft.value),
    })
    message.value = isEditing.value ? '已更新台子' : '已新增台子'
    closeForm()
    await loadStages()
  } catch (err) {
    formError.value = '保存失败：' + err.message
  } finally {
    saving.value = false
  }
}

async function deleteStage(s) {
  error.value = ''
  message.value = ''
  if (!window.confirm(`确定删除台子 #${s.id}（${s.address}）？删除后无法恢复。`)) return
  deleting.value = true
  try {
    await api('/api/delete-key-stage', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ id: s.id }),
    })
    message.value = `已删除台子 #${s.id}`
    await loadStages()
  } catch (err) {
    error.value = '删除失败：' + err.message
  } finally {
    deleting.value = false
  }
}

onMounted(loadStages)
defineExpose({ loadStages })
</script>

<template>
  <div class="card">
    <div class="toolbar">
      <button type="button" class="btn-primary" @click="openCreate">+ 新增台子</button>
      <button type="button" class="btn-browse" @click="loadStages" :disabled="loading">
        {{ loading ? '读取中…' : '刷新' }}
      </button>
      <input v-model="keyword" type="text" class="search" placeholder="搜索 ID / 地址 / 对接人 / 模型 / 注意事项" />
    </div>

    <p class="error" v-if="error">{{ error }}</p>
    <span class="hint" v-if="message">{{ message }}</span>

    <table v-if="filtered.length > 0">
      <thead>
        <tr>
          <th>台子ID</th>
          <th>台子地址</th>
          <th>对接人</th>
          <th>需求模型 / 价格</th>
          <th>注意事项</th>
          <th>操作</th>
        </tr>
      </thead>
      <tbody>
        <tr v-for="s in filtered" :key="s.id">
          <td>{{ s.id }}</td>
          <td class="wrap">{{ s.address }}</td>
          <td class="wrap pre">{{ s.contact }}</td>
          <td>
            <div v-for="(m, i) in s.models" :key="i" class="model-line">
              <span class="tag">{{ m.model }}</span>
              <span>{{ m.price }}</span>
            </div>
          </td>
          <td class="wrap pre">{{ s.notes }}</td>
          <td class="ops">
            <button type="button" class="btn-link" @click="startEdit(s)">编辑</button>
            <button type="button" class="btn-link danger" @click="deleteStage(s)" :disabled="deleting">删除</button>
          </td>
        </tr>
      </tbody>
    </table>
    <span class="hint" v-else-if="loaded && stages.length > 0">没有匹配的台子。</span>
    <span class="hint" v-else-if="!loading && loaded">还没有台子，点击上方「新增台子」添加。</span>
    <span class="hint" v-else-if="!loaded">正在读取台子列表…</span>

    <div class="modal-mask" v-if="showForm">
      <div class="modal" role="dialog" aria-modal="true">
        <div class="modal-head">
          <h3>{{ isEditing ? `编辑台子 #${draft.id}` : '新增台子' }}</h3>
          <button type="button" class="modal-close" @click="closeForm" aria-label="关闭">×</button>
        </div>

        <div class="modal-body">
          <div class="form-grid">
            <label class="full">
              <span>台子地址<em class="req">*</em></span>
              <input v-model="draft.address" type="text" placeholder="例如：https://example.com" />
            </label>
            <label class="full">
              <span>对接人信息</span>
              <textarea v-model="draft.contact" rows="2" placeholder="姓名、微信/QQ/电话等"></textarea>
            </label>
            <div class="full models-box">
              <span class="label">需求模型及对应价格</span>
              <div class="model-row" v-for="(m, i) in draft.models" :key="i">
                <input v-model="m.model" type="text" placeholder="模型，例如：claude-opus-4" />
                <input v-model="m.price" type="text" placeholder="价格，例如：0.8 元/刀" />
                <button type="button" class="btn-link danger" @click="removeModelRow(i)">删除</button>
              </div>
              <button type="button" class="btn-browse small" @click="addModelRow">+ 添加模型</button>
            </div>
            <label class="full">
              <span>注意事项</span>
              <textarea v-model="draft.notes" rows="3" placeholder="例如：结算周期、限制条件"></textarea>
            </label>
          </div>
          <p class="error" v-if="formError">{{ formError }}</p>
        </div>

        <div class="modal-foot">
          <button type="button" class="btn-browse" @click="closeForm" :disabled="saving">取消</button>
          <button type="button" class="btn-primary" @click="saveStage" :disabled="saving">
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
.form-grid {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(240px, 1fr));
  gap: 10px 16px;
  margin: 12px 0;
}
.form-grid label,
.models-box {
  display: flex;
  flex-direction: column;
  gap: 4px;
  font-size: 13px;
  color: #444;
}
.full {
  grid-column: 1 / -1;
}
.toolbar {
  display: flex;
  gap: 8px;
  align-items: center;
  flex-wrap: wrap;
  margin-bottom: 10px;
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
  max-width: 640px;
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
.req {
  color: #d92626;
  font-style: normal;
  margin-left: 2px;
}
input[type='text'],
textarea {
  padding: 5px 7px;
  border: 1px solid #ccc;
  border-radius: 4px;
  font-size: 13px;
  font-family: inherit;
  box-sizing: border-box;
}
textarea {
  resize: vertical;
}
.model-row {
  display: flex;
  gap: 8px;
  align-items: center;
  margin-bottom: 6px;
}
.model-row input {
  flex: 1;
  min-width: 0;
}
.model-line {
  margin-bottom: 4px;
}
.search {
  flex: 1;
  min-width: 220px;
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
.btn-browse.small {
  align-self: flex-start;
  padding: 3px 10px;
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
td.wrap {
  word-break: break-all;
}
td.pre {
  white-space: pre-wrap;
}
td.ops {
  white-space: nowrap;
}
.tag {
  display: inline-block;
  margin-right: 6px;
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
