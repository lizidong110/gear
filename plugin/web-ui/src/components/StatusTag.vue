<script setup lang="ts">
import { computed } from 'vue'
import { NTag } from 'naive-ui'
import { EXEC_STATUS_LABEL, STATE_LABEL, STEP_MODE_LABEL } from '@/logic/enums'

const props = defineProps<{
  kind: 'exec' | 'state' | 'mode'
  value: string | null | undefined
}>()

const LABELS: Record<string, Record<string, string>> = {
  exec: EXEC_STATUS_LABEL,
  state: STATE_LABEL,
  mode: STEP_MODE_LABEL,
}

// 三态刻意用**三种不同颜色类型**，不靠文字区分：
// 运行中=info、成功=success、失败=error、被拒=warning —— 一眼可辨（红线 7：三态不可混淆）。
// 按 kind 分层：同一个字面量在不同枚举里含义不同，不能塞进一张扁平表
// —— `rejected` 在执行状态里是「背压被拒」（warning），在版本状态里是「已否决」（error）。
// （扁平表的写法会被 TS1117「对象字面量重复键」直接拦下，这条错误救过一个真 bug。）
type TagType = 'default' | 'info' | 'success' | 'warning' | 'error'

const TYPES: Record<'exec' | 'state' | 'mode', Record<string, TagType>> = {
  exec: { running: 'info', succeeded: 'success', failed: 'error', rejected: 'warning' },
  state: { draft: 'default', testing: 'warning', stable: 'success', deprecated: 'default', rejected: 'error' },
  mode: { once: 'default', service: 'info' },
}

const label = computed(() => {
  const v = props.value ?? ''
  if (!v) return props.kind === 'mode' ? 'once（默认）' : '—'
  return LABELS[props.kind]?.[v]?.split(' · ')[0] ?? v
})

const type = computed(() => TYPES[props.kind]?.[props.value ?? ''] ?? 'default')
const isBold = computed(() => props.value === 'failed' || props.value === 'rejected')
</script>

<template>
  <NTag :type="type" size="small" :bordered="false" :strong="isBold">{{ label }}</NTag>
</template>
