<script setup lang="ts">
// 只读编排图（一期：分层 DAG 可视化）。
//
// 分层语义**完全复用** logic/levels.ts（与内核 engine/deps.go 同算法）：
//   - 同一列 = 同一层 = 可并行执行（每列有一道淡色底轨，把「同层」直观地圈出来）
//   - 实线边 = 显式 depends_on；虚线边 = 推断出来的隐式依赖（input 的 ${steps.X} 或 when 里的 steps.X）
// 推断边必须画出来：内核会因它改变时序，用户看不见就会以为「我的 step 顺序是随意的」。
//
// 配色原则（第一版踩过坑，记在这免得再犯）：
//   深底 + 浅描边 + 浅字 = 在小尺寸下糊成一团，什么都看不清。
//   现在统一用**中间调实底 + 近白字**：底色与面板底（#18181c 一带）拉开明度差，
//   字与底之间留足对比；状态靠色相区分（青/琥珀/红），不靠深浅。
//
// 明确不做拖拽编辑（PRD §1.4：框架不做低代码拖拽 UI）。
import { computed } from 'vue'
import { NAlert, NEmpty } from 'naive-ui'
import { FlowGraphError, effectiveDeps, inferredEdges, topoLevels, type StepLike } from '@/logic/levels'
import type { RunResultDTO } from '@/api/types'

const props = defineProps<{
  steps: StepLike[]
  result?: RunResultDTO | null
}>()

const W = 170
const H = 54
const GAPX = 68
const GAPY = 20
const TOP = 34 // 给「第 N 层」的列标留出空间
const PAD = 16

interface GNode {
  id: string
  step: StepLike
  x: number
  y: number
  status: 'idle' | 'ok' | 'skipped' | 'notrun'
}
interface GEdge {
  key: string
  d: string
  inferred: boolean
}
interface Column {
  index: number
  x: number
  parallel: boolean
  count: number
}

const STATUS_STYLE = {
  idle: { fill: '#99999960', stroke: '#B4B2A9', label: '未运行' },
  ok: { fill: '#0F6E5660', stroke: '#9FE1CB', label: '已执行' },
  skipped: { fill: '#854F0B60', stroke: '#FAC775', label: '已跳过' },
  notrun: { fill: '#A32D2D60', stroke: '#F7C1C1', label: '未执行' },
} as const

const graph = computed<{
  nodes: GNode[]
  edges: GEdge[]
  columns: Column[]
  width: number
  height: number
  error: string
}>(() => {
  const steps = props.steps ?? []
  const empty = { nodes: [], edges: [], columns: [], width: 0, height: 0, error: '' }
  if (steps.length === 0) return empty

  let levels: string[][]
  let deps: Record<string, string[]>
  try {
    levels = topoLevels(steps)
    deps = effectiveDeps(steps)
  } catch (e) {
    if (e instanceof FlowGraphError) return { ...empty, error: e.message }
    throw e
  }

  const slot = new Map<string, { col: number; row: number }>()
  levels.forEach((lvl, c) => lvl.forEach((id, r) => slot.set(id, { col: c, row: r })))
  const maxRows = levels.reduce((m, l) => Math.max(m, l.length), 1)

  const pos = new Map<string, { x: number; y: number }>()
  for (const s of steps) {
    const p = slot.get(s.id)
    if (p) pos.set(s.id, { x: PAD + p.col * (W + GAPX), y: TOP + p.row * (H + GAPY) })
  }

  const inferred = new Set(inferredEdges(steps).map((e) => `${e.from}->${e.to}`))
  const edges: GEdge[] = []
  for (const s of steps) {
    const p = pos.get(s.id)
    if (!p) continue
    for (const d of deps[s.id] ?? []) {
      const fp = pos.get(d)
      if (!fp) continue
      const x1 = fp.x + W
      const y1 = fp.y + H / 2
      const x2 = p.x
      const y2 = p.y + H / 2
      const mid = (x1 + x2) / 2
      edges.push({
        key: `${d}->${s.id}`,
        inferred: inferred.has(`${d}->${s.id}`),
        d: `M ${x1} ${y1} C ${mid} ${y1}, ${mid} ${y2}, ${x2} ${y2}`,
      })
    }
  }

  const statusOf = (id: string): GNode['status'] => {
    const r = props.result
    if (!r) return 'idle'
    if (r.skipped && id in r.skipped) return 'skipped'
    if (r.steps && id in r.steps) return 'ok'
    return 'notrun'
  }

  const nodes: GNode[] = steps
    .filter((s) => pos.has(s.id))
    .map((s) => ({ id: s.id, step: s, x: pos.get(s.id)!.x, y: pos.get(s.id)!.y, status: statusOf(s.id) }))

  const columns: Column[] = levels.map((lvl, c) => ({
    index: c,
    x: PAD + c * (W + GAPX) - 10,
    parallel: lvl.length > 1,
    count: lvl.length,
  }))

  return {
    nodes,
    edges,
    columns,
    width: PAD * 2 + levels.length * (W + GAPX) - GAPX,
    height: TOP + PAD + maxRows * (H + GAPY) - GAPY,
    error: '',
  }
})

/** step id 为空是「允许但危险」的状态（内核不拦），图里必须显式说出来，不能留个空盒子。 */
function nodeTitle(n: GNode): string {
  return n.id || '（未命名 step）'
}
function nodeSubtitle(n: GNode): string {
  const call = n.step.call || '（未选 Tool）'
  return n.step.mode === 'service' ? `${call} · service` : call
}
const anyParallel = computed(() => graph.value.columns.some((c) => c.parallel))

/**
 * 图例只列**本次真正出现过**的状态：挂一个没有节点在用的色块（比如「未执行」的红）
 * 会让人去图上找一个不存在的红框。执行失败时后续 step 会变成 notrun，那时它自然出现。
 */
const legendStatuses = computed(() => {
  const used = new Set(graph.value.nodes.map((n) => n.status))
  return (['idle', 'ok', 'skipped', 'notrun'] as const).filter((s) => used.has(s))
})
</script>

<template>
  <div v-if="graph.error" class="graph-wrap">
    <NAlert type="error" title="依赖图无法构建">{{ graph.error }}</NAlert>
  </div>
  <div v-else-if="graph.nodes.length === 0" class="graph-wrap">
    <NEmpty description="还没有 step，加一个就能看到编排图" />
  </div>
  <div v-else>
    <div class="graph-wrap">
      <svg
        :width="graph.width"
        :height="graph.height"
        :viewBox="`0 0 ${graph.width} ${graph.height}`"
        role="img"
        aria-label="Flow 编排图"
      >
        <defs>
          <marker
            id="gear-arrow-solid"
            viewBox="0 0 10 10"
            refX="8"
            refY="5"
            markerWidth="6"
            markerHeight="6"
            orient="auto-start-reverse"
          >
            <path d="M2 1L8 5L2 9" fill="none" stroke="#B4B2A9" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round" />
          </marker>
          <marker
            id="gear-arrow-inferred"
            viewBox="0 0 10 10"
            refX="8"
            refY="5"
            markerWidth="6"
            markerHeight="6"
            orient="auto-start-reverse"
          >
            <path d="M2 1L8 5L2 9" fill="none" stroke="#C4BCF5" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round" />
          </marker>
        </defs>

        <!-- 每层一道淡底轨 + 列标，把「同列 = 同层 = 可并行」变成看得见的东西 -->
        <g v-for="c in graph.columns" :key="`col-${c.index}`">
          <rect
            :x="c.x"
            :y="TOP - 20"
            :width="W + 20"
            :height="graph.height - TOP + 6"
            rx="12"
            fill="#FFFFFF"
            fill-opacity="0.035"
            :stroke="c.parallel ? '#7F77DD' : 'transparent'"
            stroke-opacity="0.5"
            stroke-width="1"
            :stroke-dasharray="c.parallel ? '4 4' : undefined"
          />
          <text :x="c.x + 10" :y="TOP - 8" font-size="11" fill="#A9A5A0">
            第 {{ c.index + 1 }} 层{{ c.parallel ? ` · ${c.count} 个可并行` : '' }}
          </text>
        </g>

        <g v-for="e in graph.edges" :key="e.key">
          <path
            :d="e.d"
            fill="none"
            :stroke="e.inferred ? '#C4BCF5' : '#B4B2A9'"
            stroke-width="1.6"
            :stroke-dasharray="e.inferred ? '5 3' : undefined"
            :marker-end="e.inferred ? 'url(#gear-arrow-inferred)' : 'url(#gear-arrow-solid)'"
          />
        </g>

        <g v-for="n in graph.nodes" :key="`${n.x}-${n.y}-${n.id}`">
          <rect
            :x="n.x"
            :y="n.y"
            :width="W"
            :height="H"
            rx="10"
            :fill="STATUS_STYLE[n.status].fill"
            :stroke="STATUS_STYLE[n.status].stroke"
            stroke-width="1.2"
            :stroke-dasharray="n.step.mode === 'service' ? '6 3' : undefined"
          />
          <text
            :x="n.x + 13"
            :y="n.y + 21"
            font-size="15"
            font-weight="500"
            :fill="n.id ? '#FFFFFF' : '#E8E6E3'"
            :font-style="n.id ? undefined : 'italic'"
          >
            {{ nodeTitle(n) }}
          </text>
          <text :x="n.x + 13" :y="n.y + 39" font-size="12" fill="#EFEDEA">{{ nodeSubtitle(n) }}</text>
          <text v-if="n.step.when" :x="n.x + W - 11" :y="n.y + 21" font-size="11" fill="#FAC775" text-anchor="end">
            when
          </text>
        </g>
      </svg>
    </div>

    <div class="legend">
      <span v-for="s in legendStatuses" :key="s">
        <i class="sw" :style="{ background: STATUS_STYLE[s].fill, borderColor: STATUS_STYLE[s].stroke }" />
        {{ STATUS_STYLE[s].label }}
      </span>
      <span><i class="ln solid" />显式 depends_on</span>
      <span><i class="ln dashed" />推断依赖（由 input / when 的引用算出）</span>
      <span class="hint">
        同列 = 同层 = 可并行{{ anyParallel ? '' : '（当前每层只有一个 step，全串行）' }}
      </span>
    </div>
  </div>
</template>

<style scoped>
.graph-wrap {
  overflow-x: auto;
  padding: 4px 0 10px;
}
.legend {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 8px 18px;
  font-size: 12px;
  color: #d6d3d1;
  margin-top: 8px;
}
.legend > span {
  display: inline-flex;
  align-items: center;
  gap: 7px;
}
.sw {
  width: 14px;
  height: 14px;
  border-radius: 4px;
  border: 1px solid;
  display: inline-block;
}
.ln {
  width: 26px;
  height: 0;
  border-top: 2px solid #b4b2a9;
  display: inline-block;
}
.ln.dashed {
  border-top-style: dashed;
  border-top-color: #c4bcf5;
}
.hint {
  /* 用主题三级色，不用 opacity 压 —— 图例本身就是「给人看颜色」的地方 */
  color: var(--n-text-color-3);
}
</style>
