<script setup lang="ts">
import { computed, h, onMounted, onUnmounted, ref } from 'vue'
import {
  NAlert,
  NButton,
  NCard,
  NDataTable,
  NDrawer,
  NDrawerContent,
  NEmpty,
  NSelect,
  NSpace,
  NSwitch,
  NTag,
  useDialog,
  type DataTableColumns,
} from 'naive-ui'
import { useAppsStore } from '@/stores/apps'
import { useCatalogStore } from '@/stores/catalog'
import { useApiError } from '@/composables/useApiError'
import { endpoints, streamAppLogs, type LiveLog } from '@/api/endpoints'
import type { AppDTO, AppRecordDTO, PruneAppsRespDTO } from '@/api/types'

const apps = useAppsStore()
const catalog = useCatalogStore()
const { report, ok } = useApiError()
const dialog = useDialog()

const flowId = ref<string | null>(null)
const restart = ref('always')
const starting = ref(false)
// 刚启动的 App 的「打开地址」，用于启动后直接给出可点击跳转链接。
const startedLinks = ref<string[]>([])

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
  startedLinks.value = []
  try {
    await apps.start(flowId.value, restart.value)
    ok(`已托管 ${flowId.value}（restart=${restart.value}）`)
    // 启动后从刷新后的列表里取该 App 的打开地址，直接呈现可点击跳转链接
    // （收尾步可能稍晚才回传地址，列表每 5s 轮询会自然补全；此处先尽量即时给出）。
    const a = apps.apps.find((x) => x.flow_id === flowId.value)
    startedLinks.value = a?.open_urls ?? []
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

// ---- 清理应用台账（POST /api/apps/prune）：与内存侧 /api/apps 不是同一模型，
//      动的是 `gear app start` 落下的持久台账。只删确定已终止的记录，不杀进程、不碰端口。 ----
const pruning = ref(false)
const pruneResult = ref<PruneAppsRespDTO | null>(null)

function confirmPrune() {
  dialog.warning({
    title: '清理应用台账',
    content:
      '清理落盘台账里「确定已终止」的应用记录（exited / stopped / startup_failed）。只删记录、不杀进程、不碰端口；running / unknown 一律保留。',
    positiveText: '清理',
    negativeText: '取消',
    onPositiveClick: async () => {
      pruning.value = true
      pruneResult.value = null
      try {
        const r = await endpoints.pruneApps({})
        pruneResult.value = r
        ok(`已清理 ${r.pruned_count} 条，跳过 ${r.skipped_count} 条`)
      } catch (e) {
        report(e, '清理台账')
      } finally {
        pruning.value = false
      }
    },
  })
}

const columns = computed<DataTableColumns<AppDTO>>(() => [
  { title: 'app id', key: 'id', width: 200, render: (row) => h('span', { class: 'text-mono' }, row.id) },
  { title: 'Flow', key: 'flow_id', width: 200, render: (row) => h('span', { class: 'text-mono' }, row.flow_id) },
  // 真实监管态（REQ-APP-MANAGED-STATE）：以服务端 done 是否关闭实时重算，
  // 防「监管已死、列表仍 running」的假绿（gear UI 显示正常但 app 实际没工作）。
  {
    title: '状态',
    key: 'state',
    width: 110,
    render: (row) => {
      const exited = row.state === 'exited'
      return h(
        NTag,
        { size: 'small', type: exited ? 'error' : 'success', bordered: false, title: exited ? row.exit_err || '监管已结束' : '监管运行中' },
        () => (exited ? 'exited' : 'running'),
      )
    },
  },
  {
    title: 'restart',
    key: 'restart',
    width: 120,
    render: (row) => h(NTag, { size: 'small', type: 'info', bordered: false }, () => row.restart),
  },
  {
    title: '重启次数',
    key: 'restarts',
    width: 100,
    render: (row) => {
      const n = row.restarts ?? 0
      return h(
        NTag,
        { size: 'small', type: n > 0 ? 'warning' : 'default', bordered: false, title: n > 0 ? 'service 崩溃后被监管重启的累计次数' : '从未重启' },
        () => String(n),
      )
    },
  },
  {
    title: '启动时刻',
    key: 'started_at',
    width: 200,
    render: (row) => new Date(row.started_at).toLocaleString(),
  },
  {
    title: '已运行',
    key: 'uptime',
    width: 100,
    // exited 后没有退出时刻可算，显示 — 而不是继续走表（会误读成还在跑）。
    render: (row) => (row.state === 'exited' ? h('span', { class: 'text-3' }, '—') : uptime(row)),
  },
  {
    title: '打开地址',
    key: 'open_urls',
    width: 260,
    render: (row) => {
      const urls = row.open_urls ?? []
      if (urls.length === 0) return h('span', { class: 'text-3' }, '—')
      return h(
        'div',
        { class: 'open-links' },
        urls.map((u: string) =>
          h('a', { href: u, target: '_blank', rel: 'noopener', class: 'open-link' }, u),
        ),
      )
    },
  },
  {
    title: '操作',
    key: 'actions',
    fixed: 'right' as const,
    width: 160,
    render: (row) => h(NButton, { size: 'small', type: 'error', quaternary: true, onClick: () => confirmStop(row) }, () => '停止'),
  },
])

// ---- 后台应用台账 + 实时日志（REQ-API-APP-LOGS）----
// 上方列表是 /api/apps 的**内存态受管进程**；这里是 gear app start / MCP gear_app(start)
// 派生的独立后台进程（落盘台账）。两者同名前缀，页面必须讲清，否则用户会盯着空列表问
// 「我跑着的应用呢」。实时日志走 SSE：只显示订阅后新产生的行，绝不读 <appID>.log。
const MAX_LOG_LINES = 500
const records = ref<AppRecordDTO[]>([])
const recordsLoading = ref(false)
const showLogs = ref(false)
const logApp = ref<AppRecordDTO | null>(null)
const liveOn = ref(false)
const logLines = ref<LiveLog[]>([])
const logError = ref('')
const logClosed = ref(false)
let logAbort: AbortController | null = null

async function loadRecords() {
  recordsLoading.value = true
  try {
    const res = await endpoints.listAppRecords()
    records.value = res.items ?? []
  } catch (e) {
    report(e, '加载后台应用台账')
  } finally {
    recordsLoading.value = false
  }
}

function stopAppLogStream() {
  if (logAbort) {
    logAbort.abort()
    logAbort = null
  }
}

function startAppLogStream() {
  const app = logApp.value
  if (!app) return
  stopAppLogStream()
  logError.value = ''
  logClosed.value = false
  const ctrl = new AbortController()
  logAbort = ctrl
  void streamAppLogs(
    app.app_id,
    ctrl.signal,
    (l: LiveLog) => {
      logLines.value.push(l)
      if (logLines.value.length > MAX_LOG_LINES) {
        logLines.value.splice(0, logLines.value.length - MAX_LOG_LINES)
      }
    },
    () => {
      logClosed.value = true
      liveOn.value = false
    },
  ).catch((e: unknown) => {
    if ((e as Error)?.name === 'AbortError') return // 用户主动关开关，不是故障
    logError.value = (e as Error)?.message ?? String(e)
    liveOn.value = false
    report(e, '订阅实时日志')
  })
}

function toggleLive(on: boolean) {
  if (on) startAppLogStream()
  else stopAppLogStream()
}

function openLogDrawer(row: AppRecordDTO) {
  stopAppLogStream()
  logApp.value = row
  logLines.value = []
  logError.value = ''
  logClosed.value = false
  showLogs.value = true
  liveOn.value = true
  startAppLogStream()
}

const recordColumns = computed<DataTableColumns<AppRecordDTO>>(() => [
  {
    title: 'app id',
    key: 'app_id',
    width: 210,
    render: (row) => h('span', { class: 'text-mono' }, row.app_id),
  },
  {
    title: 'Flow',
    key: 'flow_id',
    width: 180,
    render: (row) => h('span', { class: 'text-mono' }, row.flow_id),
  },
  {
    title: '状态',
    key: 'state',
    width: 120,
    render: (row) =>
      h(
        NTag,
        {
          size: 'small',
          type: row.state === 'running' ? 'success' : row.state === 'unknown' ? 'warning' : 'default',
          bordered: false,
        },
        () => row.state,
      ),
  },
  { title: 'PID', key: 'pid', width: 90, render: (row) => String(row.pid) },
  {
    title: '实时日志',
    key: 'live_logs',
    width: 120,
    render: (row) =>
      h(NTag, { size: 'small', type: row.live_logs ? 'success' : 'warning', bordered: false }, () =>
        row.live_logs ? '已接入' : '未接入',
      ),
  },
  {
    title: '操作',
    key: 'actions',
    fixed: 'right' as const,
    width: 140,
    render: (row) =>
      h(NButton, { size: 'small', quaternary: true, onClick: () => openLogDrawer(row) }, () => '实时日志'),
  },
])

onMounted(async () => {
  try {
    await Promise.all([catalog.loadAll(), apps.loadList(), loadRecords()])
  } catch (e) {
    report(e, '加载 App 列表')
  }
  apps.startPolling(5000)
})

onUnmounted(() => {
  apps.stopPolling()
  stopAppLogStream()
})
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
      <NButton :loading="pruning" @click="confirmPrune">清理台账</NButton>
    </NSpace>

    <NAlert v-if="flowId && !selectedHasService" type="info" :bordered="false" style="margin-bottom: 12px">
      这条 Flow 没有 service step：它会被当作一次性执行跑完即退出。若 restart=always，Supervisor 会不断重启它 ——
      通常这不是你想要的，请确认。
    </NAlert>

    <NAlert v-if="startedLinks.length > 0" type="success" :bordered="false" style="margin-bottom: 12px">
      <span>已托管，点此打开：</span>
      <a
        v-for="u in startedLinks"
        :key="u"
        :href="u"
        target="_blank"
        rel="noopener"
        class="open-link"
        style="margin-right: 12px"
        >{{ u }}</a
      >
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

    <NCard v-if="pruneResult" size="small" title="清理结果" style="margin-top: 12px">
      <p>
        已清理 <b>{{ pruneResult.pruned_count }}</b> 条，跳过
        <b>{{ pruneResult.skipped_count }}</b> 条（apps_dir:
        <code>{{ pruneResult.apps_dir }}</code>）
      </p>
      <p v-if="pruneResult.pruned.length">已清理：{{ pruneResult.pruned.join(', ') }}</p>
      <p v-if="pruneResult.skipped.length">已跳过：{{ pruneResult.skipped.join(', ') }}</p>
      <p v-if="pruneResult.hint" class="text-3">{{ pruneResult.hint }}</p>
    </NCard>

    <!-- 后台应用台账：用户真正跑着的 detached App 在这一列，不在上面的内存态列表里 -->
    <NCard size="small" title="后台应用 · 台账（gear app start / MCP gear_app(start)）" style="margin-top: 12px">
      <p class="text-3" style="margin-top: 0">
        这些是 <b>独立后台进程</b>的落盘台账，跨会话存活；与上方「受管 App」的内存态列表不是同一模型。
        点「实时日志」订阅该应用 <b>订阅之后新产生</b> 的 process / service 输出——
        <b>不读取日志文件、不回放历史</b>。
      </p>
      <NSpace style="margin-bottom: 10px" align="center">
        <NButton size="small" :loading="recordsLoading" @click="loadRecords">刷新台账</NButton>
        <span class="text-3">共 {{ records.length }} 条</span>
      </NSpace>
      <NDataTable
        v-if="records.length > 0"
        :columns="recordColumns"
        :data="records"
        :loading="recordsLoading"
        :bordered="false"
        :single-line="false"
        size="small"
        :scroll-x="900"
        :pagination="false"
        :row-key="(row: AppRecordDTO) => row.app_id"
      />
      <NEmpty
        v-else
        description="没有后台应用台账（用 gear app start 或 MCP gear_app(start) 启动后会出现在这里）"
      />
    </NCard>

    <NDrawer v-model:show="showLogs" :width="720" placement="right">
      <NDrawerContent :title="`实时日志 · ${logApp?.app_id ?? ''}`" closable>
        <NSpace align="center" :size="8" style="margin-bottom: 10px">
          <NSwitch v-model:value="liveOn" @update:value="(v: boolean) => toggleLive(v)" />
          <span>实时（关掉即断开 SSE，不清空已收到的行）</span>
          <NButton size="small" quaternary @click="logLines = []">清空</NButton>
        </NSpace>

        <NAlert
          v-if="logApp && !logApp.live_logs"
          type="warning"
          :bordered="false"
          style="margin-bottom: 10px"
        >
          这个应用启动时管理端没在监听（或地址已失效），<b>未接入实时日志</b>，本面板不会有数据。
          请先确认 <code>gear serve</code> 在运行，再重启该应用；本地日志文件仍有完整输出
          （<code>{{ logApp.log_path }}</code>）。
        </NAlert>
        <NAlert v-if="logError" type="error" :bordered="false" style="margin-bottom: 10px">
          实时流断开：{{ logError }}
        </NAlert>
        <NAlert v-if="logClosed" type="info" :bordered="false" style="margin-bottom: 10px">
          应用已结束，实时流关闭。
        </NAlert>

        <p class="text-3" style="margin: 0 0 6px">
          只显示订阅后新产生的行（process / service 的 stdout / stderr），不读日志文件、不回放历史。
        </p>
        <div class="log-view">
          <div v-for="(l, i) in logLines" :key="i" :class="l.stream === 'stderr' ? 'log-err' : ''">
            <span class="text-3">{{ l.step_id ? l.step_id + ' · ' : '' }}</span>{{ l.line }}
          </div>
          <span v-if="logLines.length === 0" class="text-3">
            {{ liveOn ? '已连接，等待新日志…（Tool / Flow 产生输出时才会出现）' : '实时开关已关闭' }}
          </span>
        </div>
      </NDrawerContent>
    </NDrawer>
  </div>
</template>

<style scoped>
/* .mono 已收归 src/styles/typography.css 的 .text-mono */
.log-view {
  height: calc(100vh - 280px);
  min-height: 240px;
  overflow: auto;
  padding: 10px;
  border-radius: 4px;
  background: rgba(255, 255, 255, 0.04);
  font-family: var(--font-mono, monospace);
  font-size: 12px;
  line-height: 1.65;
  white-space: pre-wrap;
  word-break: break-all;
}
.log-err {
  color: #f2777a;
}
</style>

<style>
/* 打开地址链接（render 函数产出的 <a> 不受 scoped 影响，故用全局样式）。 */
.open-link {
  color: var(--primary-color, #2080f0);
  text-decoration: none;
  word-break: break-all;
}
.open-link:hover {
  text-decoration: underline;
}
.open-links {
  display: flex;
  flex-direction: column;
  gap: 2px;
}
</style>
