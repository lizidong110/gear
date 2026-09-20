// 内核地址解析：空串 = 走同源（开发期由 Vite 代理、生产由反向代理转发 /api）。
//
// 为什么不默认写死 http://127.0.0.1:8787：那样浏览器会真的发起跨域请求，
// 而内核**没有 CORS 头**（红线：本插件零内核侵入，不要求内核为 UI 开后门）。
// 默认同源 + 反代是唯一不需要改内核的路径；确有需要时用户可在此显式填 origin。
const KEY = 'gear.apiBase'

export function getApiBase(): string {
  try {
    return localStorage.getItem(KEY) ?? ''
  } catch {
    return ''
  }
}

export function setApiBase(v: string): void {
  try {
    if (v) localStorage.setItem(KEY, v)
    else localStorage.removeItem(KEY)
  } catch {
    /* 隐私模式下 localStorage 不可用：退化为「本次会话内同源」 */
  }
}

/** 内核地址是否被用户显式覆盖成跨域 origin。 */
export function isCrossOrigin(): boolean {
  const v = getApiBase()
  if (!v) return false
  try {
    return new URL(v, location.href).origin !== location.origin
  } catch {
    return false
  }
}
