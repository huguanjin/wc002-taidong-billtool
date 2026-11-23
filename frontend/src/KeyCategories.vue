<script setup>
import { onMounted, ref } from 'vue'

// 资源类别管理：序号、类别名称、描述。台子管理里可多选绑定这些类别。

const emit = defineEmits(['unauthorized'])

const categories = ref([])
const loaded = ref(false)
const loading = ref(false)
const saving = ref(false)
const deleting = ref(false)
const error = ref('')
const message = ref('')
const showForm = ref(false)
const formError = ref('')

const emptyDraft = () => ({ id: 0, name: '', description: '' })
const draft = ref(emptyDraft())

async function api(url, options) {
  const resp = await fetch(url, options)
  const data = await resp.json().catch(() => ({}))
  if (!resp.ok) {
    if (resp.status === 401) emit('unauthorized')
    throw new Error(data.error || `请求失败（${resp.status}）`)
  }
  return data
}

async function loadCategories() {
  error.value = ''
  loading.value = true
  try {
    const data = await api('/api/key-categories')
    categories.value = data.categories || []
    loaded.value = true
  } catch (err) {
    error.value = '读取失败：' + err.message
  } finally {
    loading.value = false
  }
}

function openCreate() {
  message.value = ''
  formError.value = ''
  draft.value = emptyDraft()
  showForm.value = true
}

function startEdit(c) {
  message.value = ''
  formError.value = ''
  draft.value = { id: c.id, name: c.name, description: c.description || '' }
  showForm.value = true
}

function closeForm() {
  showForm.value = false
  formError.value = ''
  draft.value = emptyDraft()
}

async function saveCategory() {
  formError.value = ''
  if (!draft.value.name.trim()) {
    formError.value = '类别名称不能为空'
    return
  }
  saving.value = true
  try {
    await api('/api/key-categories', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(draft.value),
    })
    message.value = draft.value.id > 0 ? '已更新类别' : '已新增类别'
    closeForm()
    await loadCategories()
  } catch (err) {
    formError.value = '保存失败：' + err.message
  } finally {
    saving.value = false
  }
}

async function deleteCategory(c) {
  error.value = ''
  message.value = ''
  const n = c.stageCount || 0
  const warn = n > 0
    ? `确定删除类别「${c.name}」？\n\n有 ${n} 个台子绑定了它，删除后这些台子会自动解除该类别。`
    : `确定删除类别「${c.name}」？`
  if (!window.confirm(warn)) return
  deleting.value = true
  try {
    await api('/api/delete-key-category', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ id: c.id }),
    })
    message.value = `已删除类别「${c.name}」`
    await loadCategories()
  } catch (err) {
    error.value = '删除失败：' + err.message
  } finally {
    deleting.value = false
  }
}

onMounted(loadCategories)
defineExpose({ loadCategories })
</script>

<template>
  <div class="card">
    <div class="toolbar">
      <button type="button" class="btn-primary" @click="openCreate">+ 新增类别</button>
      <button type="button" class="btn-browse" @click="loadCategories" :disabled="loading">
        {{ loading ? '读取中…' : '刷新' }}
      </button>
    </div>

    <p class="error" v-if="error">{{ error }}</p>
    <span class="hint" v-if="message">{{ message }}</span>

    <table v-if="categories.length > 0">
      <thead>
        <tr>
          <th>资源序号</th>
          <th>类别名称</th>
          <th>描述</th>
          <th>绑定台子数</th>
          <th>操作</th>
        </tr>
      </thead>
      <tbody>
        <tr v-for="c in categories" :key="c.id">
          <td>{{ c.id }}</td>
          <td class="name">{{ c.name }}</td>
          <td class="wrap pre">{{ c.description }}</td>
          <td>{{ c.stageCount }}</td>
          <td class="ops">
            <button type="button" class="btn-link" @click="startEdit(c)">编辑</button>
            <button type="button" class="btn-link danger" @click="deleteCategory(c)" :disabled="deleting">删除</button>
          </td>
        </tr>
      </tbody>
    </table>
    <span class="hint" v-else-if="!loading && loaded">还没有资源类别，点击上方「新增类别」添加。</span>
    <span class="hint" v-else-if="!loaded">正在读取资源类别…</span>

    <div class="modal-mask" v-if="showForm">
      <div class="modal" role="dialog" aria-modal="true">
        <div class="modal-head">
          <h3>{{ draft.id > 0 ? `编辑类别 #${draft.id}` : '新增类别' }}</h3>
          <button type="button" class="modal-close" @click="closeForm" aria-label="关闭">×</button>
        </div>
        <div class="modal-body">
          <div class="form-grid">
            <label>
              <span>类别名称<em class="req">*</em></span>
              <input v-model="draft.name" type="text" placeholder="例如：GPT、Claude" />
            </label>
            <label>
              <span>描述</span>
              <textarea v-model="draft.description" rows="3"></textarea>
            </label>
          </div>
          <p class="error" v-if="formError">{{ formError }}</p>
        </div>
        <div class="modal-foot">
          <button type="button" class="btn-browse" @click="closeForm" :disabled="saving">取消</button>
          <button type="button" class="btn-primary" @click="saveCategory" :disabled="saving">
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
.form-grid {
  display: grid;
  gap: 10px;
  margin: 12px 0;
}
.form-grid label {
  display: flex;
  flex-direction: column;
  gap: 4px;
  font-size: 13px;
  color: #444;
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
  width: 100%;
}
textarea {
  resize: vertical;
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
table {
  width: 100%;
  border-collapse: collapse;
  font-size: 13px;
  margin-top: 8px;
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
td.name {
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
  max-width: 520px;
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
</style>
