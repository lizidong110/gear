// 解析钩子：让真实 spec 文件在没有 node_modules 的情况下也能跑。
//   - 'vitest'                        → scripts/local-test-shim.mjs
//   - '@/xxx'                         → src/xxx（补后缀）
//   - './xxx' / '../xxx'（无后缀）     → 相对当前文件补 .ts / .vue
// 只在本地应急验证时使用；正常流程仍应 `pnpm install` 后跑真正的 vitest。
import { existsSync } from 'node:fs'
import { fileURLToPath, pathToFileURL } from 'node:url'

const HERE = new URL('.', import.meta.url)
const SRC = new URL('../src/', HERE)
const SUFFIXES = ['.ts', '.vue', '.mjs', '/index.ts', '']

function firstExisting(baseUrl) {
  for (const ext of SUFFIXES) {
    try {
      const p = fileURLToPath(new URL(baseUrl + ext))
      if (existsSync(p)) return pathToFileURL(p).href
    } catch {
      /* 无效 URL 组合直接跳过 */
    }
  }
  return null
}

export async function resolve(specifier, context, next) {
  if (specifier === 'vitest') {
    return { url: new URL('./local-test-shim.mjs', HERE).href, shortCircuit: true }
  }
  if (specifier.startsWith('@/')) {
    const url = firstExisting(new URL(specifier.slice(2), SRC))
    if (url) return { url, shortCircuit: true }
  }
  // 项目内源码用「无后缀相对导入」（bundler 风格），Node ESM 直接跑需要补后缀。
  if ((specifier.startsWith('./') || specifier.startsWith('../')) && !/\.[a-z]+$/i.test(specifier)) {
    const parent = context.parentURL ?? import.meta.url
    const url = firstExisting(new URL(specifier, parent))
    if (url) return { url, shortCircuit: true }
  }
  return next(specifier, context)
}
