<script setup lang="ts">
import { computed, h, onMounted, onUnmounted, ref } from 'vue'
import {
  NAlert,
  NButton,
  NCard,
  NDataTable,
  NEmpty,
  NSelect,
  NSpace,
  NTag,
  useDialog,
  type DataTableColumns,
} from 'naive-ui'
import { useAppsStore } from '@/stores/apps'
import { useCatalogStore } from '@/stores/catalog'
import { useApiError } from '@/composables/useApiError'
import type { AppDTO } from '@/api/types'

const apps = useAppsStore()
const catalog = useCatalogStore()
const { report, ok } = useApiError()
const dialog = useDialog()

const flowId = ref<string | null>(null)
const restart = ref('always')
const starting = ref(false)

const restartOptions = [
  { label: 'always · 崩溃后按指数退避重启', value: 'always' },
  { label: 'never · 退出即结束', value: 'never' },
]

const flowOptions = computed(() =>
  catalog.flows.map((f) => {
    const svc = (f.steps ?? []).filter((s) => s.mode === 'service').length
    return { label: svc > 0 ? `${f.id}  (${svc} 个 service step)` : `${f.id}  (无 service step)`, value: f.id }
  }),
)

const selectedHasService = computed(() => {
  if (!flowId.value) return false
  const f = catalog.flowById.get(flowId.value)
  return (f?.steps ?? []).some((s) => s.mode === 'service')
})

function uptime(a: AppDTO): string {
  const ms = Date.now() - Date.parse(a.started_at)
  if (ms < 60000) return `${Math.floor(ms / 1000)} s`
  if (ms < 3600000) return `${Math.floor(ms / 60000)} min`
  return `${(ms / 3600000).toFixed(1)} h`
}

async function start() {
  if (!flowId.value) return
  starting.value = true
  try {
    await apps.start(flowId.value, restart.value)
    ok(`已托管 ${flowId.value}（restart=${restart.value}）`)
  } catch (e) {
    report(e, '启动 App')
  } finally {
    starting.value = false
  }
}

function confirmStop(a: AppDTO) {
  dialog.warning({
    title: '停止受管 App',
    content: `取消 ${a.id}（${a.flow_id}）的 ctx ⇒ Supervisor 会停止并杀掉它的所有 service 子进程。`,
    positiveText: '停止',
    negativeText: '取消',
    onPositiveClick: async () => {
      try {
        await apps.stop(a.id)
        ok(`已停止 ${a.id}`)
      } catch (e) {
        report(e, '停止 App')
      }
    },
  })
}

const columns = computed<DataTableColumns<AppDTO>>(() => [
  { title: 'app id', key: 'id', width: 200, render: (row) => h('span', { class: 'text-mono' }, row.id) },
  { title: 'Flow', key: 'flow_id', width: 200, render: (row) => h('span', { class: 'text-mono' }, row.flow_id) },
  {
    title: 'restart',
    key: 'restart',
    width: 120,
    render: (row) => h(NTag, { size: 'small', type: 'info', bordered: false }, () => row.restart),
  },
  {
    title: '启动时刻',
    key: 'started_at',
    width: 200,
    render: (row) => new Date(row.started_at).toLocaleString(),
  },
  { title: '已运行', key: 'uptime', width: 100, render: (row) => uptime(row) },
  {
    title: '操作',
    key: 'actions',
    fixed: 'right' as const,
    width: 160,
    render: (row) => h(NButton, { size: 'small', type: 'error', quaternary: true, onClick: () => confirmStop(row) }, () => '停止'),
  },
])

onMounted(async () => {
  try {
    await Promise.all([catalog.loadAll(), apps.loadList()])
  } catch (e) {
    report(e, '加载 App 列表')
  }
  apps.startPolling(5000)
})

onUnmounted(() => apps.stopPolling())
</script>

<template>
  <div>
    <h2 class="page-title">Apps · 长驻</h2>
    <p class="page-desc">
      <code>POST /api/apps</code> 等价于 CLI <code>app run</code>：把一条含 <b>service</b> step 的 Flow
      变成受管进程，Supervisor 负责拉起、健康探针、崩溃退避重启；停止即取消 ctx，连带杀掉所有 service 子进程。
    </p>

    <NAlert type="warning" :bordered="false" style="margin-bottom: 12px">
      <b>状态持久性</b>：App 列表是内核的<b>内存态</b>（<code>server.apps</code> 这个 map）。
      重启 <code>gear serve</code> 后列表会清空、子进程也不会自动恢复 —— 这与执行记录（SQLite 持久）不同，是内核的既定设计。
    </NAlert>

    <NSpace style="margin-bottom: 12px" align="center">
      <NSelect
        v-model:value="flowId"
        :options="flowOptions"
        filterable
        placeholder="选择要托管的 Flow"
        style="width: 300px"
      />
      <NSelect v-model:value="restart" :options="restartOptions" style="width: 300px" />
      <NButton type="primary" :loading="starting" :disabled="!flowId" @click="start">托管启动</NButton>
      <NButton @click="apps.loadList()">刷新</NButton>
    </NSpace>

    <NAlert v-if="flowId && !selectedHasService" type="info" :bordered="false" style="margin-bottom: 12px">
      这条 Flow 没有 service step：它会被当作一次性执行跑完即退出。若 restart=always，Supervisor 会不断重启它 ——
      通常这不是你想要的，请确认。
    </NAlert>

    <NCard size="small">
      <NDataTable
        v-if="apps.apps.length > 0"
        :columns="columns"
        :data="apps.apps"
        :loading="apps.loading"
        :bordered="false"
        :single-line="false"
        size="small"
        :scroll-x="980"
        :pagination="false"
        :row-key="(row: AppDTO) => row.id"
      />
      <NEmpty v-else description="当前没有受管的长驻 App" />
    </NCard>
  </div>
</template>

<style scoped>
/* .mono 已收归 src/styles/typography.css 的 .text-mono */
</style>
