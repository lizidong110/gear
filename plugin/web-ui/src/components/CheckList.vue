<script setup lang="ts">
import { NAlert, NCode } from 'naive-ui'
import type { PrecheckResult } from '@/logic/template'

const props = defineProps<{ result: PrecheckResult | null; showWhenClean?: boolean }>()

const emit = defineEmits<{ (e: 'jump', index: number): void }>()
</script>

<template>
  <div v-if="props.result" class="precheck">
    <NAlert
      v-if="props.result.errors.length > 0"
      type="error"
      title="内核会拒绝这份定义（注册期校验）"
      :bordered="false"
    >
      <div class="hint">
        以下文案与内核 <NCode code="ValidateFlowStatic" inline /> 同规则，首条即内核会返回的那一条：
      </div>
      <ol class="items">
        <li v-for="(e, i) in props.result.errors" :key="i" @click="emit('jump', i)">{{ e }}</li>
      </ol>
    </NAlert>

    <NAlert
      v-else-if="props.showWhenClean"
      type="success"
      title="静态校验通过"
      :bordered="false"
      >结构与依赖图、模板引用、Tool 与版本存在性均无问题。</NAlert
    >

    <div v-if="props.result.warnings.length > 0" class="warns">
      <NAlert v-for="(w, i) in props.result.warnings" :key="i" type="warning" :bordered="false" class="warn">
        <span class="warn-text">{{ w }}</span>
      </NAlert>
    </div>
  </div>
</template>

<style scoped>
.precheck {
  display: flex;
  flex-direction: column;
  gap: 8px;
}
.hint {
  /* 不压 opacity：它在 NAlert 里，压暗等于在红底上再糊一层，本就小的字更难读 */
  font-size: 12px;
  line-height: 1.7;
  margin-bottom: 6px;
}
.items {
  margin: 0;
  padding-left: 18px;
  font-size: 12px;
  line-height: 1.7;
  cursor: pointer;
}
.items li:hover {
  text-decoration: underline;
}
.warns {
  display: flex;
  flex-direction: column;
  gap: 6px;
}
.warn-text {
  font-size: 12px;
  white-space: pre-wrap;
  word-break: break-all;
}
</style>
