// 无依赖语法闸门：用 Node 自带的 TS 转换器解析全部源码，抓语法错误。
//
// 存在的理由：本机安装依赖会被环境垫片破坏（见 README「本机环境」），
// 但「装不上依赖」不能成为「没检查过」的借口 —— 语法错误与拼写错误这类问题
// 不需要任何第三方库就能查出来。类型检查与模板检查仍需 vue-tsc（装好依赖后）。
//
// .vue 文件单独处理：抽出 <script setup> 块按 TS 解析（模板部分此处不覆盖）。
import { existsSync, readFileSync, readdirSync, statSync } from 'node:fs'
import { dirname, join, relative } from 'node:path'
import { fileURLToPath } from 'node:url'
import { stripTypeScriptTypes } from 'node:module'

const ROOT = join(dirname(fileURLToPath(import.meta.url)), '..')
const SRC = join(ROOT, 'src')

function walk(dir) {
  const out = []
  for (const name of readdirSync(dir)) {
    const p = join(dir, name)
    if (statSync(p).isDirectory()) out.push(...walk(p))
    else out.push(p)
  }
  return out
}

const files = walk(SRC)
let checked = 0
const problems = []
const SUFFIXES = ['', '.ts', '.vue', '.mjs', '/index.ts', '/index.vue']

/** 相对 src 解析一个导入路径是否存在（补后缀）。 */
function importExists(spec, fromFile) {
  const bases = []
  if (spec.startsWith('@/')) bases.push(join(SRC, spec.slice(2)))
  else if (spec.startsWith('./') || spec.startsWith('../')) bases.push(join(dirname(fromFile), spec))
  else return true // 裸包名（vue / naive-ui / …）：没有 node_modules 无从判定，跳过
  for (const b of bases) {
    for (const ext of SUFFIXES) {
      if (existsSync(b + ext)) return true
    }
  }
  return false
}

/** 比对导入路径是否真的存在 —— 没有类型检查时，这类拼写错误最难发现。 */
function checkImports(rel, code, file) {
  const re = /from\s+['"]([^'"]+)['"]/g
  let m
  while ((m = re.exec(code)) !== null) {
    const spec = m[1]
    if ((spec.startsWith('@/') || spec.startsWith('./') || spec.startsWith('../')) && !importExists(spec, file)) {
      problems.push(`${rel}: 导入了不存在的模块 '${spec}'`)
    }
  }
}

for (const f of files) {
  const rel = relative(ROOT, f).replace(/\\/g, '/')
  try {
    if (f.endsWith('.ts')) {
      const code = readFileSync(f, 'utf8')
      stripTypeScriptTypes(code, { mode: 'transform', sourceUrl: rel })
      checkImports(rel, code, f)
      checked += 1
    } else if (f.endsWith('.vue')) {
      const src = readFileSync(f, 'utf8')
      const m = src.match(/<script setup lang="ts">([\s\S]*?)<\/script>/)
      if (!m) {
        problems.push(`${rel}: 未找到 <script setup lang="ts"> 块（规范要求所有视图使用 setup + TS）`)
        continue
      }
      // 模板里的 `${}` 会被当成模板字符串 —— 这里只解析 script 块，故直接转换。
      stripTypeScriptTypes(m[1], { mode: 'transform', sourceUrl: rel })
      checkImports(rel, m[1], f)
      checked += 1
    }
  } catch (e) {
    problems.push(`${rel}: ${e.message.split('\n')[0]}`)
  }
}

console.log(`语法检查：解析 ${checked} 个文件（.ts + .vue 的 script 块），并校验了导入路径是否存在`)
if (problems.length > 0) {
  console.error(`\n发现 ${problems.length} 个问题：`)
  for (const p of problems) console.error(`  ✗ ${p}`)
  process.exit(1)
}
console.log('全部通过。注意：这**不是**类型检查 —— 类型与模板的正确性需装好依赖后跑 vue-tsc。')
