<script setup>
import { computed, onMounted, ref, watch } from 'vue'
import { copyText, selectElementText } from './clipboard'

// 上 key 货源整理：货源的增删改查，以及「把当前在售（未下架）资源整理成文字复制」。
// 复制内容的字段可勾选，勾选状态记在浏览器里，下次打开沿用。

const emit = defineEmits(['unauthorized'])

const sources = ref([])
const loaded = ref(false)
const loading = ref(false)
const saving = ref(false)
const deleting = ref(false)
const error = ref('')
const message = ref('')
const keyword = ref('')
// 列表筛选：active 在售 / delisted 已下架 / all 全部
const view = ref('active')
// 弹窗：新增/编辑表单、复制在售资源。表单内的错误单独显示在弹窗里。
const showForm = ref(false)
const formError = ref('')
const showCopy = ref(false)

function today() {
  const d = new Date()
  const p = (n) => String(n).padStart(2, '0')
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}`
}

const emptyDraft = () => ({
  id: 0,
  addedDate: today(),
  ownerName: '',
  resourceName: '',
  configDesc: '',
  unitPrice: '',
  ownerDemand: '',
  delisted: false,
})
const draft = ref(emptyDraft())
const isEditing = computed(() => draft.value.id > 0)

const activeSources = computed(() => sources.value.filter((s) => !s.delisted))

const filtered = computed(() => {
  const k = keyword.value.trim().toLowerCase()
  return sources.value.filter((s) => {
    if (view.value === 'active' && s.delisted) return false
    if (view.value === 'delisted' && !s.delisted) return false
    if (!k) return true
    return [String(s.id), s.ownerName, s.resourceName, s.configDesc, s.unitPrice, s.ownerDemand]
      .join('\n')
      .toLowerCase()
      .includes(k)
  })
})

// ---- 复制整理 ----

const COPY_FIELDS = [
  { key: 'ownerName', label: '号主名称' },
  { key: 'resourceName', label: '资源名称' },
  { key: 'configDesc', label: '资源配置情况描述' },
  { key: 'unitPrice', label: '资源单价' },
  { key: 'ownerDemand', label: '号主要求' },
]
const STORAGE_KEY = 'keySourcesCopyOptions'

function loadCopyOptions() {
  const def = { date: true, fields: COPY_FIELDS.map((f) => f.key) }
  try {
    const saved = JSON.parse(localStorage.getItem(STORAGE_KEY) || 'null')
    if (saved && Array.isArray(saved.fields)) {
      return {
        date: saved.date !== false,
        fields: saved.fields.filter((k) => COPY_FIELDS.some((f) => f.key === k)),
      }
    }
  } catch {
    // 本地存储不可用或内容损坏时用默认值。
  }
  return def
}

const initialCopy = loadCopyOptions()
const copyDate = ref(initialCopy.date)
const copyFields = ref(initialCopy.fields)
const copyTextRef = ref(null)
const copyHint = ref('')

watch([copyDate, copyFields], () => {
  try {
    localStorage.setItem(STORAGE_KEY, JSON.stringify({ date: copyDate.value, fields: copyFields.value }))
  } catch {
    // 写不进去不影响复制本身。
  }
})

// 每条资源压成一行：字段内的换行折成空格，粘贴到聊天里不会被拆成多条。
const oneLine = (v) => String(v || '').replace(/\s*[\r\n]+\s*/g, ' ').trim()

const copyContent = computed(() => {
  // 按固定顺序而不是勾选顺序输出，保证每次复制格式一致。
  const keys = COPY_FIELDS.map((f) => f.key).filter((k) => copyFields.value.includes(k))
  const lines = []
  if (copyDate.value) lines.push(today())
  activeSources.value.forEach((s, i) => {
    const parts = keys.map((k) => oneLine(s[k])).filter((v) => v !== '')
    lines.push(`资源${i + 1}：${parts.join(' ')}`)
  })
  return activeSources.value.length > 0 ? lines.join('\n') : ''
})

async function doCopy() {
  copyHint.value = ''
  if (!copyContent.value) {
    copyHint.value = '当前没有在售资源可复制'
    return
  }
  const r = await copyText(copyContent.value)
  if (r === 'ok') {
    copyHint.value = `已复制 ${activeSources.value.length} 条在售资源`
  } else {
    selectElementText(copyTextRef.value)
    copyHint.value = '自动复制失败，内容已选中，请按 Ctrl+C 复制'
  }
}

// ---- 增删改查 ----

async function api(url, options) {
  const resp = await fetch(url, options)
  const data = await resp.json().catch(() => ({}))
  if (!resp.ok) {
    if (resp.status === 401) emit('unauthorized')
    throw new Error(data.error || `请求失败（${resp.status}）`)
  }
  return data
}

async function loadSources() {
  error.value = ''
  loading.value = true
  try {
    const data = await api('/api/key-sources')
    sources.value = data.sources || []
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

function openCopy() {
  copyHint.value = ''
  showCopy.value = true
}

function startEdit(s) {
  message.value = ''
  formError.value = ''
  draft.value = {
    id: s.id,
    addedDate: s.addedDate,
    ownerName: s.ownerName,
    resourceName: s.resourceName,
    configDesc: s.configDesc || '',
    unitPrice: s.unitPrice || '',
    ownerDemand: s.ownerDemand || '',
    delisted: s.delisted,
  }
  showForm.value = true
}

async function save(payload, okMessage) {
  message.value = ''
  saving.value = true
  try {
    await api('/api/key-sources', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(payload),
    })
    message.value = okMessage
    await loadSources()
    return true
  } catch (err) {
    const text = '保存失败：' + err.message
    if (showForm.value) formError.value = text
    else error.value = text
    return false
  } finally {
    saving.value = false
  }
}

async function saveSource() {
  formError.value = ''
  if (!draft.value.ownerName.trim()) {
    formError.value = '号主名称不能为空'
    return
  }
  if (!draft.value.resourceName.trim()) {
    formError.value = '资源名称不能为空'
    return
  }
  const ok = await save(draft.value, isEditing.value ? '已更新货源' : '已新增货源')
  if (ok) closeForm()
}

// 列表里一键上/下架：只翻转 delisted，其余字段原样带回，避免另开一个接口。
async function toggleDelisted(s) {
  error.value = ''
  await save({ ...s, delisted: !s.delisted }, s.delisted ? `货源 #${s.id} 已重新上架` : `货源 #${s.id} 已下架`)
}

async function deleteSource(s) {
  error.value = ''
  message.value = ''
  if (!window.confirm(`确定删除货源 #${s.id}（${s.ownerName} / ${s.resourceName}）？删除后无法恢复。\n只是不再出售的话，建议用「下架」。`)) return
  deleting.value = true
  try {
    await api('/api/delete-key-source', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ id: s.id }),
    })
    message.value = `已删除货源 #${s.id}`
    await loadSources()
  } catch (err) {
    error.value = '删除失败：' + err.message
  } finally {
    deleting.value = false
  }
}

onMounted(loadSources)
defineExpose({ loadSources })
</script>

<template>
  <div>
    <div class="card">
      <div class="toolbar">
        <button type="button" class="btn-primary" @click="openCreate">+ 新增货源</button>
        <button type="button" class="btn-browse" @click="openCopy">复制在售资源（{{ activeSources.length }}）</button>
        <button type="button" class="btn-browse" @click="loadSources" :disabled="loading">
          {{ loading ? '读取中…' : '刷新' }}
        </button>
        <select v-model="view">
          <option value="active">在售</option>
          <option value="delisted">已下架</option>
          <option value="all">全部</option>
        </select>
        <input v-model="keyword" type="text" class="search" placeholder="搜索 序号 / 号主 / 资源 / 配置 / 单价 / 要求" />
      </div>

      <p class="error" v-if="error">{{ error }}</p>
      <span class="hint" v-if="message">{{ message }}</span>

      <table v-if="filtered.length > 0">
        <thead>
          <tr>
            <th>序号</th>
            <th>添加日期</th>
            <th>号主名称</th>
            <th>资源名称</th>
            <th>资源配置情况描述</th>
            <th>资源单价</th>
            <th>号主要求</th>
            <th>状态</th>
            <th>操作</th>
          </tr>
        </thead>
        <tbody>
          <tr
            v-for="s in filtered"
            :key="s.id"
            :class="{ 'row-delisted': s.delisted }"
          >
            <td>{{ s.id }}</td>
            <td class="nowrap">{{ s.addedDate }}</td>
            <td class="wrap">{{ s.ownerName }}</td>
            <td class="wrap">{{ s.resourceName }}</td>
            <td class="wrap pre">{{ s.configDesc }}</td>
            <td class="wrap">{{ s.unitPrice }}</td>
            <td class="wrap pre">{{ s.ownerDemand }}</td>
            <td class="nowrap">{{ s.delisted ? '已下架' : '在售' }}</td>
            <td class="ops">
              <button type="button" class="btn-link" @click="startEdit(s)">编辑</button>
              <button type="button" class="btn-link" @click="toggleDelisted(s)" :disabled="saving">
                {{ s.delisted ? '上架' : '下架' }}
              </button>
              <button type="button" class="btn-link danger" @click="deleteSource(s)" :disabled="deleting">删除</button>
            </td>
          </tr>
        </tbody>
      </table>
      <span class="hint" v-else-if="loaded && sources.length > 0">当前筛选下没有货源。</span>
      <span class="hint" v-else-if="!loading && loaded">还没有货源，点击上方「新增货源」添加。</span>
      <span class="hint" v-else-if="!loaded">正在读取货源列表…</span>

      <div class="modal-mask" v-if="showForm">
        <div class="modal" role="dialog" aria-modal="true">
          <div class="modal-head">
            <h3>{{ isEditing ? `编辑货源 #${draft.id}` : '新增货源' }}</h3>
            <button type="button" class="modal-close" @click="closeForm" aria-label="关闭">×</button>
          </div>
          <div class="modal-body">
            <div class="form-grid">
              <label>
                <span>添加日期</span>
                <input v-model="draft.addedDate" type="date" />
              </label>
              <label>
                <span>资源单价</span>
                <input v-model="draft.unitPrice" type="text" placeholder="例如：0.8 元/刀" />
              </label>
              <label>
                <span>号主名称<em class="req">*</em></span>
                <input v-model="draft.ownerName" type="text" />
              </label>
              <label>
                <span>资源名称<em class="req">*</em></span>
                <input v-model="draft.resourceName" type="text" />
              </label>
              <label class="full">
                <span>资源配置情况描述</span>
                <textarea v-model="draft.configDesc" rows="3"></textarea>
              </label>
              <label class="full">
                <span>号主要求</span>
                <textarea v-model="draft.ownerDemand" rows="3"></textarea>
              </label>
              <label class="check">
                <input type="checkbox" v-model="draft.delisted" /> 已下架
              </label>
            </div>
            <p class="error" v-if="formError">{{ formError }}</p>
          </div>
          <div class="modal-foot">
            <button type="button" class="btn-browse" @click="closeForm" :disabled="saving">取消</button>
            <button type="button" class="btn-primary" @click="saveSource" :disabled="saving">
              {{ saving ? '保存中…' : '保存' }}
            </button>
          </div>
        </div>
      </div>

      <div class="modal-mask" v-if="showCopy">
        <div class="modal" role="dialog" aria-modal="true">
          <div class="modal-head">
            <h3>复制在售资源</h3>
            <button type="button" class="modal-close" @click="showCopy = false" aria-label="关闭">×</button>
          </div>
          <div class="modal-body">
            <p class="hint">整理当前所有未下架的资源，勾选需要带上的字段后复制。</p>
            <div class="check-row">
              <label class="check"><input type="checkbox" v-model="copyDate" /> 复制日期</label>
              <label class="check" v-for="f in COPY_FIELDS" :key="f.key">
                <input type="checkbox" :value="f.key" v-model="copyFields" /> {{ f.label }}
              </label>
            </div>
            <pre class="preview" ref="copyTextRef">{{ copyContent || '（当前没有在售资源）' }}</pre>
            <span class="hint" v-if="copyHint">{{ copyHint }}</span>
          </div>
          <div class="modal-foot">
            <button type="button" class="btn-browse" @click="showCopy = false">关闭</button>
            <button type="button" class="btn-primary" @click="doCopy" :disabled="!copyContent">
              复制（{{ activeSources.length }} 条在售）
            </button>
          </div>
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
.hint.inline {
  display: inline;
}
.form-grid {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(220px, 1fr));
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
.req {
  color: #d92626;
  font-style: normal;
  margin-left: 2px;
}
input[type='text'],
input[type='date'],
select,
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
.check-row {
  display: flex;
  flex-wrap: wrap;
  gap: 6px 18px;
  margin: 10px 0;
}
label.check {
  flex-direction: row;
  align-items: center;
  gap: 4px;
  font-size: 13px;
  color: #444;
  cursor: pointer;
}
.preview {
  background: #fafbfc;
  border: 1px solid #e2e2e2;
  border-radius: 6px;
  padding: 10px 12px;
  font-size: 13px;
  font-family: inherit;
  white-space: pre-wrap;
  word-break: break-all;
  max-height: 260px;
  overflow: auto;
  margin: 8px 0;
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
td.nowrap,
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
  max-width: 680px;
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
.row-delisted {
  color: #999;
}
.error {
  color: #d92626;
  margin-top: 8px;
}
</style>
