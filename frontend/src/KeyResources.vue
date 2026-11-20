<script setup>
import { ref } from 'vue'
import KeyStages from './KeyStages.vue'
import KeySources from './KeySources.vue'

// 上 key 资源整理：台子管理 + 上 key 货源整理两个子页。
// 子页用 v-if 切换，每次切回都会重新读取最新数据。

const emit = defineEmits(['unauthorized'])
const sub = ref('sources')
</script>

<template>
  <div>
    <div class="sub-tabs">
      <button type="button" :class="{ active: sub === 'sources' }" @click="sub = 'sources'">上key货源整理</button>
      <button type="button" :class="{ active: sub === 'stages' }" @click="sub = 'stages'">台子管理</button>
    </div>
    <KeySources v-if="sub === 'sources'" @unauthorized="emit('unauthorized')" />
    <KeyStages v-if="sub === 'stages'" @unauthorized="emit('unauthorized')" />
  </div>
</template>

<style scoped>
.sub-tabs {
  display: flex;
  gap: 8px;
  margin-bottom: 12px;
}
.sub-tabs button {
  background: #fff;
  border: 1px solid #c7d7fb;
  color: #2c6ef2;
  border-radius: 16px;
  padding: 4px 16px;
  cursor: pointer;
  font-size: 13px;
}
.sub-tabs button.active {
  background: #2c6ef2;
  color: #fff;
  border-color: #2c6ef2;
}
</style>
