<script setup lang="ts">
import { computed, h, onMounted, onUnmounted } from 'vue'
import { useRouter } from 'vue-router'
import {
  NAlert,
  NButton,
  NCard,
  NDataTable,
  NEmpty,
  NGi,
  NGrid,
  NSpace,
  NStatistic,
  NTag,
  NThing,
  type DataTableColumns,
} from 'naive-ui'
import { useConnStore } from '@/stores/conn'
import { useCatalogStore } from '@/stores/catalog'
import { useExecStore } from '@/stores/exec'
import { useAppsStore } from '@/stores/apps'
import StatusTag from '@/components/StatusTag.vue'
import type { ExecutionDTO } from '@/api/types'

const conn = useConnStore()
const catalog = useCatalogStore()
const exec = useExecStore()
const apps = useAppsStore()
const router = useRouter()

const serviceStepCount = computed(() =>
  catalog.flows.reduce((n, f) => n + (f.steps ?? []).filter((s) => s.mode === 'service').length, 0),
)

// 概览只是「最近几次」的预览，卡片只有半屏宽（约 380px）：只留三列窄的。
// （原先 5 列合计 >550px，后面的列被推出卡片右边界，横向还没滚动条 —— 够不着也看不见。
//   耗时与错误都留在「执行记录」页看，那里表更宽。）
const execColumns: DataTableColumns<ExecutionDTO> = [
  { title: 'Flow', key: 'flow_id', width: 120, ellipsis: { tooltip: true } },
  {
    title: '状态',
    key: 'status',
    width: 100,
    render: (row) => h(StatusTag, { kind: 'exec', value: row.status }),
  },
  {
    title: '开始时间',
    key: 'started_at',
    width: 150,
    render: (row) => new Date(row.started_at).toLocaleTimeString(),
  },
]

onMounted(async () => {
  await Promise.allSettled([catalog.loadAll(), exec.loadList(), apps.loadList(), conn.ping()])
  exec.startListPolling(4000)
  apps.startPolling(6000)
})
onUnmounted(() => {
  exec.stopAll()
  apps.stopPolling()
})

function gotoExecutions() {
  exec.stopListPolling()
  void router.push('/executions')
}
</script>

<template>
  <div>
    <h2 class="page-title">概览</h2>
    <p class="page-desc">
      内核状态与最近活动。本页所有数据都来自 REST 只读查询，不改变内核任何状态。
    </p>

    <NGrid :cols="4" :x-gap="14" :y-gap="14" responsive="screen" item-responsive>
      <NGi span="4 s:2 m:1">
        <NCard size="small">
          <NStatistic label="Tools 已注册" :value="conn.health?.tools ?? catalog.tools.length" />
        </NCard>
      </NGi>
      <NGi span="4 s:2 m:1">
        <NCard size="small">
          <NStatistic label="Flows 已注册" :value="conn.health?.flows ?? catalog.flows.length" />
        </NCard>
      </NGi>
      <NGi span="4 s:2 m:1">
        <NCard size="small">
          <NStatistic label="运行中执行" :value="exec.runningCount">
            <template #suffix>
              <NButton text size="tiny" @click="gotoExecutions">查看</NButton>
            </template>
          </NStatistic>
        </NCard>
      </NGi>
      <NGi span="4 s:2 m:1">
        <NCard size="small">
          <NStatistic label="受管长驻 App" :value="apps.apps.length" />
        </NCard>
      </NGi>
    </NGrid>

    <NGrid :cols="2" :x-gap="14" :y-gap="14" responsive="screen" item-responsive style="margin-top: 14px">
      <NGi span="2 m:1">
        <NCard title="最近执行" size="small" :bordered="true">
          <template #header-extra>
            <NButton text size="small" @click="gotoExecutions">全部记录</NButton>
          </template>
          <NDataTable
            :columns="execColumns"
            :data="exec.sorted.slice(0, 6)"
            :bordered="false"
            :single-line="false"
            size="small"
            :scroll-x="370"
            :pagination="false"
          />
        </NCard>
      </NGi>

      <NGi span="2 m:1">
        <NCard title="在跑的长驻 App" size="small">
          <template #header-extra>
            <NButton text size="small" @click="router.push('/apps')">管理</NButton>
          </template>
          <NEmpty v-if="apps.apps.length === 0" size="small" description="没有在跑的长驻 Flow" />
          <NSpace v-else vertical :size="10">
            <NThing v-for="a in apps.apps" :key="a.app_id" :title="a.flow_id">
              <template #description>
                <NSpace :size="8" align="center">
                  <NTag size="small" :bordered="false" :type="a.state === 'running' ? 'success' : 'default'">{{ a.state }}</NTag>
                  <span class="text-3">起于 {{ new Date(a.started_at).toLocaleTimeString() }}</span>
                </NSpace>
              </template>
            </NThing>
          </NSpace>
        </NCard>
      </NGi>
    </NGrid>

    <div v-if="serviceStepCount > 0" style="margin-top: 14px">
      <NAlert type="info" :bordered="false">
        已注册的 Flow 里有 <b>{{ serviceStepCount }}</b> 个 <code>service</code> 长驻 step。
        这类 Flow 不能用一次性执行，须走 <code>POST /api/apps</code>（Apps 页）由 Supervisor 监管。
      </NAlert>
    </div>
  </div>
</template>

<style scoped>
/* .dim → 已收归 src/styles/typography.css 的 .text-3 */
</style>
