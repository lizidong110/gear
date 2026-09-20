import { defineStore } from 'pinia'
import { computed, ref } from 'vue'
import { endpoints } from '@/api/endpoints'
import type { CatalogLike } from '@/logic/template'
import type { FlowDTO, HookDTO, ToolDTO } from '@/api/types'

/**
 * 注册表目录（Tools / Flows / Hooks）。
 *
 * 列表端点不分页、一次返回全量，所以这份本地索引是**主读法**（表单下拉、预检、
 * 表格都直接吃它）。内核另外提供 `GET /api/tools/{id}` 单查，用于「已知 id、只想拿
 * 那一个」的场合，不必为了一个 Tool 拉全表——两条路并存，不是替代关系。
 */
export const useCatalogStore = defineStore('catalog', () => {
  const tools = ref<ToolDTO[]>([])
  const flows = ref<FlowDTO[]>([])
  const hooks = ref<HookDTO[]>([])
  const loading = ref(false)
  const error = ref('')

  async function loadAll(): Promise<void> {
    loading.value = true
    error.value = ''
    try {
      const [t, f, h] = await Promise.all([endpoints.listTools(), endpoints.listFlows(), endpoints.listHooks()])
      tools.value = t ?? []
      flows.value = f ?? []
      hooks.value = h ?? []
    } catch (e) {
      error.value = (e as Error).message
      throw e
    } finally {
      loading.value = false
    }
  }

  /** 预检用的只读目录视图（Tool 存在性 + 钉版本存在性）。 */
  const precheckCatalog = computed<CatalogLike>(() => {
    const toolIds = new Set<string>()
    const toolVersions = new Map<string, string[]>()
    for (const t of tools.value) {
      toolIds.add(t.id)
      toolVersions.set(t.id, (t.versions ?? []).map((v) => v.version))
    }
    return { toolIds, toolVersions }
  })

  const flowById = computed(() => new Map(flows.value.map((f) => [f.id, f])))
  const toolById = computed(() => new Map(tools.value.map((t) => [t.id, t])))

  // ---- 写操作：一律「调内核 → 用内核返回体替换本地」 ----
  // 不乐观更新：内核会对定义做规范化/拒绝，本地猜测会与内核状态漂移。

  async function saveTool(t: ToolDTO): Promise<ToolDTO> {
    const saved = await endpoints.createTool(t)
    await refreshTools()
    return saved
  }

  async function removeTool(id: string): Promise<void> {
    await endpoints.deleteTool(id)
    await refreshTools()
  }

  async function promote(toolId: string, version: string, target: string): Promise<void> {
    await endpoints.promoteTool(toolId, version, target)
    await refreshTools()
  }

  async function saveFlow(f: FlowDTO): Promise<FlowDTO> {
    const saved = await endpoints.createFlow(f)
    await refreshFlows()
    return saved
  }

  /** 注销 Flow。内核守卫可能回 409（被引用 / 正在跑）——错误原样抛给调用方呈现。 */
  async function removeFlow(id: string): Promise<void> {
    await endpoints.deleteFlow(id)
    await refreshFlows()
  }

  async function saveHook(h: HookDTO): Promise<HookDTO> {
    const saved = await endpoints.createHook(h)
    await refreshHooks()
    return saved
  }

  /** 删除 Hook。不存在时内核回 404，原样抛出（不做「404 当成功」的静默降级）。 */
  async function removeHook(id: string): Promise<void> {
    await endpoints.deleteHook(id)
    await refreshHooks()
  }

  async function refreshTools(): Promise<void> {
    tools.value = (await endpoints.listTools()) ?? []
  }
  async function refreshFlows(): Promise<void> {
    flows.value = (await endpoints.listFlows()) ?? []
  }
  async function refreshHooks(): Promise<void> {
    hooks.value = (await endpoints.listHooks()) ?? []
  }

  return {
    tools,
    flows,
    hooks,
    loading,
    error,
    loadAll,
    refreshTools,
    refreshFlows,
    refreshHooks,
    precheckCatalog,
    flowById,
    toolById,
    saveTool,
    removeTool,
    promote,
    saveFlow,
    removeFlow,
    saveHook,
    removeHook,
  }
})
