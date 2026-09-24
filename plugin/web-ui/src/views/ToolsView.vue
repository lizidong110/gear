<script setup lang="ts">
import { computed, h, onMounted, ref } from 'vue'
import {
  NAlert,
  NButton,
  NCard,
  NCollapse,
  NCollapseItem,
  NDataTable,
  NDivider,
  NDrawer,
  NDrawerContent,
  NDynamicTags,
  NForm,
  NFormItem,
  NInput,
  NInputNumber,
  NSelect,
  NSpace,
  NSwitch,
  NTag,
  useDialog,
  type DataTableColumns,
} from 'naive-ui'
import {
  IMPLEMENTED_RUNTIMES,
  PROBE_KINDS,
  PROBE_KIND_LABEL,
  RUNTIMES,
  RUNTIME_LABEL,
  STATE_LABEL,
  VERSION_STATES,
  VERSION_TRANSITIONS,
} from '@/logic/enums'
import { toolErrors } from '@/logic/tool'
import { BUILTIN_CAPABILITIES } from '@/logic/generated/enums'
import { useCatalogStore } from '@/stores/catalog'
import { useApiError } from '@/composables/useApiError'
import CodeEditor from '@/components/CodeEditor.vue'
import StatusTag from '@/components/StatusTag.vue'
import type { HealthProbeDTO, ToolDTO, ToolVersionDTO } from '@/api/types'

const catalog = useCatalogStore()
const { report, ok } = useApiError()
const dialog = useDialog()

const show = ref(false)
const saving = ref(false)
const errors = ref<string[]>([])
const editing = ref(false)

interface Draft {
  id: string
  name: string
  description: string
  runtime: string
  entry: string
  capabilities: string[]
  inputText: string
  outputText: string
  versions: ToolVersionDTO[]
  probeEnabled: boolean
  probe: HealthProbeDTO
  newVersion: string
}

const EMPTY_PROBE: HealthProbeDTO = {
  kind: 'http',
  addr: '127.0.0.1:8080',
  path: '/',
  cmd: '',
  interval: '5s',
  timeout: '2s',
  threshold: 3,
}

function emptyDraft(): Draft {
  return {
    id: '',
    name: '',
    description: '',
    runtime: 'process',
    entry: '',
    capabilities: [],
    inputText: '{\n  "type": "object",\n  "properties": {}\n}',
    outputText: '{\n  "type": "object",\n  "properties": {}\n}',
    versions: [{ version: '1.0.0', state: 'draft', digest: '', note: '', created_at: '' }],
    probeEnabled: false,
    probe: { ...EMPTY_PROBE },
    newVersion: '',
  }
}

const draft = ref<Draft>(emptyDraft())

const runtimeOptions = RUNTIMES.map((r) => ({
  label: RUNTIME_LABEL[r] ?? r,
  value: r,
  disabled: !(IMPLEMENTED_RUNTIMES as readonly string[]).includes(r),
}))

const probeKindOptions = PROBE_KINDS.map((k) => ({ label: PROBE_KIND_LABEL[k] ?? k, value: k }))

function openCreate() {
  draft.value = emptyDraft()
  editing.value = false
  errors.value = []
  show.value = true
}

function openEdit(t: ToolDTO) {
  draft.value = {
    id: t.id,
    name: t.name ?? '',
    description: t.description ?? '',
    runtime: t.runtime,
    entry: t.entry ?? '',
    capabilities: [...(t.capabilities ?? [])],
    inputText: JSON.stringify(t.input_schema ?? { type: 'object', properties: {} }, null, 2),
    outputText: JSON.stringify(t.output_schema ?? { type: 'object', properties: {} }, null, 2),
    versions: (t.versions ?? []).map((v) => ({ ...v })),
    probeEnabled: !!t.health_probe,
    probe: t.health_probe ? { ...t.health_probe } : { ...EMPTY_PROBE },
    newVersion: '',
  }
  editing.value = true
  errors.value = []
  show.value = true
}

function parseJson(text: string, field: string): Record<string, unknown> | null {
  try {
    const v = JSON.parse(text)
    if (v === null || typeof v !== 'object' || Array.isArray(v)) {
      errors.value.push(`${field} 必须是 JSON 对象`)
      return null
    }
    return v as Record<string, unknown>
  } catch (e) {
    errors.value.push(`${field} 不是合法 JSON: ${(e as Error).message}`)
    return null
  }
}

function buildTool(): ToolDTO | null {
  errors.value = []
  const input = parseJson(draft.value.inputText, 'input_schema')
  const output = parseJson(draft.value.outputText, 'output_schema')
  if (!input || !output) return null

  const t: ToolDTO = {
    id: draft.value.id.trim(),
    name: draft.value.name,
    description: draft.value.description,
    runtime: draft.value.runtime,
    entry: draft.value.entry,
    capabilities: draft.value.capabilities,
    input_schema: input,
    output_schema: output,
    versions: draft.value.versions.map((v) => ({ ...v, note: v.note ?? '', digest: v.digest ?? '' })),
    health_probe: draft.value.probeEnabled ? draft.value.probe : null,
  }

  const pre = toolErrors(t)
  if (pre.length > 0) {
    errors.value = pre
    return null
  }
  return t
}

async function submit() {
  const t = buildTool()
  if (!t) return
  saving.value = true
  try {
    await catalog.saveTool(t)
    ok(`Tool ${t.id} 已注册`)
    show.value = false
  } catch (e) {
    report(e, '注册 Tool')
  } finally {
    saving.value = false
  }
}

/** 晋升：把「版本 + 目标状态」交给内核，合法目标由 model.VersionTransitions 决定（见 promoteOptionsFor）。 */
async function promote(toolId: string, version: string, target: string) {
  if (!target) return
  try {
    await catalog.promote(toolId, version, target)
    ok(`${toolId}@${version} → ${target}`)
  } catch (e) {
    report(e, '晋升版本')
  }
}

function confirmDelete(t: ToolDTO) {
  dialog.warning({
    title: '注销 Tool',
    content: `确认注销 ${t.id}？内核有三条守卫：被 Flow step 引用、被 Hook action 引用、正被运行中的执行使用——任一命中都会拒绝（409），这是设计而非故障。`,
    positiveText: '注销',
    negativeText: '取消',
    onPositiveClick: async () => {
      try {
        await catalog.removeTool(t.id)
        ok(`已注销 ${t.id}`)
      } catch (e) {
        report(e, '注销 Tool')
      }
    },
  })
}

// 列宽刻意收窄，并给操作列 fixed: 'right' + 表格 scroll-x：
// 原先列宽合计 >1300px，而内容区在 1080 宽的窗口下只有 ~830px ——
// 操作列被推到视口外**鼠标够不着**（Naive UI 不设 scroll-x 时不会出横向滚动条，
// 整列就这么消失在右边界外，肉眼也看不出被裁了）。固定的操作列让它在窄屏下依然可点。
const columns = computed<DataTableColumns<ToolDTO>>(() => [
  {
    title: 'id',
    key: 'id',
    width: 150,
    render: (row) => h('span', { class: 'text-mono' }, row.id),
  },
  { title: '名称', key: 'name', width: 130, ellipsis: { tooltip: true } },
  {
    title: 'runtime',
    key: 'runtime',
    width: 130,
    render: (row) => {
      const impl = (IMPLEMENTED_RUNTIMES as readonly string[]).includes(row.runtime)
      return h(NSpace, { size: 4, align: 'center' }, () => [
        h(NTag, { size: 'small', bordered: false, type: impl ? 'info' : 'error' }, () => row.runtime),
        impl ? null : h(NTag, { size: 'small', bordered: false, type: 'error' }, () => '未实现'),
      ])
    },
  },
  {
    title: '能力',
    key: 'capabilities',
    width: 160,
    render: (row) => {
      const caps = row.capabilities ?? []
      if (caps.length === 0) return h('span', { class: 'text-3' }, '—')
      return h(NSpace, { size: 4 }, () =>
        caps.map((c) => h(NTag, { size: 'small', bordered: false }, () => c)),
      )
    },
  },
  {
    title: '版本',
    key: 'versions',
    width: 210,
    render: (row) =>
      h(NSpace, { size: 6, align: 'center' }, () =>
        (row.versions ?? []).map((v) =>
          h(
            NSpace,
            { size: 2, align: 'center', key: v.version },
            () => [
              h('span', { class: 'text-mono' }, v.version),
              h(StatusTag, { kind: 'state', value: v.state }),
            ],
          ),
        ),
      ),
  },
  {
    title: '探针',
    key: 'probe',
    width: 90,
    render: (row) =>
      row.health_probe ? h(NTag, { size: 'small', type: 'warning', bordered: false }, () => row.health_probe!.kind) : h('span', { class: 'text-3' }, '—'),
  },
  {
    title: '操作',
    key: 'actions',
    fixed: 'right' as const,
    width: 300,
    render: (row) =>
      h(NSpace, { size: 8, align: 'center' }, () => [
        h(NButton, { size: 'small', onClick: () => openEdit(row) }, () => '编辑 / 覆盖'),
        h(NButton, { size: 'small', type: 'error', quaternary: true, onClick: () => confirmDelete(row) }, () => '注销'),
        h(
          NSelect,
          {
            size: 'small',
            placeholder: '晋升到…',
            style: 'width: 150px',
            options: promoteOptionsFor(row),
            value: null,
            onUpdateValue: (v: string) => onPromoteSelect(row, v),
          },
          {},
        ),
      ]),
  },
])

/**
 * 晋升下拉：把「版本 + 目标状态」编码进 option value，选中即执行。
 * 这样每个版本的可迁目标都由 model.VersionTransitions（生成自内核）决定，
 * 前端不会凭空造出内核不认的迁移。
 */
function promoteOptionsFor(row: ToolDTO) {
  const out: Array<{ label: string; value: string }> = []
  for (const v of row.versions ?? []) {
    for (const t of VERSION_TRANSITIONS[v.state] ?? []) {
      out.push({ label: `${v.version} → ${STATE_LABEL[t] ?? t}`, value: `${v.version}::${t}` })
    }
  }
  return out
}

async function onPromoteSelect(row: ToolDTO, val: string) {
  const [version, target] = val.split('::')
  if (version && target) await promote(row.id, version, target)
}

onMounted(async () => {
  try {
    await catalog.loadAll()
  } catch (e) {
    report(e, '加载目录')
  }
})
</script>

<template>
  <div>
    <h2 class="page-title">Tools · 原子能力</h2>
    <p class="page-desc">
      每个 Tool = 一个带 input/output schema 的执行单元。runtime 只允许「已实现」的值——
      选 wasm 会被内核在注册期拒绝（前端也提前置灰）。
    </p>

    <NSpace style="margin-bottom: 12px">
      <NButton type="primary" @click="openCreate">新建 Tool</NButton>
      <NButton @click="catalog.refreshTools()">刷新</NButton>
    </NSpace>

    <NCard size="small">
      <NDataTable
        :columns="columns"
        :data="catalog.tools"
        :loading="catalog.loading"
        :bordered="false"
        :single-line="false"
        size="small"
        :scroll-x="1170"
        :pagination="{ pageSize: 10 }"
        :row-key="(row: ToolDTO) => row.id"
      />
    </NCard>

    <NDrawer v-model:show="show" :width="720" placement="right">
      <NDrawerContent :title="editing ? `编辑 Tool · ${draft.id}` : '新建 Tool'" closable>
        <NAlert v-if="errors.length > 0" type="error" title="内核会拒绝这份定义" :bordered="false" style="margin-bottom: 12px">
          <ol class="errs">
            <li v-for="(e, i) in errors" :key="i">{{ e }}</li>
          </ol>
        </NAlert>

        <NForm label-placement="top" size="small">
          <NSpace :size="12">
            <NFormItem label="tool.id" style="width: 220px" required>
              <NInput v-model:value="draft.id" placeholder="如 news.fetch" />
            </NFormItem>
            <NFormItem label="名称" style="width: 220px">
              <NInput v-model:value="draft.name" placeholder="人类可读名" />
            </NFormItem>
            <NFormItem label="runtime" style="width: 240px" required>
              <NSelect v-model:value="draft.runtime" :options="runtimeOptions" />
            </NFormItem>
          </NSpace>

          <NFormItem label="描述">
            <NInput v-model:value="draft.description" type="textarea" :autosize="{ 'maxRows': 3 }" />
          </NFormItem>

          <NFormItem label="entry">
            <!-- entry 的形态由 runtime 决定，三者**互斥**：
                 写成两条独立的 v-if 再接 v-else 是错的 —— v-else 只跟紧邻的那个 v-if 配对，
                 于是 runtime=lua 时编辑器与下面的输入框会同时出现（同一个值两个输入口）。 -->
            <CodeEditor v-if="draft.runtime === 'lua'" v-model="draft.entry" language="lua" height="220px" />
            <CodeEditor
              v-else-if="draft.runtime === 'process'"
              v-model="draft.entry"
              language="bash"
              height="120px"
            />
            <NInput v-else v-model:value="draft.entry" placeholder="go-host: 已注册的 Go 函数名（如 sample.Tool）" />
          </NFormItem>
          <div v-if="draft.runtime === 'process'" class="hint-line">
            process 用 <code>sh -c</code> 时注意：外层单引号、JSON 内双引号
            （<code>echo '{"v":"1"}'</code>），否则 <code>sh</code> 会吃掉引号导致解析失败。
          </div>
          <div v-else-if="draft.runtime === 'lua'" class="hint-line">
            Lua 是内联源码：通过 <code>ctx.http</code> / <code>ctx.call</code> / <code>ctx.db</code> /
            <code>ctx.secret</code> 等原语访问外部能力，且必须与上面 capabilities 里声明的**一致** ——
            未声明的原语在运行期直接拒绝。
          </div>

          <NFormItem label="capabilities（最小权限：运行期未声明的原语一律拒绝）">
            <NDynamicTags v-model:value="draft.capabilities" :disabled="false" />
          </NFormItem>
          <div class="hint-line">
            内置原语：{{ (BUILTIN_CAPABILITIES as readonly string[]).join(' / ') }}；扩展原语写
            <code>ext.ai</code> / <code>ext.db</code> 这类名字（需内核侧 extensions.yaml 已注册）。
          </div>

          <NDivider />

          <NCollapse>
            <NCollapseItem title="input_schema（JSON Schema，type 必须为 object）" name="in">
              <CodeEditor v-model="draft.inputText" language="json" height="200px" style="width: 100%;" />
            </NCollapseItem>
            <NCollapseItem title="output_schema" name="out">
              <CodeEditor v-model="draft.outputText" language="json" height="200px" style="width: 100%;" />
            </NCollapseItem>
            <NCollapseItem title="health_probe（仅 service 模式步进生效；once 工具忽略）" name="probe">
              <NSpace align="center" style="margin-bottom: 10px">
                <NSwitch v-model:value="draft.probeEnabled" />
                <span class="text-3">启用健康探针</span>
              </NSpace>
              <template v-if="draft.probeEnabled">
                <NSpace :size="12">
                  <NFormItem label="kind" style="width: 220px">
                    <NSelect v-model:value="draft.probe.kind" :options="probeKindOptions" />
                  </NFormItem>
                  <NFormItem label="addr" style="width: 200px">
                    <NInput v-model:value="draft.probe.addr" placeholder="host:port" />
                  </NFormItem>
                  <NFormItem label="path（http）" style="width: 140px">
                    <NInput v-model:value="draft.probe.path" />
                  </NFormItem>
                </NSpace>
                <NFormItem v-if="draft.probe.kind === 'cmd'" label="cmd（sh -c 执行，退出 0 = 健康）">
                  <NInput v-model:value="draft.probe.cmd" />
                </NFormItem>
                <NSpace :size="12">
                  <NFormItem label="interval" style="width: 140px">
                    <NInput v-model:value="draft.probe.interval" placeholder="5s" />
                  </NFormItem>
                  <NFormItem label="timeout" style="width: 140px">
                    <NInput v-model:value="draft.probe.timeout" placeholder="2s" />
                  </NFormItem>
                  <NFormItem label="threshold" style="width: 160px">
                    <NInputNumber v-model:value="draft.probe.threshold" :min="1" />
                  </NFormItem>
                </NSpace>
              </template>
            </NCollapseItem>
          </NCollapse>

          <NDivider />

          <NFormItem label="versions（至少一个；SemVer 为 x.y.z）">
            <NSpace vertical style="width: 100%">
              <NSpace v-for="(v, i) in draft.versions" :key="i" :size="8" align="center">
                <NInput v-model:value="v.version" placeholder="1.0.0" style="width: 130px" />
                <NSelect
                  v-model:value="v.state"
                  :options="VERSION_STATES.map((s) => ({ label: STATE_LABEL[s] ?? s, value: s }))"
                  style="width: 150px"
                />
                <NInput v-model:value="v.note" placeholder="备注" style="width: 260px" />
                <NButton
                  size="small"
                  quaternary
                  type="error"
                  :disabled="draft.versions.length <= 1"
                  @click="draft.versions.splice(i, 1)"
                  >删除</NButton
                >
              </NSpace>
              <NButton
                size="small"
                dashed
                @click="draft.versions.push({ version: '', state: 'draft', digest: '', note: '', created_at: '' })"
                >追加版本</NButton
              >
            </NSpace>
          </NFormItem>
        </NForm>

        <template #footer>
          <NSpace>
            <NButton type="primary" :loading="saving" @click="submit">注册 / 覆盖</NButton>
            <NButton @click="show = false">取消</NButton>
          </NSpace>
        </template>
      </NDrawerContent>
    </NDrawer>
  </div>
</template>

<style scoped>
/* .mono / .dim / .hint 已收归 src/styles/typography.css
   （.text-mono / .text-3 / .hint-line）—— 11px + opacity 0.6 的字段说明在暗色主题下几乎读不出来。 */
.errs {
  margin: 0;
  padding-left: 18px;
  font-size: 12px;
  line-height: 1.7;
}
</style>
