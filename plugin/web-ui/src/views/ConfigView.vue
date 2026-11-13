<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import {
  NAlert,
  NButton,
  NCard,
  NInput,
  NSelect,
  NSpace,
  NSpin,
  NTag,
  useDialog,
  type SelectOption,
} from 'naive-ui'
import { endpoints } from '@/api/endpoints'
import type { ConfigItemDTO, ConfigKeySpec } from '@/api/types'
import { useApiError } from '@/composables/useApiError'

// 配置键页面（REQ-API-CONFIG-CATALOG）：表单完全由内核目录驱动，页面不写死任何 ai.* key。
// 用户只看到「模型服务商 / 模型名称 / API 密钥」这类中文名，不必记配置键字符串。
const { report, ok } = useApiError()
const dialog = useDialog()

const loading = ref(false)
const items = ref<ConfigItemDTO[]>([])
const priority = ref('')
const groups = ref<string[]>([])
// draft[key] 是输入框里的值。敏感项初始为空串（表示「不修改」），非敏感项回显原值。
const draft = ref<Record<string, string>>({})
const saving = ref<Record<string, boolean>>({})
const dirty = ref<Record<string, boolean>>({})

const byGroup = computed(() => {
  const out: { name: string; items: ConfigItemDTO[] }[] = []
  const idx = new Map<string, number>()
  for (const name of groups.value) {
    idx.set(name, out.length)
    out.push({ name, items: [] })
  }
  for (const it of items.value) {
    let i = idx.get(it.spec.group)
    if (i === undefined) {
      i = out.length
      idx.set(it.spec.group, i)
      out.push({ name: it.spec.group, items: [] })
    }
    out[i].items.push(it)
  }
  return out.filter((g) => g.items.length > 0)
})

function resetDraft(list: ConfigItemDTO[]) {
  const d: Record<string, string> = {}
  for (const it of list) {
    // 敏感项回的是掩码，绝不能把它当原值填进输入框——否则用户什么都没改、点一下保存，
    // 就会把 "sk-a…mnop" 这串掩码写回配置库，等于亲手毁掉真实密钥。
    d[it.spec.key] = it.masked ? '' : it.value
  }
  draft.value = d
  dirty.value = {}
}

async function load() {
  loading.value = true
  try {
    const [cat, cur] = await Promise.all([endpoints.configCatalog(), endpoints.listConfig()])
    groups.value = cat.groups ?? []
    priority.value = cat.priority ?? ''
    items.value = cur.items ?? []
    resetDraft(cur.items ?? [])
  } catch (e) {
    report(e, '加载配置')
  } finally {
    loading.value = false
  }
}

onMounted(load)

function onInput(key: string, value: string) {
  draft.value[key] = value
  dirty.value[key] = true
}

function selectOptions(spec: ConfigKeySpec): SelectOption[] {
  return (spec.options ?? []).map((o) => ({ label: o, value: o }))
}

async function save(it: ConfigItemDTO) {
  const key = it.spec.key
  // 敏感项留空 = 不修改已存的密钥（页面从未拿到明文，无法回写）。
  if (it.masked && !draft.value[key]) {
    ok('未修改（留空表示保持已配置的密钥）')
    dirty.value[key] = false
    return
  }
  const value = draft.value[key] ?? ''
  if (it.spec.value_type === 'number' && value !== '' && !/^[1-9]\d*$/.test(value)) {
    report(new Error('必须填正整数（留空表示回退内置默认）'), `保存 ${it.spec.label}`)
    return
  }
  saving.value[key] = true
  try {
    const res = await endpoints.setConfig(key, value)
    ok(res.known ? `${it.spec.label} 已保存` : `${key} 已保存（该键不在内置目录，请确认拼写）`)
    dirty.value[key] = false
    await load()
  } catch (e) {
    report(e, `保存 ${it.spec.label}`)
  } finally {
    saving.value[key] = false
  }
}

function restore(it: ConfigItemDTO) {
  const key = it.spec.key
  dialog.warning({
    title: '恢复默认',
    content: `删除「${it.spec.label}」的配置值（${key}），此后回退到环境变量或内置默认。当前未配置时无需操作。`,
    positiveText: '删除该项',
    negativeText: '取消',
    onPositiveClick: async () => {
      try {
        await endpoints.deleteConfig(key)
        ok(`${it.spec.label} 已恢复默认`)
        await load()
      } catch (e) {
        // 404 = 本来就没配，与「删掉」对用户是同一结果，不该当错误吓人。
        const msg = (e as Error).message ?? ''
        if (msg.includes('404') || msg.includes('不存在')) {
          ok(`${it.spec.label} 本来就是默认值`)
          await load()
          return
        }
        report(e, `恢复 ${it.spec.label}`)
      }
    },
  })
}
</script>

<template>
  <div>
    <h2 class="page-title">配置 · 关键键</h2>
    <p class="page-desc">
      这里列出 Gear 内部的关键配置键，按中文名填写即可，<b>不必记 <code>ai.model</code> 这类键名</b>。
      目录由内核提供，新增键会自动出现。写入后全局生效（CLI / REST / App 各进程读同一份配置库）。
    </p>

    <NAlert type="info" :bordered="false" style="margin-bottom: 12px">
      <b>生效优先级</b>：{{ priority || '调用参数 > 环境变量 > 配置库 > 内置默认' }}。
      环境变量非空时会压过本页设置，对应项下方会标出「环境变量覆盖中」。
    </NAlert>

    <NSpace style="margin-bottom: 12px" align="center">
      <NButton @click="load()" :loading="loading">刷新</NButton>
      <span class="text-3">API 密钥类只显示掩码；留空保存 = 不修改已配置的密钥。</span>
    </NSpace>

    <NSpin :show="loading">
      <NCard
        v-for="g in byGroup"
        :key="g.name"
        size="small"
        :title="g.name"
        style="margin-bottom: 12px"
      >
        <div v-for="it in g.items" :key="it.spec.key" class="config-row">
          <div class="config-meta">
            <NSpace align="center" :size="6">
              <span class="config-label">{{ it.spec.label }}</span>
              <code class="config-key">{{ it.spec.key }}</code>
              <NTag v-if="it.set" size="small" type="success" :bordered="false">已配置</NTag>
              <NTag v-else size="small" :bordered="false">用默认值</NTag>
              <NTag v-if="it.env_override" size="small" type="warning" :bordered="false">
                {{ it.env_name }} 覆盖中
              </NTag>
              <NTag v-if="!it.known" size="small" type="warning" :bordered="false">目录外，请确认拼写</NTag>
            </NSpace>
            <p class="config-desc">{{ it.spec.description }}</p>
            <p class="config-default text-3">
              默认 {{ it.spec.default || '（空）' }}<span v-if="it.spec.default_note"> · {{ it.spec.default_note }}</span>
              <span v-if="it.masked && it.value"> · 当前 {{ it.value }}（掩码）</span>
            </p>
          </div>

          <NSpace align="center" :size="8" class="config-edit">
            <NSelect
              v-if="it.spec.value_type === 'enum'"
              :value="draft[it.spec.key] ?? ''"
              :options="selectOptions(it.spec)"
              :consistent-menu-width="false"
              style="width: 200px"
              @update:value="(v: string) => onInput(it.spec.key, v)"
            />
            <NInput
              v-else
              :value="draft[it.spec.key] ?? ''"
              :type="it.spec.value_type === 'secret' ? 'password' : 'text'"
              :show-password-toggle="it.spec.value_type === 'secret'"
              :placeholder="
                it.masked ? '已配置，留空 = 不修改' : it.spec.default || '留空 = 用内置默认'
              "
              style="width: 300px"
              @update:value="(v: string) => onInput(it.spec.key, v)"
            />
            <NButton
              size="small"
              type="primary"
              :loading="!!saving[it.spec.key]"
              :disabled="it.spec.value_type !== 'enum' && !dirty[it.spec.key] && !(it.masked && !draft[it.spec.key])"
              @click="save(it)"
            >
              保存
            </NButton>
            <NButton size="small" quaternary :disabled="!it.set" @click="restore(it)">恢复默认</NButton>
          </NSpace>
        </div>
      </NCard>
    </NSpin>
  </div>
</template>

<style scoped>
.config-row {
  display: flex;
  justify-content: space-between;
  align-items: flex-start;
  gap: 16px;
  padding: 10px 0;
  border-bottom: 1px solid var(--n-border-color);
}
.config-row:last-child {
  border-bottom: none;
}
.config-meta {
  flex: 1;
  min-width: 0;
}
.config-label {
  font-weight: 500;
}
.config-key {
  font-family: var(--font-mono, monospace);
  font-size: 12px;
  opacity: 0.75;
}
.config-desc {
  margin: 4px 0 2px;
  font-size: 13px;
  opacity: 0.85;
}
.config-default {
  margin: 0;
  font-size: 12px;
}
.config-edit {
  flex-shrink: 0;
  padding-top: 2px;
}
</style>
