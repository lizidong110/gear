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
  NSpace,
  useDialog,
  type DataTableColumns,
} from 'naive-ui'
import { useSecretsStore } from '@/stores/secrets'
import { useApiError } from '@/composables/useApiError'

interface SecretRow {
  name: string
}

const secrets = useSecretsStore()
const { report, ok } = useApiError()
const dialog = useDialog()

const show = ref(false)
const saving = ref(false)
const errors = ref<string[]>([])
const draft = ref<{ name: string; value: string }>({ name: '', value: '' })

const rows = computed(() => secrets.names.map((n) => ({ name: n })))

function openCreate() {
  draft.value = { name: '', value: '' }
  errors.value = []
  show.value = true
}

function validate(): boolean {
  errors.value = []
  if (!draft.value.name.trim()) errors.value.push('密钥名不能为空')
  if (draft.value.value === '') errors.value.push('value 不能为空（空值密钥无意义）')
  return errors.value.length === 0
}

async function submit() {
  if (!validate()) return
  saving.value = true
  try {
    await secrets.setSecret(draft.value.name.trim(), draft.value.value)
    ok(`密钥 ${draft.value.name.trim()} 已写入`)
    show.value = false
  } catch (e) {
    report(e, '写入密钥')
  } finally {
    saving.value = false
  }
}

function confirmDelete(name: string) {
  dialog.warning({
    title: '删除密钥',
    content:
      '确认删除 ' + name + '？运行期引用了它的 Tool/Flow 将读不到该密钥（${secrets.NAME} 解析失败）。删除不可恢复。',
    positiveText: '删除',
    negativeText: '取消',
    onPositiveClick: async () => {
      try {
        await secrets.removeSecret(name)
        ok(`已删除 ${name}`)
      } catch (e) {
        report(e, '删除密钥')
      }
    },
  })
}

const columns = computed<DataTableColumns<SecretRow>>(() => [
  {
    title: '密钥名',
    key: 'name',
    render: (row) => h('span', { class: 'text-mono' }, row.name),
  },
  {
    title: '操作',
    key: 'actions',
    fixed: 'right' as const,
    width: 160,
    render: (row) =>
      h(
        NButton,
        { size: 'small', type: 'error', quaternary: true, onClick: () => confirmDelete(row.name) },
        () => '删除',
      ),
  },
])

onMounted(async () => {
  try {
    await secrets.load()
  } catch (e) {
    report(e, '加载密钥')
  }
})
</script>

<template>
  <div>
    <h2 class="page-title">Secrets · 密钥</h2>
    <p class="page-desc">
      管理面经 REST 写入部署凭据（REQ-API-SECRETS）。运行期只读：Tool 内用
      <code>${secrets.NAME}</code> 或 <code>ctx.secret.get</code> 读取。响应不回显明文，列表只显示密钥名。
    </p>

    <NSpace style="margin-bottom: 12px">
      <NButton type="primary" @click="openCreate">写入密钥</NButton>
      <NButton @click="secrets.load()">刷新</NButton>
    </NSpace>

    <NCard size="small">
      <NDataTable
        :columns="columns"
        :data="rows"
        :loading="secrets.loading"
        :bordered="false"
        :single-line="false"
        size="small"
        :scroll-x="400"
        :pagination="{ pageSize: 10 }"
        :row-key="(row: SecretRow) => row.name"
      />
    </NCard>

    <NDrawer v-model:show="show" :width="480" placement="right">
      <NDrawerContent title="写入密钥" closable>
        <NAlert
          v-if="errors.length > 0"
          type="error"
          title="内核会拒绝这份写入"
          :bordered="false"
          style="margin-bottom: 12px"
        >
          <ol class="errs">
            <li v-for="(e, i) in errors" :key="i">{{ e }}</li>
          </ol>
        </NAlert>

        <NForm label-placement="top" size="small">
          <NFormItem label="密钥名（不得含空白或 ${} 等模板语法字符）" required>
            <NInput v-model:value="draft.name" placeholder="如 OPENAI_API_KEY" />
          </NFormItem>
          <NFormItem label="value" required>
            <NInput v-model:value="draft.value" type="password" show-password-toggle placeholder="密钥明文" />
          </NFormItem>
        </NForm>

        <template #footer>
          <NSpace>
            <NButton type="primary" :loading="saving" @click="submit">写入</NButton>
            <NButton @click="show = false">取消</NButton>
          </NSpace>
        </template>
      </NDrawerContent>
    </NDrawer>
  </div>
</template>

<style scoped>
.errs {
  margin: 0;
  padding-left: 18px;
  font-size: 12px;
  line-height: 1.7;
}
</style>
