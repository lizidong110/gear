import { defineStore } from 'pinia'
import { computed, ref } from 'vue'
import { endpoints } from '@/api/endpoints'
import { getApiBase, isCrossOrigin, setApiBase } from '@/api/base'
import type { HealthzDTO } from '@/api/types'

/** 连接与内核健康：UI 的「我接的是哪个内核、它还活着吗」。 */
export const useConnStore = defineStore('conn', () => {
  const base = ref(getApiBase())
  const health = ref<HealthzDTO | null>(null)
  const online = ref(false)
  const lastError = ref('')
  const checking = ref(false)

  const displayBase = computed(() => base.value || '同源（/api 由代理转发）')
  const crossOrigin = computed(() => isCrossOrigin())

  async function ping(): Promise<boolean> {
    checking.value = true
    try {
      health.value = await endpoints.healthz()
      online.value = true
      lastError.value = ''
      return true
    } catch (e) {
      online.value = false
      health.value = null
      lastError.value = (e as Error).message
      return false
    } finally {
      checking.value = false
    }
  }

  function updateBase(v: string): void {
    base.value = v.trim()
    setApiBase(base.value)
    void ping()
  }

  return { base, health, online, lastError, checking, displayBase, crossOrigin, ping, updateBase }
})
