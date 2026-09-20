import { computed, ref } from 'vue'
import { defineStore } from 'pinia'
import { endpoints } from '@/api/endpoints'
import type { CapabilityDTO, CapabilityInvokeReq } from '@/api/types'

/**
 * 原子能力目录（只读、无副作用）。
 *
 * 与 CLI `gear ai capabilities`、REST `GET /api/capabilities` 同源 —— 全部来自
 * engine.AIAgent().CapabilitiesMethod()：内置 6 原语 + 已注册 ext.*，每个能力含
 * 方法级参数 schema。回答「有哪些原子能力 / 扩展了哪些 / 各能力要什么参数」。
 *
 * 能力目录是内核启动期确定的（扩展在 serve 时加载），运行时不会变，因此不乐观更新、
 * 只整份替换；轮询仅用于在「重连到另一个内核 / 内核热重载扩展」后能自动刷新。
 */
export const useCapabilitiesStore = defineStore('capabilities', () => {
  const caps = ref<CapabilityDTO[]>([])
  const loading = ref(false)
  const error = ref('')

  async function load(signal?: AbortSignal): Promise<void> {
    loading.value = true
    error.value = ''
    try {
      caps.value = (await endpoints.capabilities(signal)) ?? []
    } catch (e) {
      error.value = (e as Error).message
      throw e
    } finally {
      loading.value = false
    }
  }

  const builtinCaps = computed(() => caps.value.filter((c) => c.kind === 'builtin'))
  const extCaps = computed(() => caps.value.filter((c) => c.kind === 'ext'))

  /** 总方法数（用来给摘要行一个直观数字）。 */
  const totalMethods = computed(() =>
    caps.value.reduce((n, c) => n + Object.keys(c.methods ?? {}).length, 0),
  )

  // ---- 轮询：用于「切内核 / 内核热重载扩展」后自动刷新（非高频）。 ----
  let timer: ReturnType<typeof setInterval> | null = null
  function startPolling(intervalMs = 15000): void {
    stopPolling()
    timer = setInterval(() => void load().catch(() => {}), intervalMs)
  }
  function stopPolling(): void {
    if (timer !== null) {
      clearInterval(timer)
      timer = null
    }
  }

  /** 直接执行一个原子能力的方法（POST /api/capabilities/invoke）。返回该方法的原始结果。 */
  async function invoke(req: Omit<CapabilityInvokeReq, never>): Promise<unknown> {
    const resp = await endpoints.invokeCapability(req)
    return resp.result
  }

  return {
    caps,
    loading,
    error,
    builtinCaps,
    extCaps,
    totalMethods,
    load,
    invoke,
    startPolling,
    stopPolling,
  }
})
