import { defineStore } from 'pinia'
import { ref } from 'vue'
import { endpoints } from '@/api/endpoints'
import type { AppDTO } from '@/api/types'

/**
 * 受管长驻进程（POST /api/apps ⇒ 等价于 CLI `app run`，由 Supervisor 监管 service step）。
 * 注意：app 状态是内核**内存态**（server.apps map），重启 gear serve 后列表会空 —— 见 README 的状态持久性说明。
 */
export const useAppsStore = defineStore('apps', () => {
  const apps = ref<AppDTO[]>([])
  const loading = ref(false)
  const error = ref('')
  let timer: ReturnType<typeof setInterval> | null = null

  async function loadList(): Promise<void> {
    loading.value = true
    try {
      apps.value = (await endpoints.listApps()) ?? []
      error.value = ''
    } catch (e) {
      error.value = (e as Error).message
    } finally {
      loading.value = false
    }
  }

  async function start(flowId: string, restart: string): Promise<void> {
    await endpoints.createApp(flowId, restart)
    await loadList()
  }

  async function stop(id: string): Promise<void> {
    await endpoints.deleteApp(id)
    await loadList()
  }

  function startPolling(intervalMs = 5000): void {
    stopPolling()
    timer = setInterval(() => {
      if (document.hidden) return
      void loadList()
    }, intervalMs)
  }

  function stopPolling(): void {
    if (timer !== null) {
      clearInterval(timer)
      timer = null
    }
  }

  return { apps, loading, error, loadList, start, stop, startPolling, stopPolling }
})
