// HTTP 客户端：把内核的**语义化状态码**翻成用户能据以行动的话。
//
// 内核的状态码契约（server.go:statusForErr + 各 handler）：
//   200 正常 / 201 已创建 / 202 已受理（执行异步） / 400 注册期校验未通过 /
//   404 不存在 / 409 被引用不可注销（ErrInUse） / 429 入口繁忙（并发闸门背压） / 500 内核内部错误
//
// 设计取舍：**不吞内核的错误原文**。内核的错误文案本身就是最好的排障线索
// （例如「step s1 引用的 Tool 不存在: nope」「Flow 存在循环依赖」），
// 因此 ApiError.message 始终保留内核原文，hint 只做「这属于哪一类、下一步该做什么」的补充。

export class ApiError extends Error {
  readonly status: number
  readonly hint: string
  readonly body: string

  constructor(status: number, message: string, body = '') {
    super(message)
    this.name = 'ApiError'
    this.status = status
    this.hint = hintForStatus(status)
    this.body = body
  }
}

/** 状态码 → 行动指引（纯函数，单测覆盖）。 */
export function hintForStatus(status: number): string {
  switch (status) {
    case 0:
      return '连不上内核：确认 `gear serve` 已启动，且地址 / 代理配置正确'
    case 400:
      return '内核在注册期就拒绝了：按下面的原文改定义（这类错误不用等运行期才暴露）'
    case 404:
      return '对象不存在：可能已被删除，或 Flow 尚未注册'
    case 409:
      return '被引用中，不可注销：该 Tool 正被 Flow step / Hook action / 运行中的执行使用'
    case 422:
      return '语义校验未通过'
    case 429:
      return '执行入口繁忙（并发已达上限且队列已满，被背压拒绝）：稍后重试，或调大内核并发'
    case 500:
      return '内核内部错误：看 `gear serve` 的 stderr'
    default:
      return '请求失败'
  }
}

export class ApiClient {
  /** base 是内核 origin（如 http://127.0.0.1:8787）；空串表示走同源（反代/开发代理）。 */
  constructor(private readonly base: () => string = () => '') {}

  private url(path: string): string {
    const b = this.base().replace(/\/+$/, '')
    const p = path.startsWith('/') ? path : `/${path}`
    return `${b}/api${p}`
  }

  async request<T>(method: string, path: string, body?: unknown, signal?: AbortSignal): Promise<T> {
    let res: Response
    try {
      res = await fetch(this.url(path), {
        method,
        headers: body === undefined ? undefined : { 'Content-Type': 'application/json' },
        body: body === undefined ? undefined : JSON.stringify(body),
        signal,
      })
    } catch (e) {
      // 网络层失败（内核没起、被浏览器拦跨域、代理配错）——状态码 0 表达「没拿到 HTTP 响应」。
      throw new ApiError(0, (e as Error).message || '网络请求失败')
    }

    const text = await res.text()
    if (!res.ok) {
      throw new ApiError(res.status, extractError(text) ?? `HTTP ${res.status}`, text)
    }
    if (!text) return undefined as T
    try {
      return JSON.parse(text) as T
    } catch {
      throw new ApiError(res.status, `响应不是合法 JSON: ${text.slice(0, 200)}`, text)
    }
  }

  get<T>(path: string, signal?: AbortSignal) {
    return this.request<T>('GET', path, undefined, signal)
  }
  post<T>(path: string, body?: unknown) {
    return this.request<T>('POST', path, body ?? {})
  }
  put<T>(path: string, body?: unknown) {
    return this.request<T>('PUT', path, body ?? {})
  }
  del<T>(path: string) {
    return this.request<T>('DELETE', path)
  }
}

/** 内核错误体统一是 `{"error": "..."}`；解析失败则回退原文。 */
export function extractError(text: string): string | null {
  if (!text) return null
  try {
    const v = JSON.parse(text) as { error?: unknown }
    if (typeof v.error === 'string' && v.error) return v.error
  } catch {
    /* 非 JSON：回退原文 */
  }
  return text.length > 500 ? `${text.slice(0, 500)}…` : text
}
