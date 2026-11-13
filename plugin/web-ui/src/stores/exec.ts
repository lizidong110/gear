import { defineStore } from 'pinia'
import { computed, ref } from 'vue'
import { endpoints, streamExecutionLogs, type ExecLog, type LiveLog } from '@/api/endpoints'
import type { ExecutionDTO } from '@/api/types'

/**
 * 执行记录与日志。
 *
 * 内核执行是**异步**的（POST /api/executions 回 202 + id），且没有任何推送通道
 * （无 SSE / WebSocket），所以「实时」只能靠轮询：
 *   - 列表：仅当存在 running 记录时轮询（没有在跑的就别骚扰内核）
 *   - 详情 + 日志：仅当被查看的那条是 running 时轮询 1s
 * 轮询必须在视图卸载时停掉，否则切页后还在打接口。
 */
export const useExecStore = defineStore('exec', () => {
  const executions = ref<ExecutionDTO[]>([])
  const loading = ref(false)
  const error = ref('')

  const selected = ref<ExecutionDTO | null>(null)
  const logs = ref<ExecLog[]>([])
  const logsError = ref('')

  /** 实时日志开关（REQ-API-LOGS-STREAM）：开 = SSE 流推新行；关 = 历史轮询。 */
  const realtime = ref(false)
  let streamAbort: AbortController | null = null

  let listTimer: ReturnType<typeof setInterval> | null = null
  let detailTimer: ReturnType<typeof setInterval> | null = null

  const runningCount = computed(() => executions.value.filter((e) => e.status === 'running').length)
  const hasRunning = computed(() => runningCount.value > 0)

  const sorted = computed(() =>
    [...executions.value].sort((a, b) => Date.parse(b.started_at) - Date.parse(a.started_at)),
  )

  async function loadList(): Promise<void> {
    loading.value = true
    try {
      executions.value = (await endpoints.listExecutions()) ?? []
      error.value = ''
    } catch (e) {
      error.value = (e as Error).message
    } finally {
      loading.value = false
    }
  }

  /** 拉详情并写入 selected，**同时返回**读到的记录 —— 见 open() 里的说明。 */
  async function loadDetail(id: string): Promise<ExecutionDTO> {
    const detail = await endpoints.getExecution(id)
    selected.value = detail
    return detail
  }

  async function loadLogs(id: string): Promise<void> {
    try {
      logs.value = (await endpoints.executionLogs(id)) ?? []
      logsError.value = ''
    } catch (e) {
      logsError.value = (e as Error).message
    }
  }

  async function trigger(flowId: string): Promise<string> {
    const res = await endpoints.createExecution(flowId)
    await loadList()
    return res.id
  }

  /** 打开某条执行：拉详情 + 日志，并在其仍 running 时续轮询。 */
  async function open(id: string): Promise<void> {
    stopDetailPolling()
    stopLogStream()
    selected.value = null
    logs.value = []
    logsError.value = ''
    // 用 loadDetail 的**返回值**判，而不是再读 selected.value：
    // 上面 `selected.value = null` 的赋值会被 TS 的控制流分析一路带到这里，
    // 于是 `selected.value` 被窄化成 null（它不认为 await 里的赋值会改回来），
    // 取 `?.status` 就得到 never。让函数把读到的记录返回出来，既过类型也更直白。
    const [detail] = await Promise.all([loadDetail(id), loadLogs(id)])
    if (detail && detail.status === 'running') {
      detailTimer = setInterval(() => {
        void (async () => {
          const id2 = selected.value?.id
          if (!id2) return stopDetailPolling()
          try {
            await loadDetail(id2)
            // 实时模式下日志由 SSE 追加，不再用轮询覆盖（否则会把实时新行冲掉）。
            if (!realtime.value) await loadLogs(id2)
          } catch {
            stopDetailPolling()
          }
          if (selected.value?.status !== 'running') {
            stopDetailPolling()
            stopLogStream()
            void loadLogs(id2) // 终态后补一次完整历史，确保尾部无遗漏
          }
        })()
      }, 1000)
    }
  }

  /** 开始实时日志流：接 SSE，新行追加到 logs（不覆盖已载入的历史）。 */
  async function startLogStream(id: string): Promise<void> {
    stopLogStream()
    const ac = new AbortController()
    streamAbort = ac
    try {
      await streamExecutionLogs(
        id,
        ac.signal,
        (l: LiveLog) => {
          logs.value = [
            ...logs.value,
            { exec_id: selected.value?.id ?? id, step_id: l.step_id, stream: l.stream, line: l.line },
          ]
        },
        () => {
          // 流结束（执行终态 / 断连）：停流并补一次历史，确保尾部完整。
          stopLogStream()
          void loadLogs(id)
        },
      )
    } catch (e) {
      if (!ac.signal.aborted) logsError.value = (e as Error).message
    }
  }

  function stopLogStream(): void {
    if (streamAbort) {
      streamAbort.abort()
      streamAbort = null
    }
  }

  /** 实时开关：开 → 先补历史再接 SSE 流（停掉日志轮询）；关 → 停流并补一次历史。 */
  async function setRealtime(v: boolean): Promise<void> {
    realtime.value = v
    const id = selected.value?.id
    if (!id) return
    if (v) {
      await loadLogs(id)
      await startLogStream(id)
    } else {
      stopLogStream()
      await loadLogs(id)
    }
  }

  function startListPolling(intervalMs = 3000): void {
    stopListPolling()
    listTimer = setInterval(() => {
      if (document.hidden) return // 页面不可见时不打接口
      void loadList()
    }, intervalMs)
  }

  function stopListPolling(): void {
    if (listTimer !== null) {
      clearInterval(listTimer)
      listTimer = null
    }
  }

  function stopDetailPolling(): void {
    if (detailTimer !== null) {
      clearInterval(detailTimer)
      detailTimer = null
    }
  }

  function close(): void {
    stopDetailPolling()
    stopLogStream()
    realtime.value = false
    selected.value = null
    logs.value = []
  }

  function stopAll(): void {
    stopListPolling()
    stopDetailPolling()
    stopLogStream()
  }

  return {
    executions,
    loading,
    error,
    selected,
    logs,
    logsError,
    realtime,
    runningCount,
    hasRunning,
    sorted,
    loadList,
    loadDetail,
    loadLogs,
    trigger,
    open,
    close,
    setRealtime,
    startLogStream,
    stopLogStream,
    startListPolling,
    stopListPolling,
    stopDetailPolling,
    stopAll,
  }
})
