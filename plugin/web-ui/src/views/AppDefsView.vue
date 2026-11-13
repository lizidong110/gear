<script setup lang="ts">
// App 清单管理页（REQ-UI-RES-APPDEFS / 内核 REQ-API-APP-DEFS）。
//
// 两套模型必须分清：
//   /apps      = 本进程内存里的**受管长驻进程**（「Apps 长驻」页，等价 gear app run）
//   /app-defs  = registry 里 `kind: App` 的**清单定义**（本页：这个应用由什么组成）
// 同名前缀最容易让人以为「这里能看到正在跑的 App」，所以页面顶部直接写明。
//
// 导入/导出走的是**内存 YAML 文本**，不是服务器路径：内核 artifact.Export/Import 吃本地
// 磁盘路径，REST 若透传就等于开了任意文件读写口。导出的文本可直接粘回导入框往返。
import { computed, h, onMounted, ref } from 'vue'
import {
  NAlert,
  NButton,
  NCard,
  NDataTable,
  NDrawer,
  NDrawerContent,
  NEmpty,
  NInput,
  NSpace,
  NSwitch,
  NTag,
  useDialog,
  type DataTableColumns,
} from 'naive-ui'
import { endpoints } from '@/api/endpoints'
import { ApiError } from '@/api/client'
import type {
  AppDefDTO,
  AppDefImportRespDTO,
  BadBundleRespDTO,
  BundleDiagnosticDTO,
} from '@/api/types'
import { useApiError } from '@/composables/useApiError'

const { report, ok } = useApiError()
const dialog = useDialog()

// ---- 列表 ----
const loading = ref(false)
const defs = ref<AppDefDTO[]>([])
const filter = ref('')

/** 模糊过滤：id / 名称 / 说明 / 入口 Flow / 组成里出现的任一对象 id。 */
const filtered = computed<AppDefDTO[]>(() => {
  const q = filter.value.trim().toLowerCase()
  if (!q) return defs.value
  return defs.value.filter((d) => {
    const c = d.components
    const hay = [
      d.app.id,
      d.app.name ?? '',
      d.app.description ?? '',
      d.app.entry_flow ?? '',
      c?.entry_flow ?? '',
      (c?.tools ?? []).join(' '),
      (c?.flows ?? []).join(' '),
      (c?.hooks ?? []).join(' '),
      (c?.static_files ?? []).join(' '),
    ]
      .join(' ')
      .toLowerCase()
    return hay.includes(q)
  })
})

async function load() {
  loading.value = true
  try {
    const res = await endpoints.listAppDefs()
    defs.value = res.items ?? []
  } catch (e) {
    report(e, '加载 App 清单')
  } finally {
    loading.value = false
  }
}

onMounted(load)

// ---- 详情抽屉 ----
const showDetail = ref(false)
const detail = ref<AppDefDTO | null>(null)

const detailGroups = computed(() => {
  const c = detail.value?.components
  return [
    { label: 'Flows', items: c?.flows ?? [] },
    { label: 'Tools', items: c?.tools ?? [] },
    { label: 'Hooks', items: c?.hooks ?? [] },
    { label: '静态资源', items: c?.static_files ?? [] },
  ]
})

function openDetail(row: AppDefDTO) {
  detail.value = row
  showDetail.value = true
}

// ---- 导出（文本 → 浏览器下载 + 可粘回导入框） ----
const exporting = ref('')
const exported = ref<{ id: string; text: string } | null>(null)

function downloadYAML(name: string, text: string) {
  const blob = new Blob([text], { type: 'application/x-yaml;charset=utf-8' })
  const url = URL.createObjectURL(blob)
  const a = document.createElement('a')
  a.href = url
  a.download = name
  document.body.appendChild(a)
  a.click()
  a.remove()
  // 立刻 revoke 在个别浏览器会取消下载，放到下一个 tick。
  window.setTimeout(() => URL.revokeObjectURL(url), 0)
}

async function exportDef(id: string) {
  exporting.value = id
  try {
    const text = await endpoints.exportAppDef(id)
    exported.value = { id, text }
    downloadYAML(`${id}.bundle.yaml`, text)
    ok(`已导出 ${id}（${text.split('\n').length} 行 YAML）`)
  } catch (e) {
    report(e, `导出 ${id}`)
  } finally {
    exporting.value = ''
  }
}

function fillFromExport() {
  if (!exported.value) return
  bundleText.value = exported.value.text
  resetResult()
  ok('已把导出的 YAML 填进导入框（可直接预检/往返导入）')
}

function downloadAgain() {
  if (!exported.value) return
  downloadYAML(`${exported.value.id}.bundle.yaml`, exported.value.text)
}

// ---- 导入（先 dry_run 预检，再真写） ----
const SAMPLE_BUNDLE = `# 最小示例：把 id 改成你自己的应用名，再点「预检」
kind: App
id: my_app
name: 我的应用
description: 从 Web UI 导入
entry_flow: my_app.up
---
kind: Tool
id: my_app__echo
name: echo
description: 返回空对象的示例 Tool
runtime: lua
entry: |
  return {}
input_schema:
  type: object
output_schema:
  type: object
versions:
  - version: "1.0.0"
    state: stable
---
kind: Flow
id: my_app.up
name: 入口
description: 示例入口 Flow
steps:
  - id: s1
    call: my_app__echo
    input: {}
`

const bundleText = ref('')
const overwrite = ref(false)
const checking = ref(false)
const importing = ref(false)
const precheck = ref<AppDefImportRespDTO | null>(null)
const importResult = ref<AppDefImportRespDTO | null>(null)
const diagnostics = ref<BundleDiagnosticDTO[]>([])
const importHint = ref('')

function resetResult() {
  precheck.value = null
  importResult.value = null
  diagnostics.value = []
  importHint.value = ''
}

/** 400 的 BAD_BUNDLE 响应带结构化诊断（含文档序号与修法），不能只显示一句「请求失败」。 */
function parseDiagnostics(e: unknown): BundleDiagnosticDTO[] {
  if (!(e instanceof ApiError)) return []
  try {
    const body = JSON.parse(e.body) as BadBundleRespDTO
    if (body.code === 'BAD_BUNDLE') return body.diagnostics ?? []
  } catch {
    /* 普通错误体：交给 report 显示内核原文 */
  }
  return []
}

async function runImport(dryRun: boolean) {
  if (!bundleText.value.trim()) {
    report(new Error('请先粘贴 bundle YAML，或上传 .yaml 文件'), '导入 App 清单')
    return
  }
  if (dryRun) checking.value = true
  else importing.value = true
  resetResult()
  try {
    const res = await endpoints.importAppDef({
      bundle: bundleText.value,
      dry_run: dryRun,
      overwrite: overwrite.value,
    })
    if (dryRun) {
      precheck.value = res
      const conflicts = res.conflicts ?? []
      if (conflicts.length > 0 && !overwrite.value) {
        importHint.value =
          '预检发现同名对象。若内容有改动，正式导入会被内核拒绝（409）；确认要覆盖请勾选「覆盖已存在对象」。'
      }
    } else {
      importResult.value = res
      ok(`导入完成：新建 ${(res.created ?? []).length} 项，更新 ${(res.updated ?? []).length} 项`)
      await load()
    }
  } catch (e) {
    diagnostics.value = parseDiagnostics(e)
    if (e instanceof ApiError && e.status === 409) {
      importHint.value = '同名对象已存在且内容不同：勾选「覆盖已存在对象」后重试；不改内容则无需覆盖。'
    }
    report(e, dryRun ? '导入预检' : '导入 App 清单')
  } finally {
    checking.value = false
    importing.value = false
  }
}

function onFile(e: Event) {
  const input = e.target as HTMLInputElement
  const file = input.files?.[0]
  if (!file) return
  void file
    .text()
    .then((text) => {
      bundleText.value = text
      resetResult()
      ok(`已读取 ${file.name}（请点「预检」确认再导入）`)
    })
    .catch((err: unknown) => report(err, `读取 ${file.name}`))
  // 允许再次选择同一个文件。
  input.value = ''
}

function fillSample() {
  bundleText.value = SAMPLE_BUNDLE
  resetResult()
  ok('已填入最小示例；请先把 id 改成你自己的应用名，再点「预检」')
}

// ---- 删除 ----
function confirmDelete(row: AppDefDTO) {
  const id = row.app.id
  dialog.warning({
    title: '删除 App 清单',
    content: `删除「${row.app.name || id}」（${id}）这份清单定义。它引用的 Tool / Flow / Hook 不会被删除；若仍被引用，内核会返回 409。`,
    positiveText: '删除',
    negativeText: '取消',
    onPositiveClick: async () => {
      try {
        await endpoints.deleteAppDef(id)
        ok(`已删除 ${id}`)
        if (detail.value?.app.id === id) showDetail.value = false
        await load()
      } catch (e) {
        report(e, `删除 ${id}`)
      }
    },
  })
}

const columns = computed<DataTableColumns<AppDefDTO>>(() => [
  {
    title: 'app id',
    key: 'id',
    width: 180,
    render: (row) => h('span', { class: 'text-mono' }, row.app.id),
  },
  { title: '名称', key: 'name', width: 150, render: (row) => row.app.name || '—' },
  {
    title: '说明',
    key: 'description',
    ellipsis: { tooltip: true },
    render: (row) => h('span', { class: 'text-3' }, row.app.description || '—'),
  },
  {
    title: '入口 Flow',
    key: 'entry_flow',
    width: 190,
    render: (row) => h('span', { class: 'text-mono' }, row.app.entry_flow || '—'),
  },
  {
    title: '组成',
    key: 'counts',
    width: 210,
    render: (row) =>
      h('div', { class: 'row-actions' }, [
        h(NTag, { size: 'small', type: 'info', bordered: false }, () => `Flow ${row.counts?.flows ?? 0}`),
        h(NTag, { size: 'small', bordered: false }, () => `Tool ${row.counts?.tools ?? 0}`),
        h(NTag, { size: 'small', bordered: false }, () => `Hook ${row.counts?.hooks ?? 0}`),
        ...(row.counts?.static
          ? [h(NTag, { size: 'small', type: 'warning', bordered: false }, () => `静态 ${row.counts.static}`)]
          : []),
      ]),
  },
  {
    title: '操作',
    key: 'actions',
    fixed: 'right' as const,
    width: 230,
    render: (row) =>
      h('div', { class: 'row-actions' }, [
        h(NButton, { size: 'small', quaternary: true, onClick: () => openDetail(row) }, () => '详情'),
        h(
          NButton,
          {
            size: 'small',
            quaternary: true,
            loading: exporting.value === row.app.id,
            onClick: () => void exportDef(row.app.id),
          },
          () => '导出',
        ),
        h(
          NButton,
          { size: 'small', type: 'error', quaternary: true, onClick: () => confirmDelete(row) },
          () => '删除',
        ),
      ]),
  },
])
</script>

<template>
  <div>
    <h2 class="page-title">App 清单 · 定义与导入导出</h2>
    <p class="page-desc">
      这里管理 registry 里的 <code>kind: App</code> <b>清单定义</b>（应用由哪些 Flow / Tool / Hook 组成），
      可以导出成 YAML 文本、再导入到另一台机器。所有内容在内存中生成，<b>不读写服务器上的文件路径</b>。
    </p>

    <NAlert type="warning" :bordered="false" style="margin-bottom: 12px">
      <b>别和「Apps 长驻」搞混</b>：那一页是 <code>/api/apps</code> 的<b>内存态受管进程</b>（等价
      <code>gear app run</code>），这里是 <code>/api/app-defs</code> 的<b>持久化清单定义</b>。
      本页看不到进程是否在运行，也不负责启停。<br />
      另外：文本导出<b>不含静态资源文件内容</b>（需在源机器用 CLI <code>gear export &lt;id&gt;</code> 取完整产物）；
      声明了 <code>migrations</code> 的 bundle 在网页端会被<b>显式拒绝</b>（请改用 CLI <code>gear import</code>
      或 MCP <code>gear_import</code>），避免「导入成功但库结构没演进」的假象。
    </NAlert>

    <NSpace style="margin-bottom: 12px" align="center">
      <NInput
        v-model:value="filter"
        clearable
        placeholder="模糊过滤：app id / 名称 / 说明 / 入口 Flow / 组成对象 id"
        style="width: 420px"
      />
      <NButton :loading="loading" @click="load">刷新</NButton>
      <span class="text-3">共 {{ defs.length }} 份清单<span v-if="filter.trim()">，命中 {{ filtered.length }} 份</span></span>
    </NSpace>

    <NCard size="small" style="margin-bottom: 12px">
      <NDataTable
        v-if="filtered.length > 0"
        :columns="columns"
        :data="filtered"
        :loading="loading"
        :bordered="false"
        :single-line="false"
        size="small"
        :scroll-x="1160"
        :pagination="false"
        :row-key="(row: AppDefDTO) => row.app.id"
      />
      <NEmpty v-else :description="filter.trim() ? '没有匹配的 App 清单' : 'registry 里还没有 App 清单（可从下方导入）'" />
    </NCard>

    <!-- 导入：粘贴 / 上传 → 预检（dry_run）→ 确认写入 -->
    <NCard size="small" title="导入 App 清单" style="margin-bottom: 12px">
      <p class="text-3" style="margin-top: 0">
        粘贴本页导出的多文档 YAML，或上传 <code>.yaml</code> 文件。写入口与 CLI <code>gear import</code> /
        MCP <code>gear_apply</code> 同源；<b>预检只汇报、不写库</b>。
      </p>
      <NInput
        v-model:value="bundleText"
        type="textarea"
        :autosize="{ minRows: 8, maxRows: 20 }"
        placeholder="kind: App\nid: ...\n---\nkind: Tool\n..."
        class="mono-input"
        @update:value="resetResult"
      />
      <NSpace style="margin-top: 10px" align="center" :size="8">
        <label class="file-btn">
          <span>上传 .yaml 文件</span>
          <input type="file" accept=".yaml,.yml,.txt,.gearapp" @change="onFile" />
        </label>
        <NButton @click="fillSample">填入示例</NButton>
        <NButton :loading="checking" :disabled="!bundleText.trim()" @click="runImport(true)">预检（dry_run）</NButton>
        <NButton
          type="primary"
          :loading="importing"
          :disabled="!bundleText.trim()"
          @click="runImport(false)"
        >
          确认导入
        </NButton>
        <NSwitch v-model:value="overwrite" />
        <span class="text-3">覆盖已存在对象（id 相同且内容不同时必需，否则 409）</span>
        <NButton quaternary :disabled="!bundleText" @click="bundleText = ''; resetResult()">清空</NButton>
      </NSpace>

      <NAlert v-if="importHint" type="warning" :bordered="false" style="margin-top: 10px">
        {{ importHint }}
      </NAlert>

      <NAlert v-if="diagnostics.length" type="error" title="bundle 解析失败" :bordered="false" style="margin-top: 10px">
        <ul class="diag-list">
          <li v-for="(d, i) in diagnostics" :key="i">
            <code>{{ d.code }}</code> {{ d.message }}
            <span v-if="d.field"> · 位置 {{ d.field }}</span>
            <span v-if="d.hint" class="text-3"> · {{ d.hint }}</span>
          </li>
        </ul>
      </NAlert>

      <NAlert v-if="precheck" type="info" title="预检结果（未写入任何数据）" :bordered="false" style="margin-top: 10px">
        新建 <b>{{ (precheck.created ?? []).length }}</b> 项 · 更新
        <b>{{ (precheck.updated ?? []).length }}</b> 项 · 可能冲突
        <b>{{ (precheck.conflicts ?? []).length }}</b> 项
        <div v-if="(precheck.conflicts ?? []).length" class="text-3">
          可能冲突：{{ (precheck.conflicts ?? []).join(', ') }}
        </div>
      </NAlert>

      <NAlert v-if="importResult" type="success" title="导入完成" :bordered="false" style="margin-top: 10px">
        新建 {{ (importResult.created ?? []).join(', ') || '（无）' }}<br />
        更新 {{ (importResult.updated ?? []).join(', ') || '（无）' }}
      </NAlert>
    </NCard>

    <!-- 导出结果：既已下载，也留一份文本便于复制 / 直接粘回导入框 -->
    <NCard v-if="exported" size="small" :title="`导出结果 · ${exported.id}`">
      <NSpace style="margin-bottom: 8px" align="center">
        <NButton size="small" @click="downloadAgain">重新下载</NButton>
        <NButton size="small" @click="fillFromExport">填入导入框</NButton>
        <span class="text-3">导出的是多文档 YAML 文本，可直接粘回导入框完成往返（静态资源不含文件内容）。</span>
      </NSpace>
      <pre class="yaml-view">{{ exported.text }}</pre>
    </NCard>

    <!-- 详情 -->
    <NDrawer v-model:show="showDetail" :width="620" placement="right">
      <NDrawerContent title="App 清单详情" closable>
        <template v-if="detail">
          <p class="text-3" style="margin-top: 0">
            <code>{{ detail.app.id }}</code> · {{ detail.app.name || '（未命名）' }}
          </p>
          <p v-if="detail.app.description">{{ detail.app.description }}</p>
          <p v-if="detail.synth_note" class="text-3">{{ detail.synth_note }}</p>

          <div v-for="g in detailGroups" :key="g.label" class="detail-group">
            <b>{{ g.label }}（{{ g.items.length }}）</b>
            <div v-if="g.items.length" class="id-list">
              <code v-for="x in g.items" :key="x">{{ x }}</code>
            </div>
            <span v-else class="text-3">—</span>
          </div>

          <p class="text-3" style="margin: 14px 0 4px">清单原文（JSON 视图）</p>
          <pre class="yaml-view">{{ JSON.stringify(detail.app, null, 2) }}</pre>
          <NSpace style="margin-top: 10px">
            <NButton size="small" :loading="exporting === detail.app.id" @click="exportDef(detail.app.id)">
              导出这份
            </NButton>
          </NSpace>
        </template>
      </NDrawerContent>
    </NDrawer>
  </div>
</template>

<style scoped>
.row-actions {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 6px;
}
.mono-input :deep(textarea) {
  font-family: var(--font-mono, monospace);
  font-size: 12px;
}
.yaml-view {
  max-height: 320px;
  overflow: auto;
  margin: 0;
  padding: 10px;
  border-radius: 4px;
  background: rgba(255, 255, 255, 0.05);
  font-family: var(--font-mono, monospace);
  font-size: 12px;
  line-height: 1.6;
  white-space: pre-wrap;
  word-break: break-all;
}
.diag-list {
  margin: 0;
  padding-left: 18px;
}
.detail-group {
  margin-top: 12px;
}
.id-list {
  display: flex;
  flex-wrap: wrap;
  gap: 6px;
  margin-top: 6px;
}
.id-list code {
  font-family: var(--font-mono, monospace);
  font-size: 12px;
  padding: 2px 6px;
  border-radius: 3px;
  background: rgba(255, 255, 255, 0.06);
}
.file-btn {
  display: inline-flex;
  align-items: center;
  height: 34px;
  padding: 0 14px;
  border: 1px solid var(--n-border-color, #555);
  border-radius: 3px;
  cursor: pointer;
  font-size: 14px;
}
.file-btn:hover {
  border-color: var(--primary-color, #2080f0);
}
.file-btn input {
  display: none;
}
</style>
