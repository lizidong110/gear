// 契约漂移守卫（红线 15 的前端版本）。
//
// 判定方式：重新生成一遍枚举产物，与仓库里已提交的 src/logic/generated/enums.ts 逐字节比对。
// 不一致即失败 —— 意味着有人改了 Go 的枚举却没同步前端。
//
// 这与内核 `gofmt -l 零差异` 是同一类闸门：产物必须可被源复现，否则 CI 红。
// 退出码：0 = 一致；1 = 漂移；2 = 解析失败（源文件结构变了）。

import { readFileSync, existsSync } from 'node:fs'
import { generate, OUT_FILE, GO_SOURCES } from './gen-enums.mjs'

let expected
try {
  expected = generate()
} catch (e) {
  console.error('[契约守卫] 解析内核源码失败:', e.message)
  for (const p of GO_SOURCES) console.error(`  源文件: ${p}`)
  process.exit(2)
}

if (!existsSync(OUT_FILE)) {
  console.error(`[契约守卫] 产物缺失: ${OUT_FILE}`)
  console.error('  修复: npm run gen:enums')
  process.exit(1)
}

const actual = readFileSync(OUT_FILE, 'utf8')

if (actual !== expected) {
  console.error('[契约守卫] 失败：前端枚举与内核枚举不一致（漂移）。')
  console.error(`  内核源: ${GO_SOURCES.join(' / ')}`)
  console.error(`  前端产物: ${OUT_FILE}`)
  const a = actual.split('\n')
  const b = expected.split('\n')
  const n = Math.max(a.length, b.length)
  for (let i = 0; i < n; i++) {
    if (a[i] !== b[i]) {
      console.error(`  首个差异在第 ${i + 1} 行:`)
      console.error(`    仓库中: ${a[i] ?? '<无此行>'}`)
      console.error(`    期望值: ${b[i] ?? '<无此行>'}`)
      break
    }
  }
  console.error('  修复: npm run gen:enums（然后把产物一起提交）')
  process.exit(1)
}

console.log('[契约守卫] 通过：前端枚举与内核 model.go / cap.go 完全一致。')
