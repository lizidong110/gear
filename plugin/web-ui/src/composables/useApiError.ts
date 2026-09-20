import { useMessage, useNotification } from 'naive-ui'
import { ApiError } from '@/api/client'

/**
 * 统一的错误呈现：**绝不吞内核原文**。
 *
 * 内核的错误文案是排障的第一手线索（「step s1 引用的 Tool 不存在: nope」比
 * 「请求失败」有用一个数量级），所以通知里正文放内核原文、副标题放状态码分类与行动指引。
 */
export function useApiError() {
  const message = useMessage()
  const notify = useNotification()

  function describe(e: unknown): { title: string; body: string; status: number } {
    if (e instanceof ApiError) {
      return { title: `HTTP ${e.status || '—'} · ${e.hint}`, body: e.message, status: e.status }
    }
    return { title: '未预期的错误', body: (e as Error)?.message ?? String(e), status: -1 }
  }

  /** 写操作失败（注册/注销/晋升/触发）：用 notification，内容长也不怕。 */
  function report(e: unknown, action: string): void {
    const d = describe(e)
    notify.error({
      title: `${action}失败`,
      content: d.body,
      meta: d.title,
      duration: 8000,
      keepAliveOnHover: true,
    })
  }

  /** 提示成功。 */
  function ok(text: string): void {
    message.success(text)
  }

  /** 轻量失败（如列表刷新失败）：用 message 即可，不打断操作。 */
  function brief(e: unknown, action: string): void {
    const d = describe(e)
    message.error(`${action}失败：${d.body.slice(0, 120)}`, { duration: 6000 })
  }

  return { report, ok, brief, describe }
}
