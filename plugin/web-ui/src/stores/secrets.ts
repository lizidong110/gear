import { defineStore } from 'pinia'
import { ref } from 'vue'
import { endpoints } from '@/api/endpoints'
import type { SecretListDTO } from '@/api/types'

/**
 * 密钥管理面（REQ-API-SECRETS）。
 *
 * 列表端点只回密钥名（不回明文、不回值）；写入经 PUT /api/secrets/{name}，
 * 响应刻意不回显 value（红线：任何响应不含明文）。运行期只读——Tool 内用
 * ${secrets.NAME} 或 ctx.secret.get 读取，管理面只负责「写入/列出/删除」。
 */
export const useSecretsStore = defineStore('secrets', () => {
  const names = ref<string[]>([])
  const loading = ref(false)
  const error = ref('')

  async function load(): Promise<void> {
    loading.value = true
    error.value = ''
    try {
      const r: SecretListDTO | undefined = await endpoints.listSecrets()
      names.value = r?.names ?? []
    } catch (e) {
      error.value = (e as Error).message
      throw e
    } finally {
      loading.value = false
    }
  }

  async function setSecret(name: string, value: string): Promise<void> {
    await endpoints.setSecret(name, value)
    await load()
  }

  async function removeSecret(name: string): Promise<void> {
    await endpoints.deleteSecret(name)
    await load()
  }

  return { names, loading, error, load, setSecret, removeSecret }
})
