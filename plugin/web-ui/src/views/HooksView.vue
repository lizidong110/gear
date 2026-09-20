<script setup lang="ts">
import { computed, h, onMounted, ref } from 'vue'
import {
  NAlert,
  NButton,
  NCard,
  NDataTable,
  NDrawer,
  NDrawerContent,
  NForm,
  NFormItem,
  NInput,
  NInputNumber,
  NSelect,
  NSpace,
  NTag,
  useDialog,
  type DataTableColumns,
} from 'naive-ui'
import { CRITICALITIES, CRITICALITY_LABEL, FLOW_LEVEL_STAGES, HOOK_STAGES, STAGE_LABEL } from '@/logic/enums'
import { hookErrors } from '@/logic/tool'
import { useCatalogStore } from '@/stores/catalog'
import { useApiError } from '@/composables/useApiError'
import type { HookDTO } from '@/api/types'

const catalog = useCatalogStore()
const { report, ok } = useApiError()
const dialog = useDialog()

const show = ref(false)
const saving = ref(false)
const errors = ref<string[]>([])

const draft = ref<HookDTO>({ id: '', target: '', stage: 'before_step', action: '', criticality: 'critical', order: 0 })

const stageOptions = HOOK_STAGES.map((s) => ({ label: STAGE_LABEL[s] ?? s, value: s }))
const criticalityOptions = CRITICALITIES.map((c) => ({ label: CRITICALITY_LABEL[c] ?? c, value: c }))

const isFlowLevel = computed(() => (FLOW_LEVEL_STAGES as readonly string[]).includes(draft.value.stage))

/** action 可指向已注册的 Tool（step 级常用）或 Flow（before_flow 常用）。 */
const actionOptions = computed(() => [
  ...catalog.tools.map((t) => ({ label: `Tool · ${t.id}`, value: t.id })),
  ...catalog.flows.map((f) => ({ label: `Flow · ${f.id}`, value: f.id })),
])

function openCreate() {
  draft.value = { id: '', target: '', stage: 'before_step', action: '', criticality: 'critical', order: 0 }
  errors.value = []
  show.value = true
}

function openEdit(hk: HookDTO) {
  draft.value = { ...hk }
  errors.value = []
  show.value = true
}

async function submit() {
  errors.value = hookErrors(draft.value)
  if (errors.value.length > 0) return
  saving.value = true
  try {
    await catalog.saveHook({ ...draft.value })
    ok(`Hook ${draft.value.id} 已注册`)
    show.value = false
  } catch (e) {
    report(e, '注册 Hook')
  } finally {
    saving.value = false
  }
}

/** 删除 Hook。Hook 不被别的定义引用，所以内核没有引用守卫，删掉即生效。 */
function confirmRemove(hk: HookDTO) {
  dialog.warning({
    title: `删除 Hook · ${hk.id}`,
    content: `将从内核 registry 中删除 ${hk.stage} @ ${hk.target} → ${hk.action} 这条切面，不可撤销。` +
      '删除后该阶段不再触发它（Hook 不是 Tool，不存在被引用的顾虑）。',
    positiveText: '确认删除',
    negativeText: '取消',
    onPositiveClick: async () => {
      try {
        await catalog.removeHook(hk.id)
        ok(`Hook ${hk.id} 已删除`)
      } catch (e) {
        report(e, `删除 Hook ${hk.id}`)
      }
    },
  })
}

const columns = computed<DataTableColumns<HookDTO>>(() => [
  { title: 'id', key: 'id', width: 160, render: (row) => h('span', { class: 'text-mono' }, row.id) },
  {
    title: 'stage',
    key: 'stage',
    width: 150,
    render: (row) =>
      h(
        NSpace,
        { size: 4, align: 'center' },
        () => [
          h(NTag, { size: 'small', bordered: false, type: 'info' }, () => row.stage),
          h(
            NTag,
            { size: 'small', bordered: false },
            () => ((FLOW_LEVEL_STAGES as readonly string[]).includes(row.stage) ? 'Flow 级' : 'step 级'),
          ),
        ],
      ),
  },
  {
    title: 'target（匹配对象）',
    key: 'target',
    width: 190,
    render: (row) => h('span', { class: 'text-mono' }, row.target),
  },
  { title: 'action', key: 'action', width: 180, render: (row) => h('span', { class: 'text-mono' }, row.action) },
  {
    title: 'criticality',
    key: 'criticality',
    width: 130,
    render: (row) => {
      const eff = row.criticality || 'critical'
      return h(
        NTag,
        { size: 'small', bordered: false, type: eff === 'critical' ? 'error' : 'warning' },
        () => (eff === 'critical' ? 'critical' : 'best-effort'),
      )
    },
  },
  { title: 'order', key: 'order', width: 70 },
  {
    title: '操作',
    key: 'actions',
    fixed: 'right' as const,
    width: 200,
    render: (row) =>
      h(NSpace, { size: 6 }, () => [
        h(NButton, { size: 'small', onClick: () => openEdit(row) }, () => '编辑 / 覆盖'),
        h(
          NButton,
          { size: 'small', type: 'error', quaternary: true, onClick: () => confirmRemove(row) },
          () => '删除',
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
})
</script>

<template>
  <div>
    <h2 class="page-title">Hooks · 切面</h2>
    <p class="page-desc">
      一条 Hook = stage + target + action + order。留空 criticality 视同 <b>critical</b>（失败即终止主流程）——
      日志/指标类必须显式选 best-effort，否则它会挡住业务流程。Hook 执行<b>不再触发 Hook</b>（结构性无递归）。
    </p>

    <NSpace style="margin-bottom: 12px">
      <NButton type="primary" @click="openCreate">新建 Hook</NButton>
      <NButton @click="catalog.refreshHooks()">刷新</NButton>
      <span class="text-3">删除在行的「操作」列：Hook 不被别的定义引用，删掉即生效。</span>
    </NSpace>

    <NCard size="small">
      <NDataTable
        :columns="columns"
        :data="catalog.hooks"
        :loading="catalog.loading"
        :bordered="false"
        :single-line="false"
        size="small"
        :scroll-x="1080"
        :pagination="{ pageSize: 10 }"
        :row-key="(row: HookDTO) => row.id"
      />
    </NCard>

    <NDrawer v-model:show="show" :width="580" placement="right">
      <NDrawerContent title="Hook 定义" closable>
        <NAlert v-if="errors.length > 0" type="error" title="内核会拒绝这份定义" :bordered="false" style="margin-bottom: 12px">
          <ol class="errs">
            <li v-for="(e, i) in errors" :key="i">{{ e }}</li>
          </ol>
        </NAlert>

        <NForm label-placement="top" size="small">
          <NFormItem label="hook.id" required>
            <NInput v-model:value="draft.id" placeholder="如 auth.check" />
          </NFormItem>

          <NFormItem label="stage（决定 target 的匹配维度）" required>
            <NSelect v-model:value="draft.stage" :options="stageOptions" />
          </NFormItem>
          <NAlert :bordered="false" type="info" style="margin-bottom: 12px">
            {{
              isFlowLevel
                ? 'Flow 级 stage：target 按 **flow id** 匹配（填 "*" 表示全部）'
                : 'step 级 stage：target 按 **tool id** 匹配（填 "*" 表示全部）'
            }}
          </NAlert>

          <NFormItem label="target" required>
            <NInput v-model:value="draft.target" placeholder='flow id / tool id，或 "*" 全部' />
          </NFormItem>

          <NFormItem label="action（已注册的 Tool id 或 Flow id）" required>
            <NSelect v-model:value="draft.action" :options="actionOptions" filterable tag placeholder="选择或直接输入 id" />
          </NFormItem>

          <NSpace :size="12">
            <NFormItem label="criticality" style="width: 260px">
              <NSelect v-model:value="draft.criticality" :options="criticalityOptions" />
            </NFormItem>
            <NFormItem label="order（升序执行）" style="width: 160px">
              <NInputNumber v-model:value="draft.order" :min="0" />
            </NFormItem>
          </NSpace>
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
/* .mono 已收归 src/styles/typography.css 的 .text-mono */
.errs {
  margin: 0;
  padding-left: 18px;
  font-size: 12px;
  line-height: 1.7;
}
</style>
