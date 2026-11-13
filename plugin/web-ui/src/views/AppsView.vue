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
import type { AppDTO, PruneAppsRespDTO } from '@/api/types'

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
    // （收尾步可能稍晚才在日志里打 GEAR_OPEN_URL 标记，列表每 5s 轮询会自然补全）。
    const a = apps.apps.find((x) => x.flow_id === flowId.value && x.state === 'running')
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
    content: `按台账停止 ${a.app_id}（${a.flow_id}）：杀掉整个进程树并复检端口。收不干净会如实报错。`,
    positiveText: '停止',
    negativeText: '取消',
    onPositiveClick: async () => {
      try {
        await apps.stop(a.app_id)
        ok(`已停止 ${a.app_id}`)
      } catch (e) {
        report(e, '停止 App')
      }
    },
  })
}

// ---- 清理应用台账（POST /api/apps/prune）：只删确定已终止的记录，不杀进程、不碰端口。 ----
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

// 状态 → 标签颜色：running 绿；unknown 黄（双判据矛盾，不猜）；exited / startup_failed /
// restart_exhausted 红（后者是「被反复弄死到放弃重启」，必须与正常退出区分开）；
// stopped / starting 灰（终态 / 还没起完）。
function stateTagType(state: string): 'success' | 'warning' | 'error' | 'default' {
  if (state === 'running') return 'success'
  if (state === 'unknown') return 'warning'
  if (state === 'exited' || state === 'startup_failed' || state === 'restart_exhausted') return 'error'
  return 'default'
}

const columns = computed<DataTableColumns<AppDTO>>(() => [
  { title: 'app id', key: 'app_id', width: 200, render: (row) => h('span', { class: 'text-mono' }, row.app_id) },
  { title: 'Flow', key: 'flow_id', width: 180, render: (row) => h('span', { class: 'text-mono' }, row.flow_id) },
  // 台账双判据（pid 存活 + 端口探活）实时算出的状态，服务端每轮刷新都重算，不缓存不假绿。
  {
    title: '状态',
    key: 'state',
    width: 110,
    render: (row) =>
      h(
        NTag,
        { size: 'small', type: stateTagType(row.state), bordered: false, title: `PID ${row.pid}` },
        () => row.state,
      ),
  },
  { title: 'PID', key: 'pid', width: 80, render: (row) => String(row.pid) },
  {
    title: 'restart',
    key: 'restart',
    width: 100,
    render: (row) => h(NTag, { size: 'small', type: 'info', bordered: false }, () => row.restart ?? '—'),
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
    title: '实时日志',
    key: 'live_logs',
    width: 100,
    render: (row) =>
      h(
        NTag,
        { size: 'small', type: row.live_logs ? 'success' : 'warning', bordered: false },
        () => (row.live_logs ? '已接入' : '未接入'),
      ),
  },
  {
    title: '启动时刻',
    key: 'started_at',
    width: 170,
    render: (row) => new Date(row.started_at).toLocaleString(),
  },
  {
    title: '已运行',
    key: 'uptime',
    width: 90,
    // 已退出 / 已停止的没有「正在运行多久」可算，显示 —（继续走表会误读成还在跑）。
    render: (row) => (row.state === 'running' ? uptime(row) : h('span', { class: 'text-3' }, '—')),
  },
  {
    title: '打开地址',
    key: 'open_urls',
    width: 220,
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
    width: 170,
    render: (row) =>
      h('div', { style: 'display:flex;gap:4px' }, [
        h(NButton, { size: 'small', quaternary: true, onClick: () => openLogDrawer(row) }, () => '实时日志'),
        h(
          NButton,
          { size: 'small', type: 'error', quaternary: true, disabled: row.state === 'stopped' || row.state === 'startup_failed' || row.state === 'restart_exhausted', onClick: () => confirmStop(row) },
          () => '停止',
        ),
      ]),
  },
])

// ---- 实时日志（REQ-API-APP-LOGS）----
// 订阅对象即上面台账表的行。SSE 只推订阅后新产生的行，绝不读 <appID>.log 回放历史。
const MAX_LOG_LINES = 500
const showLogs = ref(false)
const logApp = ref<AppDTO | null>(null)
const liveOn = ref(false)
const logLines = ref<LiveLog[]>([])
const logError = ref('')
const logClosed = ref(false)
let logAbort: AbortController | null = null

// ---- 调用详情展开（REQ-APP-LOGS-DIRECT-TOOL 的 GEAR_TOOL_CALL 详情标记）----
// serve 在回推上下文里给 ✓/✗ Tool 行尾附加单行 JSON（入参 + 返回 + 错误）。
// UI 把行拆成「摘要文本 + 详情」，点击行切换展开/折叠；解析失败按普通行渲染。
interface ToolCallDetail {
  kind?: string
  id?: string
  ok?: boolean
  ms?: number
  input?: unknown
  output?: unknown
  error?: string
}
const TOOL_CALL_MARKER = '\tGEAR_TOOL_CALL='
const expandedLogRows = ref(new Set<number>())

function splitToolCallDetail(line: string): { text: string; detail: ToolCallDetail | null } {
  const idx = line.indexOf(TOOL_CALL_MARKER)
  if (idx < 0) return { text: line, detail: null }
  try {
    return {
      text: line.slice(0, idx),
      detail: JSON.parse(line.slice(idx + TOOL_CALL_MARKER.length)) as ToolCallDetail,
    }
  } catch {
    return { text: line, detail: null } // 残缺标记（如被截断）按普通行渲染
  }
}

// 渲染视图：解析一次，模板不重复 split。
const renderedLogLines = computed(() =>
  logLines.value.map((l, i) => {
    const { text, detail } = splitToolCallDetail(l.line)
    return { ...l, index: i, text, detail }
  }),
)

function toggleLogRow(i: number) {
  const next = new Set(expandedLogRows.value)
  if (next.has(i)) next.delete(i)
  else next.add(i)
  expandedLogRows.value = next
}

function prettyJSON(v: unknown): string {
  if (v === null || v === undefined) return '—'
  try {
    return JSON.stringify(v, null, 2)
  } catch {
    return String(v)
  }
}

function clearLogs() {
  logLines.value = []
  expandedLogRows.value = new Set<number>()
}

function stopAppLogStream() {
  if (logAbort) {
    logAbort.abort()
    logAbort = null
  }
}

function startAppLogStream() {
  const a = logApp.value
  if (!a) return
  stopAppLogStream()
  logError.value = ''
  logClosed.value = false
  const ctrl = new AbortController()
  logAbort = ctrl
  void streamAppLogs(
    a.app_id,
    ctrl.signal,
    (l: LiveLog) => {
      logLines.value.push(l)
      if (logLines.value.length > MAX_LOG_LINES) {
        logLines.value.splice(0, logLines.value.length - MAX_LOG_LINES)
        expandedLogRows.value = new Set<number>() // 行号整体前移，展开状态不再可靠，统一收起
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

function openLogDrawer(row: AppDTO) {
  stopAppLogStream()
  logApp.value = row
  logLines.value = []
  expandedLogRows.value = new Set<number>()
  logError.value = ''
  logClosed.value = false
  showLogs.value = true
  liveOn.value = true
  startAppLogStream()
}

onMounted(async () => {
  try {
    await Promise.all([catalog.loadAll(), apps.loadList()])
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
      <code>POST /api/apps</code> 是 CLI <code>gear app start</code> 的 REST 包装：把一条含
      <b>service</b> step 的 Flow 派生为<b>独立后台进程</b>（脱离 serve 进程树），监管由它自己持有，
      台账落盘、跨会话存活；「停止」按台账杀整个进程树并复检端口。重启 <code>gear serve</code> 不会带走应用。
    </p>

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

    <NCard size="small" title="应用台账">
      <p class="text-3" style="margin-top: 0">
        这些是落盘台账的应用（含 CLI <code>gear app start</code> / MCP <code>gear_app(start)</code> /
        本页托管启动的所有来源）。状态由服务端按 <b>pid 存活 + 端口探活</b> 实时算出；
        点「实时日志」订阅该应用<b>订阅之后新产生</b>的 process / service 输出——
        <b>不读取日志文件、不回放历史</b>。
      </p>
      <NDataTable
        v-if="apps.apps.length > 0"
        :columns="columns"
        :data="apps.apps"
        :loading="apps.loading"
        :bordered="false"
        :single-line="false"
        size="small"
        :scroll-x="1220"
        :pagination="false"
        :row-key="(row: AppDTO) => row.app_id"
      />
      <NEmpty
        v-else
        description="台账里没有应用（托管启动或 gear app start 之后会出现在这里）"
      />
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

    <NDrawer v-model:show="showLogs" :width="720" placement="right">
      <NDrawerContent :title="`实时日志 · ${logApp?.app_id ?? ''}`" closable>
        <NSpace align="center" :size="8" style="margin-bottom: 10px">
          <NSwitch v-model:value="liveOn" @update:value="(v: boolean) => toggleLive(v)" />
          <span>实时（关掉即断开 SSE，不清空已收到的行）</span>
          <NButton size="small" quaternary @click="clearLogs">清空</NButton>
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
          带 ▸ 的 ✓/✗ Tool 行<b>可点击展开</b>入参与返回结果，再点折叠。
        </p>
        <div class="log-view">
          <template v-for="l in renderedLogLines" :key="l.index">
            <div
              v-if="l.detail"
              class="log-row"
              :title="l.detail.error ? '点击展开完整错误' : '点击展开入参与返回'"
              @click="toggleLogRow(l.index)"
            >
              <span class="chev">{{ expandedLogRows.has(l.index) ? '▾' : '▸' }}</span>
              <span :class="l.stream === 'stderr' ? 'log-err' : ''"
                ><span class="text-3">{{ l.step_id ? l.step_id + ' · ' : '' }}</span
                >{{ l.text }}</span
              >
            </div>
            <div v-else :class="l.stream === 'stderr' ? 'log-err' : ''">
              <span class="text-3">{{ l.step_id ? l.step_id + ' · ' : '' }}</span>{{ l.line }}
            </div>
            <template v-if="l.detail && expandedLogRows.has(l.index)">
              <pre v-if="l.detail.input != null" class="log-detail">入参：{{ prettyJSON(l.detail.input) }}</pre>
              <pre v-if="l.detail.output != null" class="log-detail">返回：{{ prettyJSON(l.detail.output) }}</pre>
              <pre v-if="l.detail.error" class="log-detail log-err">错误：{{ l.detail.error }}</pre>
            </template>
          </template>
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
/* ✓/✗ Tool 行可点击展开（GEAR_TOOL_CALL 详情标记） */
.log-row {
  cursor: pointer;
  border-radius: 3px;
}
.log-row:hover {
  background: rgba(255, 255, 255, 0.06);
}
.chev {
  display: inline-block;
  width: 13px;
  color: var(--text-color-3, #999);
  user-select: none;
}
.log-detail {
  margin: 2px 0 6px 13px;
  padding: 8px 10px;
  border-radius: 4px;
  background: rgba(255, 255, 255, 0.06);
  font-size: 11px;
  line-height: 1.5;
  white-space: pre-wrap;
  word-break: break-all;
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
