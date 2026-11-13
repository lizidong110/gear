<script setup lang="ts">
import { computed, h, onMounted, onUnmounted, ref } from 'vue'
import {
  NAlert,
  NButton,
  NCard,
  NCode,
  NDataTable,
  NDivider,
  NDrawer,
  NDrawerContent,
  NEmpty,
  NSelect,
  NSpace,
  NSwitch,
  NTabPane,
  NTabs,
  NTag,
  type DataTableColumns,
} from 'naive-ui'
import { EXEC_STATUSES, EXEC_STATUS_LABEL } from '@/logic/enums'
import { useCatalogStore } from '@/stores/catalog'
import { useExecStore } from '@/stores/exec'
import { useApiError } from '@/composables/useApiError'
import FlowGraph from '@/components/FlowGraph.vue'
import StatusTag from '@/components/StatusTag.vue'
import type { ExecutionDTO } from '@/api/types'
import type { StepLike } from '@/logic/levels'

const exec = useExecStore()
const catalog = useCatalogStore()
const { report, ok } = useApiError()

const statusFilter = ref<string | null>(null)
const autoRefresh = ref(true)
const triggerFlow = ref<string | null>(null)
const triggering = ref(false)

const filtered = computed(() =>
  statusFilter.value ? exec.sorted.filter((e) => e.status === statusFilter.value) : exec.sorted,
)

const statusOptions = EXEC_STATUSES.map((s) => ({ label: EXEC_STATUS_LABEL[s] ?? s, value: s }))
const flowOptions = computed(() => catalog.flows.map((f) => ({ label: f.id, value: f.id })))

function duration(row: ExecutionDTO): string {
  const end = row.finished_at ? Date.parse(row.finished_at) : Date.now()
  const ms = Math.max(0, end - Date.parse(row.started_at))
  return ms < 1000 ? `${ms} ms` : `${(ms / 1000).toFixed(2)} s`
}

/** RunResult.steps：step → {input, output}（仅**已执行**的 step）。 */
const executedSteps = computed(() => {
  const r = exec.selected?.result
  if (!r?.steps) return [] as Array<{ id: string; input: unknown; output: unknown }>
  return Object.entries(r.steps).map(([id, v]) => ({ id, input: v.input, output: v.output }))
})

/** RunResult.skipped：step → 跳过原因（when 为假或上游被跳过）——「没跑」与「跑失败」分开展示。 */
const skippedSteps = computed(() => {
  const r = exec.selected?.result
  if (!r?.skipped) return [] as Array<{ id: string; reason: string }>
  return Object.entries(r.skipped).map(([id, reason]) => ({ id, reason }))
})

const selectedFlowSteps = computed<StepLike[]>(() => {
  const flowId = exec.selected?.flow_id
  if (!flowId) return []
  const f = catalog.flowById.get(flowId)
  return (f?.steps ?? []) as unknown as StepLike[]
})

const columns = computed<DataTableColumns<ExecutionDTO>>(() => [
  {
    title: '执行 id',
    key: 'id',
    width: 190,
    render: (row) => h('span', { class: 'text-mono' }, row.id),
  },
  { title: 'Flow', key: 'flow_id', width: 160, ellipsis: { tooltip: true } },
  {
    title: '状态',
    key: 'status',
    width: 130,
    render: (row) => h(StatusTag, { kind: 'exec', value: row.status }),
  },
  {
    title: '开始时间',
    key: 'started_at',
    width: 190,
    render: (row) => new Date(row.started_at).toLocaleString(),
  },
  { title: '耗时', key: 'dur', width: 90, render: (row) => duration(row) },
  {
    title: '用到 Tool',
    key: 'tool_ids',
    width: 100,
    render: (row) => (row.tool_ids?.length ? `${row.tool_ids.length} 个` : '—'),
  },
  {
    title: '错误',
    key: 'error',
    width: 220,
    ellipsis: { tooltip: true },
    render: (row) => row.error || '—',
  },
  {
    title: '操作',
    key: 'actions',
    fixed: 'right' as const,
    width: 90,
    render: (row) => h(NButton, { size: 'small', onClick: () => void open(row.id) }, () => '查看'),
  },
])

async function open(id: string) {
  try {
    await exec.open(id)
  } catch (e) {
    report(e, '查看执行')
  }
}

async function trigger() {
  if (!triggerFlow.value) return
  triggering.value = true
  try {
    const id = await exec.trigger(triggerFlow.value)
    ok(`已触发执行 ${id}（202 异步受理）`)
  } catch (e) {
    report(e, '触发执行')
  } finally {
    triggering.value = false
  }
}

function pretty(v: unknown): string {
  try {
    return JSON.stringify(v, null, 2)
  } catch {
    return String(v)
  }
}

onMounted(async () => {
  try {
    await Promise.all([catalog.loadAll().catch(() => undefined), exec.loadList()])
  } catch (e) {
    report(e, '加载执行记录')
  }
  exec.startListPolling(3000)
})

onUnmounted(() => {
  exec.stopAll()
})
</script>

<template>
  <div>
    <h2 class="page-title">执行记录</h2>
    <p class="page-desc">
      执行是<b>异步</b>的：触发后内核立刻回 202 + id，结果靠查询。三种结局刻意分开呈现 ——
      <b>成功 / 失败 / 被跳过（when 为假）</b>，跳过不是失败。日志为 step 的 stdout/stderr
      （内核只落运行期输出，绝不落 input / secret 明文）。
    </p>

    <NSpace style="margin-bottom: 12px" align="center">
      <NSelect
        v-model:value="triggerFlow"
        :options="flowOptions"
        filterable
        placeholder="选择 Flow 触发一次执行"
        style="width: 260px"
      />
      <NButton type="primary" :loading="triggering" :disabled="!triggerFlow" @click="trigger">触发执行</NButton>
      <NSelect
        v-model:value="statusFilter"
        :options="statusOptions"
        clearable
        placeholder="按状态筛选"
        style="width: 170px"
      />
      <NButton @click="exec.loadList()">刷新</NButton>
      <NSpace align="center" :size="6">
        <NSwitch v-model:value="autoRefresh" size="small" @update:value="(v: boolean) => (v ? exec.startListPolling(3000) : exec.stopListPolling())" />
        <span class="text-3">自动刷新（3s，仅页面可见时）</span>
      </NSpace>
    </NSpace>

    <NCard size="small">
      <NDataTable
        :columns="columns"
        :data="filtered"
        :loading="exec.loading"
        :bordered="false"
        :single-line="false"
        size="small"
        :scroll-x="1150"
        :pagination="{ pageSize: 12 }"
        :row-key="(row: ExecutionDTO) => row.id"
      />
    </NCard>

    <NDrawer :show="!!exec.selected" :width="820" placement="right" @update:show="(v: boolean) => !v && exec.close()">
      <NDrawerContent title="执行详情" closable>
        <template v-if="exec.selected">
          <NSpace :size="10" align="center" style="margin-bottom: 10px">
            <span class="text-mono">{{ exec.selected.id }}</span>
            <StatusTag kind="exec" :value="exec.selected.status" />
            <NTag size="small" :bordered="false">{{ exec.selected.flow_id }}</NTag>
            <span class="text-3">{{ new Date(exec.selected.started_at).toLocaleString() }} · {{ duration(exec.selected) }}</span>
          </NSpace>

          <NAlert v-if="exec.selected.status === 'running'" type="info" :bordered="false" style="margin-bottom: 10px">
            仍在运行：详情与日志每秒自动刷新一次，出终态后自动停止轮询。
          </NAlert>

          <NAlert v-if="exec.selected.error" type="error" title="失败原因" :bordered="false" style="margin-bottom: 10px">
            {{ exec.selected.error }}
          </NAlert>

          <NTabs type="line" animated>
            <NTabPane name="graph" tab="编排图">
              <FlowGraph :steps="selectedFlowSteps" :result="exec.selected.result ?? null" />
            </NTabPane>

            <NTabPane name="result" tab="RunResult">
              <template v-if="executedSteps.length === 0 && skippedSteps.length === 0">
                <NEmpty description="没有 step 结果（可能刚触发或直接失败）" />
              </template>

              <div v-for="s in executedSteps" :key="s.id" class="step-block">
                <NSpace align="center" :size="8">
                  <span class="text-mono">{{ s.id }}</span>
                  <NTag size="small" type="success" :bordered="false">已执行</NTag>
                </NSpace>
                <div class="io-grid">
                  <div>
                    <div class="io-label">input</div>
                    <NCode :code="pretty(s.input)" language="json" word-wrap />
                  </div>
                  <div>
                    <div class="io-label">output</div>
                    <NCode :code="pretty(s.output)" language="json" word-wrap />
                  </div>
                </div>
                <NDivider />
              </div>

              <div v-for="s in skippedSteps" :key="s.id" class="step-block">
                <NSpace align="center" :size="8">
                  <span class="text-mono">{{ s.id }}</span>
                  <NTag size="small" type="warning" :bordered="false">已跳过</NTag>
                  <span class="text-3">{{ s.reason }}</span>
                </NSpace>
              </div>
            </NTabPane>

            <NTabPane name="tools" tab="用到的 Tool">
              <NEmpty v-if="!exec.selected.tool_ids?.length" description="内核未上报 Tool 清单" />
              <NSpace v-else>
                <NTag v-for="t in exec.selected.tool_ids" :key="t" size="small" :bordered="false">{{ t }}</NTag>
              </NSpace>
              <div class="text-3" style="margin-top: 8px">
                这是「本次执行**可能**用到的 Tool」（step 的 call ∪ 生效 Hook 的 action），
                也是内核「运行中不可注销」守卫的判据。
              </div>
            </NTabPane>

            <NTabPane name="logs" tab="运行日志">
              <NSpace align="center" style="margin-bottom: 8px">
                <NSwitch
                  :value="exec.realtime"
                  size="small"
                  @update:value="(v: boolean) => void exec.setRealtime(v)"
                />
                <span class="text-3">实时（SSE 流，只推订阅后的新行；历史仍先拉一次）</span>
              </NSpace>
              <NAlert v-if="exec.logsError" type="error" :bordered="false" style="margin-bottom: 8px">{{
                exec.logsError
              }}</NAlert>
              <NEmpty v-if="exec.logs.length === 0" description="暂无日志（仅 process runtime 的 stdout/stderr 会落这里）" />
              <div v-else class="logs">
                <div
                  v-for="(l, i) in exec.logs"
                  :key="i"
                  :class="['log-line', l.stream === 'stderr' ? 'log-err' : 'log-out']"
                >{{ l.step_id }} · {{ l.stream }}: {{ l.line }}</div>
              </div>
            </NTabPane>
          </NTabs>
        </template>

        <NEmpty v-else description="加载中…" />
      </NDrawerContent>
    </NDrawer>
  </div>
</template>

<style scoped>
/* .mono / .dim 已收归 src/styles/typography.css（.text-mono / .text-3）——
   每个视图各写一份 opacity 压字的做法，就是「六个页面各糊各的」的来源。 */
.step-block {
  margin-bottom: 10px;
}
.io-grid {
  display: grid;
  grid-template-columns: 1fr 1fr;
  gap: 10px;
  margin-top: 6px;
}
.io-label {
  font-size: 12px;
  color: var(--n-text-color-3);
  margin-bottom: 4px;
}
.logs {
  margin: 0;
  padding: 10px;
  max-height: 420px;
  overflow: auto;
  /* 比卡片略深一档的日志面板；文字用主题正文色，别用 rgba 直接压黑把字也压没了 */
  background: var(--n-action-color, rgba(0, 0, 0, 0.28));
  border: 1px solid var(--n-border-color);
  border-radius: 6px;
  color: var(--n-text-color-base);
  font-size: 12px;
  line-height: 1.7;
}
.log-line {
  white-space: pre-wrap;
  word-break: break-all;
  font-family: var(--n-font-family-mono, ui-monospace, monospace);
}
.log-out {
  color: var(--n-text-color-base);
}
.log-err {
  color: var(--n-error-color, #e88080);
}
</style>
