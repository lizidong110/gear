<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'
import { NAlert, NButton, NEmpty, NInput, NPopover, NTag, useMessage } from 'naive-ui'
import { useCapabilitiesStore } from '@/stores/capabilities'
import { useApiError } from '@/composables/useApiError'
import CapMethodInvoke from '@/components/CapMethodInvoke.vue'
import type { CapabilityDTO, CapMethodDTO } from '@/api/types'

const caps = useCapabilitiesStore()
const { brief } = useApiError()
const message = useMessage()

// 调用签名：内置原语走位置参数 ctx.<name>.<method>，ext.* 走单个具名 table ctx.ext.<name>.<method>({...})。
// 与 recipes/tool.yaml 里钉死的入口契约一致，避免模型写成位置参数。
function callSig(cap: CapabilityDTO, method: string): string {
  return cap.kind === 'ext' ? `ctx.ext.${cap.name}.${method}` : `ctx.${cap.name}.${method}`
}
const argStyle = (cap: CapabilityDTO) => (cap.kind === 'ext' ? '单具名 table' : '位置参数')
const isExt = (cap: CapabilityDTO) => cap.kind === 'ext'

function methodList(cap: CapabilityDTO): [string, CapMethodDTO][] {
  const m = cap.methods ?? {}
  return Object.keys(m)
    .sort()
    .map((k) => [k, m[k]] as [string, CapMethodDTO])
}
const paramCount = (m: CapMethodDTO) => Object.keys(m.parameters ?? {}).length

// ---- 搜索 + 选中：左侧导航 / 右侧详情。这是一份「目录」，导航比长列表更好用。 ----
const query = ref('')
const selected = ref('')
/** 展开态显式覆盖表；未出现在表里时，默认「搜索中展开、否则折叠」。 */
const expanded = ref<Record<string, boolean>>({})
const keyOf = (cap: string, m: string) => `${cap}.${m}`
const q = computed(() => query.value.trim().toLowerCase())

interface CapEntry {
  cap: CapabilityDTO
  methods: [string, CapMethodDTO][]
}function buildEntries(list: CapabilityDTO[]): CapEntry[] {
  const kw = q.value
  const out: CapEntry[] = []
  for (const cap of list) {
    const all = methodList(cap)
    if (!kw) {
      out.push({ cap, methods: all })
      continue
    }
    // 命中能力名 → 整个能力都在；否则保留命中方法名 / 方法描述的方法。
    const byName = cap.name.toLowerCase().includes(kw)
    const hit = byName
      ? all
      : all.filter(
          ([n, m]) =>
            n.toLowerCase().includes(kw) || (m.description ?? '').toLowerCase().includes(kw),
        )
    if (byName || hit.length) out.push({ cap, methods: hit })
  }
  return out
}

const builtinEntries = computed(() => buildEntries(caps.builtinCaps))
const extEntries = computed(() => buildEntries(caps.extCaps))
const flatEntries = computed(() => [...builtinEntries.value, ...extEntries.value])
const current = computed(
  () => flatEntries.value.find((e) => e.cap.name === selected.value) ?? null,
)
const noMatch = computed(() => q.value !== '' && flatEntries.value.length === 0)

const groups = computed(() => [
  { title: '内置原语', entries: builtinEntries.value },
  { title: '扩展 ext.*', entries: extEntries.value },
])

function isOpen(cap: string, m: string): boolean {
  const v = expanded.value[keyOf(cap, m)]
  return v === undefined ? q.value !== '' : v
}
function toggle(cap: string, m: string): void {
  expanded.value = { ...expanded.value, [keyOf(cap, m)]: !isOpen(cap, m) }
}

// ---- 直接调用：整页只有一个抽屉，父视图持有「当前要调的方法」 ----
// （每个方法各挂一个抽屉会在连点两个「调用」时叠出两层遮罩）
const invokeTarget = ref<{ cap: CapabilityDTO; methodName: string; method: CapMethodDTO } | null>(null)
const invokeShow = ref(false)
function openInvoke(cap: CapabilityDTO, methodName: string, method: CapMethodDTO): void {
  invokeTarget.value = { cap, methodName, method }
  invokeShow.value = true
}

// 目录加载完 / 搜索把当前项过滤掉时，自动落到第一项，右侧不留空白。
watch(flatEntries, (list) => {
  if (!list.length) return
  if (!list.some((e) => e.cap.name === selected.value)) selected.value = list[0].cap.name
})

// 选中一个能力时默认展开它的第一个方法：否则右侧只有一个折叠标题，看着像没内容。
// 用户手动折叠过的（表里有显式 false）不覆盖。
watch(selected, (name) => {
  const first = flatEntries.value.find((e) => e.cap.name === name)?.methods[0]?.[0]
  if (!first) return
  const k = keyOf(name, first)
  if (expanded.value[k] === undefined) expanded.value = { ...expanded.value, [k]: true }
})

const stats = computed(() => [
  { label: '能力', value: caps.caps.length },
  { label: '方法', value: caps.totalMethods },
  { label: '内置', value: caps.builtinCaps.length },
  { label: '扩展', value: caps.extCaps.length },
])

async function copy(text: string, what: string) {
  try {
    await navigator.clipboard.writeText(text)
    message.success(`已复制${what}`)
  } catch {
    message.error('复制失败：浏览器拒绝了剪贴板访问')
  }
}

async function refresh() {
  try {
    await caps.load()
  } catch (e) {
    brief(e, '刷新能力目录')
  }
}

onMounted(async () => {
  try {
    await caps.load()
  } catch (e) {
    brief(e, '加载能力目录')
  }
  caps.startPolling(15000)
})
onUnmounted(() => caps.stopPolling())
</script>

<template>
  <div class="cap-page">
    <header class="cap-head">
      <div class="head-row">
        <h2 class="page-title">能力目录 · Capabilities</h2>
        <div class="head-tools">
          <NTag v-for="s in stats" :key="s.label" size="tiny" :bordered="false">
            {{ s.label }} {{ s.value }}
          </NTag>
          <NPopover trigger="click" placement="bottom-end" :width="470">
            <template #trigger>
              <NButton size="tiny" quaternary>说明</NButton>
            </template>
            <div class="pop">
              <p>
                实时「原子能力目录」：内置 6 原语 + 已注册 <code>ext.*</code>，含方法级参数 schema（类型 / 必填 /
                枚举 / 说明）。与 CLI <code>gear ai capabilities</code>、REST
                <code>GET /api/capabilities</code> 同源，能力可查询、可扩展——新增扩展自动出现在这里。
              </p>
              <p>
                每个方法可点「调用」直接执行（<code>POST /api/capabilities/invoke</code>，无需先写成 Tool）。
                执行权限与 <code>react</code> 一致：全量宿主授权 + Lua 沙箱；<code>db</code>
                为内存态（重启即失）、<code>secret</code> 需内核已注入密钥源、<code>http</code> 可访问任意
                URL。未知能力 / 方法直接报错，不会静默执行。
              </p>
            </div>
          </NPopover>
          <NButton size="tiny" :loading="caps.loading" @click="refresh">刷新</NButton>
        </div>
      </div>
      <p class="page-desc">
        左侧选能力、右侧看方法与参数，点方法上的「调用」可直接执行。参数填「以参数名为键」的 JSON
        对象：内置原语按位置重排，<code>ext.*</code> 原样作为单 table 传入。
      </p>
    </header>

    <NAlert
      v-if="caps.error"
      type="error"
      :bordered="false"
      style="margin-bottom: 10px"
      :title="`拉取能力目录失败：${caps.error}`"
    />

    <div class="cap-body">
      <aside class="cap-nav">
        <NInput
          v-model:value="query"
          size="small"
          clearable
          placeholder="搜索能力 / 方法"
          class="nav-search"
        />
        <div v-for="g in groups" :key="g.title" class="nav-group">
          <div class="nav-group-title">
            <span>{{ g.title }}</span>
            <span class="text-3">{{ g.entries.length }}</span>
          </div>
          <button
            v-for="e in g.entries"
            :key="e.cap.name"
            type="button"
            class="nav-item"
            :class="{ active: e.cap.name === selected, ext: isExt(e.cap) }"
            @click="selected = e.cap.name"
          >
            <span class="nav-dot" />
            <span class="text-mono nav-name">{{ e.cap.name }}</span>
            <span class="text-3 nav-count">{{ e.methods.length }}</span>
          </button>
        </div>
        <div v-if="noMatch" class="text-3 nav-none">无匹配的能力或方法</div>
      </aside>

      <section class="cap-detail">
        <template v-if="current">
          <div class="detail-head">
            <span class="text-mono detail-name">{{ current.cap.name }}</span>
            <NTag size="tiny" :bordered="false" :type="isExt(current.cap) ? 'success' : 'info'">
              {{ current.cap.kind }}
            </NTag>
            <NTag size="tiny" :bordered="false">{{ argStyle(current.cap) }}</NTag>
            <span class="text-3">{{ current.methods.length }} 个方法</span>
            <span class="spacer" />
            <NButton size="tiny" quaternary @click="copy(current.cap.name, '能力名')">
              复制名
            </NButton>
          </div>

          <div v-if="current.methods.length === 0" class="text-3">没有匹配的方法</div>

          <div v-for="[mname, m] in current.methods" :key="mname" class="m-card">
            <div class="m-head" @click="toggle(current.cap.name, mname)">
              <span class="m-caret">{{ isOpen(current.cap.name, mname) ? '▾' : '▸' }}</span>
              <div class="m-title">
                <div class="m-sig-row">
                  <span class="text-mono m-sig">{{ callSig(current.cap, mname) }}</span>
                  <NTag size="tiny" :bordered="false">{{ paramCount(m) }} 参数</NTag>
                </div>
                <div v-if="m.description" class="text-3 m-desc">{{ m.description }}</div>
              </div>
              <div class="m-actions">
                <NButton
                  size="tiny"
                  quaternary
                  @click.stop="copy(callSig(current.cap, mname), '签名')"
                >
                  复制签名
                </NButton>
                <NButton
                  size="tiny"
                  type="primary"
                  secondary
                  @click.stop="openInvoke(current.cap, mname, m)"
                >
                  调用
                </NButton>
              </div>
            </div>

            <div v-if="isOpen(current.cap.name, mname)" class="m-body">
              <div v-if="paramCount(m) === 0" class="text-3">无参数</div>
              <div v-else class="param-grid">
                <div class="pg-h">参数</div>
                <div class="pg-h">类型</div>
                <div class="pg-h">必填</div>
                <div class="pg-h">说明</div>
                <template v-for="(p, pname) in m.parameters ?? {}" :key="pname">
                  <div class="pg-name text-mono">{{ pname }}</div>
                  <div class="pg-type">
                    {{ p.type }}
                    <div v-if="p.enum && p.enum.length" class="text-3 pg-enum">
                      {{ p.enum.join(' | ') }}
                    </div>
                  </div>
                  <div>
                    <NTag v-if="p.required" size="tiny" type="warning" :bordered="false">必填</NTag>
                    <span v-else class="text-3">可选</span>
                  </div>
                  <div class="text-3 pg-desc">{{ p.description || '—' }}</div>
                </template>
              </div>
            </div>
          </div>
        </template>

        <NEmpty
          v-else
          :description="caps.loading ? '加载中…' : '没有可用的原子能力'"
          style="margin-top: 60px"
        />
      </section>
    </div>

    <CapMethodInvoke
      v-model:show="invokeShow"
      :cap="invokeTarget?.cap ?? null"
      :method-name="invokeTarget?.methodName ?? ''"
      :method="invokeTarget?.method ?? null"
    />
  </div>
</template>

<style scoped>
/* 整页做成「定高左右分栏」：左导航固定、右详情独立滚动。
   之前是单列长卡片，ext.ai 一个能力就占两三屏，找方法全靠滚。 */
.cap-page {
  display: flex;
  flex-direction: column;
  height: calc(100vh - 88px);
  min-height: 420px;
}
.cap-head {
  flex: none;
}
.head-row {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  flex-wrap: wrap;
}
.head-tools {
  display: flex;
  align-items: center;
  gap: 6px;
  flex-wrap: wrap;
}
.page-desc {
  margin: 6px 0 10px;
}
.pop p {
  margin: 0 0 8px;
  font-size: 12px;
  line-height: 1.75;
}
.pop p:last-child {
  margin-bottom: 0;
}

.cap-body {
  flex: 1;
  min-height: 0;
  display: flex;
  gap: 14px;
}
.cap-nav {
  width: 236px;
  flex: none;
  overflow: auto;
  padding-right: 12px;
  border-right: 1px solid var(--n-border-color);
}
.nav-search {
  margin-bottom: 10px;
}
.nav-group {
  margin-bottom: 12px;
}
.nav-group-title {
  display: flex;
  justify-content: space-between;
  font-size: 11px;
  letter-spacing: 0.06em;
  color: var(--n-text-color-3);
  padding: 0 8px 6px;
}
.nav-item {
  display: flex;
  align-items: center;
  gap: 8px;
  width: 100%;
  padding: 6px 8px;
  border: none;
  border-radius: 6px;
  background: transparent;
  color: var(--n-text-color-base);
  cursor: pointer;
  text-align: left;
  border-left: 2px solid transparent;
}
.nav-item:hover {
  background: var(--n-action-color, rgba(127, 127, 127, 0.12));
}
.nav-item.active {
  background: var(--n-action-color, rgba(127, 127, 127, 0.16));
  border-left-color: var(--n-text-color-2);
  font-weight: 600;
}
.nav-dot {
  width: 6px;
  height: 6px;
  border-radius: 50%;
  flex: none;
  background: var(--n-text-color-3);
}
.nav-item.ext .nav-dot {
  background: #63e2b7;
}
.nav-name {
  flex: 1;
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  color: inherit;
}
.nav-count {
  flex: none;
}
.nav-none {
  padding: 4px 8px;
}

.cap-detail {
  flex: 1;
  min-width: 0;
  overflow: auto;
}
.detail-head {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
  padding-bottom: 8px;
  margin-bottom: 10px;
  border-bottom: 1px solid var(--n-border-color);
}
.detail-name {
  font-size: 14px;
  font-weight: 600;
  color: var(--n-text-color-1);
}
.spacer {
  flex: 1;
}

.m-card {
  border: 1px solid var(--n-border-color);
  border-radius: 8px;
  margin-bottom: 8px;
  overflow: hidden;
}
.m-head {
  display: flex;
  align-items: flex-start;
  gap: 8px;
  padding: 10px 12px;
  cursor: pointer;
}
.m-head:hover {
  background: var(--n-action-color, rgba(127, 127, 127, 0.08));
}
.m-caret {
  flex: none;
  width: 12px;
  line-height: 20px;
  color: var(--n-text-color-3);
  user-select: none;
}
.m-title {
  flex: 1;
  min-width: 0;
}
.m-sig-row {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
}
.m-sig {
  font-size: 12.5px;
  color: var(--n-text-color-1);
}
.m-desc {
  margin-top: 3px;
  line-height: 1.65;
}
.m-actions {
  flex: none;
  display: flex;
  align-items: center;
  gap: 4px;
}
.m-body {
  padding: 10px 12px 12px 32px;
  border-top: 1px solid var(--n-border-color);
  /* 展开区比卡片略低一档，读作「卡片里的嵌套面板」 */
  background: var(--n-color-embedded, rgba(127, 127, 127, 0.06));
}

/* 参数表：列对齐的网格。之前是「flex + 描述整行悬挂缩进」，描述一长就糊成一片。 */
.param-grid {
  display: grid;
  grid-template-columns: 120px 104px 48px minmax(0, 1fr);
  gap: 6px 12px;
  align-items: start;
  font-size: 12px;
}
.pg-h {
  font-size: 11px;
  letter-spacing: 0.06em;
  color: var(--n-text-color-3);
  border-bottom: 1px solid var(--n-border-color);
  padding-bottom: 5px;
  margin-bottom: 2px;
}
.pg-name {
  color: var(--n-text-color-1);
  word-break: break-all;
}
.pg-type {
  color: var(--n-text-color-2);
}
.pg-enum {
  margin-top: 2px;
  word-break: break-word;
}
.pg-desc {
  line-height: 1.7;
}

@media (max-width: 900px) {
  .cap-page {
    height: auto;
  }
  .cap-body {
    flex-direction: column;
  }
  .cap-nav {
    width: auto;
    border-right: none;
    border-bottom: 1px solid var(--n-border-color);
    padding: 0 0 10px;
    max-height: 240px;
  }
  .cap-detail {
    overflow: visible;
  }
}
</style>
