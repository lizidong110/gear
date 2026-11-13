import { defineStore } from 'pinia'
import { ref } from 'vue'
import { endpoints } from '@/api/endpoints'
import type { AppDTO } from '@/api/types'

/**
 * 受管长驻 App（POST /api/apps ⇒ `gear app start` 的 REST 包装，REQ-APP-MANAGED-DETACHED）。
 * 列表是落盘台账的实时探活投影：app 是 detached 独立进程，重启 gear serve 不带走应用。
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
