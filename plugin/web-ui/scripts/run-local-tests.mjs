// 在无依赖环境下直接跑 spec 文件（应急验证通道）。
//
// 用法：node --experimental-strip-types scripts/run-local-tests.mjs
// 退出码 0 = 全部通过。**这不是 vitest 的替代品**：只在 `pnpm install` 不可用时
// 用它证明核心逻辑（依赖推断/预检/契约映射）确实成立。
import { register } from 'node:module'
import { localSummary } from './local-test-shim.mjs'

register('./local-test-loader.mjs', import.meta.url)

const specs = [
  '../src/logic/levels.spec.ts',
  '../src/logic/template.spec.ts',
  '../src/logic/contract.spec.ts',
]

for (const s of specs) {
  await import(s)
}

process.exit(localSummary() ? 0 : 1)
