<script setup lang="ts">
import { computed, h, onMounted, ref } from 'vue'
import { useRouter } from 'vue-router'
import yaml from 'js-yaml'
import {
  NButton,
  NCard,
  NCollapse,
  NCollapseItem,
  NDataTable,
  NDivider,
  NDrawer,
  NDrawerContent,
  NForm,
  NFormItem,
  NInput,
  NSelect,
  NSpace,
  NTag,
  useDialog,
  type DataTableColumns,
} from 'naive-ui'
import { STEP_MODE_LABEL } from '@/logic/enums'
import { topoLevels, type StepLike } from '@/logic/levels'
import { precheckFlow, type PrecheckResult } from '@/logic/template'
import { useCapabilitiesStore } from '@/stores/capabilities'
import { useCatalogStore } from '@/stores/catalog'
import { useExecStore } from '@/stores/exec'
import { useApiError } from '@/composables/useApiError'
import { endpoints } from '@/api/endpoints'
import CodeEditor from '@/components/CodeEditor.vue'
import CheckList from '@/components/CheckList.vue'
import FlowGraph from '@/components/FlowGraph.vue'
import type { FlowDTO } from '@/api/types'

const catalog = useCatalogStore()
const capabilities = useCapabilitiesStore()
const exec = useExecStore()
const { report, ok } = useApiError()
const dialog = useDialog()
const router = useRouter()

/** 模糊过滤：id / 名称 / 描述 / 各 step 的 call 任一包含即命中（大小写不敏感）。 */
const flowFilter = ref('')
const filteredFlows = computed<FlowDTO[]>(() => {
  const q = flowFilter.value.trim().toLowerCase()
  if (!q) return catalog.flows
  return catalog.flows.filter((f) => {
    const calls = (f.steps ?? []).map((s) => s.call).join(' ')
    const hay = [f.id, f.name ?? '', f.description ?? '', calls].join(' ').toLowerCase()
    return hay.includes(q)
  })
})

const show = ref(false)
const saving = ref(false)
const editingId = ref('')

// call 下拉的选项类型：flat 选项 + 分组（naive-ui 的 SelectMixedOption 未从包根导出，故此处
// 就地声明结构化类型；分组项用 type:'group' 与 children 描述，与被组件实际读取的字段一致）。
// 用 type 别名而非 interface：naive-ui 的选项类型带 `[k: string]: unknown` 索引签名，
// 而 TS 只为 type 别名/对象字面量推导隐式索引签名，interface 会因此不可赋值。
type CallOption = {
  label: string
  value: string
  disabled?: boolean
}
type CallOptionGroup = {
  type: 'group'
  label: string
  key: string
  children: CallOption[]
}
type CallOptions = Array<CallOption | CallOptionGroup>

interface DraftStep {
  id: string
  call: string
  use_version: string
  mode: string
  when: string
  depends_on: string[]
  inputText: string
}

const EMPTY_STEP = (): DraftStep => ({
  id: '',
  call: '',
  use_version: '',
  mode: 'once',
  when: '',
  depends_on: [],
  inputText: '{}',
})

interface Draft {
  id: string
  name: string
  description: string
  varsText: string
  steps: DraftStep[]
  editor: 'form' | 'yaml'
  yamlText: string
}

const draft = ref<Draft>({
  id: '',
  name: '',
  description: '',
  varsText: '{}',
  steps: [EMPTY_STEP()],
  editor: 'form',
  yamlText: '',
})

function openCreate() {
  draft.value = {
    id: '',
    name: '',
    description: '',
    varsText: '{}',
    steps: [EMPTY_STEP()],
    editor: 'form',
    yamlText: '',
  }
  editingId.value = ''
  show.value = true
}

function openEdit(f: FlowDTO) {
  draft.value = {
    id: f.id,
    name: f.name ?? '',
    description: f.description ?? '',
    varsText: JSON.stringify(f.vars ?? {}, null, 2),
    steps: (f.steps ?? []).map((s) => ({
      id: s.id,
      call: s.call,
      use_version: s.use_version ?? '',
      mode: s.mode || 'once',
      when: s.when ?? '',
      depends_on: [...(s.depends_on ?? [])],
      inputText: JSON.stringify(s.input ?? {}, null, 2),
    })),
    editor: 'form',
    yamlText: '',
  }
  editingId.value = f.id
  show.value = true
}

// ---- 草稿 → FlowDTO（含 parse 错误收集） ----

function parseObject(text: string, label: string, errs: string[]): Record<string, unknown> {
  try {
    const v = JSON.parse(text || '{}')
    if (v === null || typeof v !== 'object' || Array.isArray(v)) {
      errs.push(`${label} 必须是 JSON 对象`)
      return {}
    }
    return v as Record<string, unknown>
  } catch (e) {
    errs.push(`${label} 不是合法 JSON: ${(e as Error).message}`)
    return {}
  }
}

function draftToFlow(): { flow: FlowDTO; parseErrors: string[] } {
  const errs: string[] = []
  const vars = parseObject(draft.value.varsText, 'vars', errs)
  const steps = draft.value.steps.map((s) => ({
    id: s.id.trim(),
    call: s.call.trim(),
    use_version: s.use_version,
    mode: s.mode,
    when: s.when,
    parallel: false,
    depends_on: s.depends_on,
    input: parseObject(s.inputText, `step ${s.id || '(未命名)'} 的 input`, errs),
  }))
  return {
    flow: {
      id: draft.value.id.trim(),
      name: draft.value.name,
      description: draft.value.description,
      vars,
      steps,
    },
    parseErrors: errs,
  }
}

const draftParsed = computed(() => draftToFlow())

/** 实时预检：与内核 ValidateFlowStatic 同规则同序（见 logic/template.ts）。 */
const precheck = computed<PrecheckResult>(() => {
  const { flow, parseErrors } = draftParsed.value
  if (parseErrors.length > 0) return { errors: parseErrors, warnings: [] }
  return precheckFlow(flow, catalog.precheckCatalog)
})

const graphSteps = computed<StepLike[]>(() => {
  const { flow, parseErrors } = draftParsed.value
  if (parseErrors.length > 0) return []
  return (flow.steps ?? []) as unknown as StepLike[]
})

const levelsInfo = computed(() => {
  try {
    const levels = topoLevels(graphSteps.value)
    const parallel = levels.filter((l) => l.length > 1).length
    return { count: levels.length, parallel, error: '' }
  } catch (e) {
    return { count: 0, parallel: 0, error: (e as Error).message }
  }
})

// ---- 提交 ----

async function submit() {
  const { flow, parseErrors } = draftParsed.value
  if (parseErrors.length > 0) {
    report(new Error(parseErrors.join('；')), '注册 Flow')
    return
  }
  const pre = precheckFlow(flow, catalog.precheckCatalog)
  if (pre.errors.length > 0) {
    report(new Error(pre.errors.join('；')), '注册 Flow')
    return
  }
  saving.value = true
  try {
    await catalog.saveFlow(flow)
    ok(`Flow ${flow.id} 已注册`)
    show.value = false
  } catch (e) {
    report(e, '注册 Flow')
  } finally {
    saving.value = false
  }
}

/** 内核侧静态校验（POST /api/flows/{id}/validate）——需要 Flow 已注册。 */
async function kernelValidate(f: FlowDTO) {
  try {
    const res = await endpoints.validateFlow(f.id)
    if (res.valid) ok(`${f.id} 内核静态校验通过`)
    else dialog.error({ title: `${f.id} 内核校验未通过`, content: res.error ?? '（无错误详情）' })
  } catch (e) {
    report(e, '内核校验')
  }
}

async function run(f: FlowDTO) {
  const hasService = (f.steps ?? []).some((s) => s.mode === 'service')
  if (hasService) {
    dialog.warning({
      title: '这条 Flow 含 service step',
      content: 'service 长驻不可一次性执行（内核会在运行期拒绝）。请到「Apps 长驻」页由 Supervisor 托管。',
      positiveText: '去 Apps 页',
      negativeText: '知道了',
      onPositiveClick: () => void router.push('/apps'),
    })
    return
  }
  try {
    const id = await exec.trigger(f.id)
    ok(`已触发执行 ${id}`)
    void router.push('/executions')
  } catch (e) {
    report(e, '触发执行')
  }
}

/**
 * 注销 Flow。内核的 §8.2 三条守卫在 registry 层，UI 不预判引用关系——
 * 直接发请求，被引用 / 正在跑就由内核回 409 并写明是谁挡住的，原样呈现给用户。
 */
function confirmRemove(f: FlowDTO) {
  dialog.warning({
    title: `注销 Flow · ${f.id}`,
    content:
      '将从内核 registry 中删除这条 Flow，不可撤销。若它被其它 Flow 的 step、Flow 级 Hook 的 action 引用，' +
      '或正被运行中的执行使用，内核会拒绝删除并说明引用者。',
    positiveText: '确认注销',
    negativeText: '取消',
    onPositiveClick: async () => {
      try {
        await catalog.removeFlow(f.id)
        ok(`Flow ${f.id} 已注销`)
      } catch (e) {
        report(e, `注销 Flow ${f.id}`)
      }
    },
  })
}

// ---- YAML 双向 ----

function applyYaml() {
  try {
    const parsed = yaml.load(draft.value.yamlText) as Partial<FlowDTO> | undefined
    if (!parsed || typeof parsed !== 'object') {
      report(new Error('YAML 顶层必须是对象'), '解析 YAML')
      return
    }
    draft.value = {
      ...draft.value,
      id: parsed.id ?? draft.value.id,
      name: parsed.name ?? '',
      description: parsed.description ?? '',
      varsText: JSON.stringify(parsed.vars ?? {}, null, 2),
      steps: (parsed.steps ?? []).map((s) => ({
        id: s.id ?? '',
        call: s.call ?? '',
        use_version: s.use_version ?? '',
        mode: s.mode || 'once',
        when: s.when ?? '',
        depends_on: s.depends_on ?? [],
        inputText: JSON.stringify(s.input ?? {}, null, 2),
      })),
      editor: 'form',
    }
    ok('YAML 已解析并填入表单')
  } catch (e) {
    report(e, '解析 YAML')
  }
}

function toYamlText() {
  const { flow } = draftParsed.value
  return yaml.dump(flow, { noRefs: true, lineWidth: 120 })
}

function exportYaml() {
  draft.value.yamlText = toYamlText()
  draft.value.editor = 'yaml'
}

/**
 * call 的可选项 = 已注册 Tool ∪ 原子能力直调「能力.方法」。
 *
 * 直调省掉「为了用一次原语先注册一层 Tool」的样板（内核 resolveStepCall：**已注册 Tool 优先**，
 * 无同名 Tool 时按「能力.方法」解析）。方法名与参数 schema 与「能力目录」页同源
 * （engine.AIAgent().CapabilitiesMethod），所以新增扩展会自动出现在这里。
 */
const primitiveOptions = computed<CallOption[]>(() =>
  capabilities.caps.flatMap((c) =>
    Object.keys(c.methods ?? {})
      .sort()
      .map((m) => ({ label: `${c.name}.${m}`, value: `${c.name}.${m}` })),
  ),
)

const toolOptions = computed<CallOptions>(() => {
  const tools: CallOption[] = catalog.tools.map((t) => ({
    label: `${t.id}  (${t.runtime})`,
    value: t.id,
  }))
  const groups: CallOptions = []
  if (tools.length) groups.push({ type: 'group', label: '已注册 Tool', key: 'tools', children: tools })
  if (primitiveOptions.value.length) {
    groups.push({
      type: 'group',
      label: '原子能力直调（无需先写 Tool）',
      key: 'primitives',
      children: primitiveOptions.value,
    })
  }
  return groups
})

/** 只有已注册 Tool 才有版本；直调原子能力没有版本概念（内核注册期会拒 use_version）。 */
function isToolCall(call: string): boolean {
  return catalog.toolById.has(call)
}

function versionOptions(call: string) {
  const t = catalog.toolById.get(call)
  return (t?.versions ?? []).map((v) => ({ label: `${v.version} · ${v.state}`, value: v.version }))
}

/** mode 选项：直调原子能力是进程内 Lua，内核注册期即拒 mode=service —— 这里置灰，免得到注册才被打回。 */
function modeOptions(call: string) {
  return [
    { label: STEP_MODE_LABEL.once, value: 'once' },
    { label: STEP_MODE_LABEL.service, value: 'service', disabled: !isToolCall(call) },
  ]
}

const stepIdOptions = computed(() =>
  draft.value.steps.filter((s) => s.id).map((s) => ({ label: s.id, value: s.id })),
)

// ---- 表格 ----

const columns = computed<DataTableColumns<FlowDTO>>(() => [
  { title: 'id', key: 'id', width: 170, render: (row) => h('span', { class: 'text-mono' }, row.id) },
  { title: '名称', key: 'name', width: 150, ellipsis: { tooltip: true } },
  {
    title: 'steps',
    key: 'steps',
    width: 90,
    render: (row) => (row.steps ?? []).length,
  },
  {
    title: '并行层',
    key: 'levels',
    width: 100,
    render: (row) => {
      try {
        const lv = topoLevels((row.steps ?? []) as unknown as StepLike[])
        const par = lv.filter((l) => l.length > 1).length
        return h('span', {}, par > 0 ? `${lv.length} 层（${par} 层可并行）` : `${lv.length} 层（全串行）`)
      } catch (e) {
        return h(NTag, { size: 'small', type: 'error', bordered: false }, () => (e as Error).message)
      }
    },
  },
  {
    title: 'service',
    key: 'svc',
    width: 90,
    render: (row) => {
      const n = (row.steps ?? []).filter((s) => s.mode === 'service').length
      return n > 0 ? h(NTag, { size: 'small', type: 'info', bordered: false }, () => `${n} 个`) : h('span', { class: 'text-3' }, '—')
    },
  },
  {
    title: '操作',
    key: 'actions',
    fixed: 'right' as const,
    width: 312,
    render: (row) =>
      h(NSpace, { size: 6, align: 'center' }, () => [
        h(NButton, { size: 'small', onClick: () => openEdit(row) }, () => '编辑 / 覆盖'),
        h(NButton, { size: 'small', quaternary: true, onClick: () => void kernelValidate(row) }, () => '内核校验'),
        h(NButton, { size: 'small', type: 'primary', quaternary: true, onClick: () => void run(row) }, () => '运行'),
        h(
          NButton,
          { size: 'small', type: 'error', quaternary: true, onClick: () => confirmRemove(row) },
          () => '注销',
        ),
      ]),
  },
])

onMounted(async () => {
  try {
    await catalog.loadAll()
  } catch (e) {
    report(e, '加载目录')
  }
  // 能力目录只用来给 call 提供「直调原子能力」的可选项；拉不到不该挡住本页主功能，故静默降级。
  void capabilities.load().catch(() => {})
})
</script>

<template>
  <div>
    <h2 class="page-title">Flows · 编排</h2>
    <p class="page-desc">
      无依赖的 step <b>默认并行</b>调度；input 里的 <code>${steps.X…}</code> 与 when 里的
      <code>steps.X</code> 都会被算成隐式依赖（图里用虚线画出来）。表单改动会实时跑一遍与内核同规则的静态校验。
    </p>
    <p class="page-desc">
      step 的 <code>call</code> 有两种写法：<b>已注册 Tool 的 id</b>（优先），或<b>直调原子能力</b>
      「<code>能力.方法</code>」（如 <code>db.exec</code> / <code>http.Request</code> /
      <code>ext.ai.complete</code>）——后者免去为用一次原语先写一层 Tool。直调时 input 即该方法参数，
      输出统一为 <code>{ result: … }</code>；方法名与参数见「能力目录」页。仅放行该 step 写出的那一个能力，
      且不支持 use_version / mode=service（注册期即拒）。
    </p>

    <NSpace style="margin-bottom: 12px">
      <NButton type="primary" @click="openCreate">新建 Flow</NButton>
      <NButton @click="catalog.refreshFlows()">刷新</NButton>
      <NInput
        v-model:value="flowFilter"
        clearable
        placeholder="模糊过滤：id / 名称 / 描述 / step.call"
        style="width: 320px"
      />
      <span class="text-3">
        注销在行的「操作」列。内核带引用守卫：被其它 Flow / Flow 级 Hook 引用或正在跑时会被拒绝，并说明是谁挡住的。
      </span>
    </NSpace>

    <NCard size="small">
      <NDataTable
        :columns="columns"
        :data="filteredFlows"
        :loading="catalog.loading"
        :bordered="false"
        :single-line="false"
        size="small"
        :scroll-x="890"
        :pagination="{ pageSize: 10 }"
        :row-key="(row: FlowDTO) => row.id"
      />
    </NCard>

    <NDrawer v-model:show="show" :width="860" placement="right">
      <NDrawerContent :title="editingId ? `编辑 Flow · ${editingId}` : '新建 Flow'" closable>
        <NSpace align="center" style="margin-bottom: 10px">
          <NButton size="small" :type="draft.editor === 'form' ? 'primary' : 'default'" @click="draft.editor = 'form'"
            >表单</NButton
          >
          <NButton size="small" :type="draft.editor === 'yaml' ? 'primary' : 'default'" @click="exportYaml()"
            >YAML（点此导出当前草稿）</NButton
          >
          <span class="text-3">命令行用户习惯写 YAML，所以这里双向支持：贴 YAML → 填表单 → 再注册</span>
        </NSpace>

        <template v-if="draft.editor === 'yaml'">
          <CodeEditor v-model="draft.yamlText" language="yaml" height="420px" style="width: 100%;" />
          <NSpace style="margin-top: 10px">
            <NButton type="primary" @click="applyYaml">解析并填入表单</NButton>
            <NButton @click="draft.yamlText = toYamlText()">用当前草稿覆盖</NButton>
          </NSpace>
        </template>

        <template v-else>
          <NForm label-placement="top" size="small">
            <NSpace :size="12">
              <NFormItem label="flow.id" style="width: 220px" required>
                <NInput v-model:value="draft.id" placeholder="如 news.top10" />
              </NFormItem>
              <NFormItem label="名称" style="width: 220px">
                <NInput v-model:value="draft.name" />
              </NFormItem>
            </NSpace>
            <NFormItem label="描述">
              <NInput v-model:value="draft.description" type="textarea" :autosize="{'maxRows': 3}" />
            </NFormItem>
            <NFormItem label="vars（Flow 内部变量；${vars.x} 引用它）">
              <CodeEditor v-model="draft.varsText" language="json" height="120px" style="width: 100%;"/>
            </NFormItem>
          </NForm>

          <NDivider>steps</NDivider>

          <NCollapse :default-expanded-names="draft.steps.map((_, i) => String(i))">
            <NCollapseItem v-for="(s, i) in draft.steps" :key="i" :name="String(i)">
              <template #header>
                <NSpace align="center" :size="8">
                  <span class="text-mono">{{ s.id || '(未命名 step)' }}</span>
                  <NTag v-if="s.mode === 'service'" size="small" type="info" :bordered="false">service</NTag>
                  <span class="text-3">{{ s.call || '未选调用目标' }}</span>
                </NSpace>
              </template>
              <template #header-extra>
                <NButton
                  size="tiny"
                  quaternary
                  type="error"
                  :disabled="draft.steps.length <= 1"
                  @click="draft.steps.splice(i, 1)"
                  >删除</NButton
                >
              </template>

              <NSpace :size="10">
                <NFormItem label="step.id" style="width: 170px">
                  <NInput v-model:value="s.id" placeholder="s1" />
                </NFormItem>
                <NFormItem label="call（Tool 或「能力.方法」直调）" style="width: 240px">
                  <NSelect
                    v-model:value="s.call"
                    :options="toolOptions"
                    filterable
                    tag
                    placeholder="选 Tool，或直调 db.exec 这类原子能力"
                  />
                </NFormItem>
                <NFormItem label="use_version" style="width: 190px">
                  <NSelect
                    v-model:value="s.use_version"
                    :options="versionOptions(s.call)"
                    :disabled="!isToolCall(s.call)"
                    clearable
                    :placeholder="isToolCall(s.call) ? '默认版本' : '直调能力无版本'"
                  />
                </NFormItem>
                <NFormItem label="mode" style="width: 170px">
                  <NSelect v-model:value="s.mode" :options="modeOptions(s.call)" />
                </NFormItem>
              </NSpace>

              <NSpace :size="10">
                <NFormItem label="depends_on（留空且无引用 ⇒ 与同层 step 并行）" style="width: 440px">
                  <NSelect v-model:value="s.depends_on" multiple :options="stepIdOptions" placeholder="选择上游 step" />
                </NFormItem>
                <NFormItem label="when（表达式，**不要**用 ${}）" style="width: 380px">
                  <NInput v-model:value="s.when" placeholder="steps.s1.output.n > 1" />
                </NFormItem>
              </NSpace>

              <NFormItem label="input（JSON；${vars.x} / ${steps.s1.output.f} 在此展开）">
                <CodeEditor v-model="s.inputText" language="json" height="150px" style="width: 100%;" />
              </NFormItem>
            </NCollapseItem>
          </NCollapse>

          <NButton dashed style="width: 100%; margin-top: 8px" @click="draft.steps.push(EMPTY_STEP())"
            >追加 step</NButton
          >

          <NDivider>预检与编排图</NDivider>

          <NSpace align="center" style="margin-bottom: 8px">
            <NTag size="small" :bordered="false">共 {{ levelsInfo.count }} 层</NTag>
            <NTag v-if="levelsInfo.parallel > 0" size="small" type="info" :bordered="false"
              >{{ levelsInfo.parallel }} 层含并行 step</NTag
            >
            <NTag v-else size="small" :bordered="false">全串行</NTag>
          </NSpace>

          <CheckList :result="precheck" show-when-clean />

          <div style="margin-top: 12px">
            <FlowGraph :steps="graphSteps" />
          </div>
        </template>

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
/* .mono / .dim 已收归 src/styles/typography.css（.text-mono / .text-3） */
</style>
