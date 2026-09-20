<script setup lang="ts">
// 「直接调用」抽屉 —— **整页只挂一个实例**（受控组件）。
// 早先是每个方法自带一个 NDrawer，点第二个方法的「调用」会把两层遮罩叠起来；
// 改成「父视图持有 invokeTarget + 一个受控抽屉」后，状态与遮罩都只有一份。
import { computed, ref, watch } from 'vue'
import {
  NAlert,
  NButton,
  NCheckbox,
  NDivider,
  NDrawer,
  NDrawerContent,
  NTag,
  useMessage,
} from 'naive-ui'
import CodeEditor from '@/components/CodeEditor.vue'
import { useCapabilitiesStore } from '@/stores/capabilities'
import { useApiError } from '@/composables/useApiError'
import type { CapabilityDTO, CapMethodDTO, CapParamDTO } from '@/api/types'

const props = defineProps<{
  show: boolean
  cap: CapabilityDTO | null
  methodName: string
  method: CapMethodDTO | null
}>()

const emit = defineEmits<{ (e: 'update:show', v: boolean): void }>()

const caps = useCapabilitiesStore()
const { report } = useApiError()
const message = useMessage()

const withOptional = ref(false)
const argsText = ref('')
const invoking = ref(false)
const result = ref<unknown>(undefined)
const err = ref('')
const elapsed = ref<number | null>(null)

const callSig = computed(() =>
  props.cap?.kind === 'ext'
    ? `ctx.ext.${props.cap.name}.${props.methodName}({...})`
    : `ctx.${props.cap?.name}.${props.methodName}(...)`,
)

const params = computed<[string, CapParamDTO][]>(() =>
  Object.entries(props.method?.parameters ?? {}).sort(([a], [b]) => a.localeCompare(b)),
)

function defaultForType(t: string): unknown {
  switch (t) {
    case 'string':
      return ''
    case 'number':
    case 'integer':
      return 0
    case 'boolean':
      return false
    case 'object':
      return {}
    case 'array':
      return []
    default:
      return null
  }
}

// 预填模板：以「参数名为键」的 JSON 对象（与后端 buildActionEntry 的 args map 约定一致）。
// 默认只给**必填**参数，避免可选参数把模板撑成一大片空值；有枚举的取第一个值，填完即可跑。
function buildTemplate(): string {
  const obj: Record<string, unknown> = {}
  for (const [name, p] of params.value) {
    if (!withOptional.value && !p.required) continue
    obj[name] = p.enum && p.enum.length ? p.enum[0] : defaultForType(p.type)
  }
  return JSON.stringify(obj, null, 2)
}
const template = computed(() => buildTemplate())
const resetTemplate = () => (argsText.value = template.value)

// 打开、以及「抽屉开着时换了另一个方法」都要重新初始化；
// 只盯 show 变化是不够的——连点两个方法的「调用」时 show 一直为 true，参数会留着上一个方法的。
watch(
  () => (props.show ? `${props.cap?.name ?? ''}.${props.methodName}` : ''),
  (key) => {
    if (!key) return
    withOptional.value = false
    result.value = undefined
    err.value = ''
    elapsed.value = null
    resetTemplate()
  },
)

async function run() {
  if (!props.cap || !props.method) return
  invoking.value = true
  err.value = ''
  result.value = undefined
  elapsed.value = null
  let parsed: Record<string, unknown>
  try {
    parsed = JSON.parse(argsText.value || '{}')
  } catch (e) {
    err.value = `参数 JSON 解析失败: ${(e as Error).message}`
    invoking.value = false
    return
  }
  const t0 = performance.now()
  try {
    result.value = await caps.invoke({
      name: props.cap.name,
      method: props.methodName,
      args: parsed,
    })
    elapsed.value = Math.round(performance.now() - t0)
  } catch (e) {
    err.value = (e as Error).message
    report(e, `调用 ${props.cap.name}.${props.methodName}`)
  } finally {
    invoking.value = false
  }
}

function pretty(v: unknown): string {
  try {
    return JSON.stringify(v, null, 2)
  } catch {
    return String(v)
  }
}
const resultText = computed(() => (result.value === undefined ? '' : pretty(result.value)))

async function copy(text: string, what: string) {
  try {
    await navigator.clipboard.writeText(text)
    message.success(`已复制${what}`)
  } catch {
    message.error('复制失败：浏览器拒绝了剪贴板访问')
  }
}
</script>

<template>
  <NDrawer :show="show" :width="620" placement="right" @update:show="(v: boolean) => emit('update:show', v)">
    <NDrawerContent closable>
      <template #header>
        <span class="text-mono drawer-title">{{ callSig }}</span>
      </template>

      <div class="block">
        <div class="block-label">参数 schema（{{ params.length }}）</div>
        <div v-if="params.length === 0" class="text-3">该方法无参数</div>
        <div v-else class="param-grid">
          <div class="pg-h">参数</div>
          <div class="pg-h">类型</div>
          <div class="pg-h">必填</div>
          <div class="pg-h">说明</div>
          <template v-for="[pname, p] in params" :key="pname">
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

      <NDivider style="margin: 12px 0" />

      <div class="block">
        <div class="block-label">
          <span>参数 JSON</span>
          <NCheckbox v-model:checked="withOptional" size="small" @update:checked="resetTemplate">
            含可选参数
          </NCheckbox>
        </div>
        <CodeEditor v-model="argsText" language="json" height="190px" />
      </div>

      <div class="run-row">
        <NButton type="primary" size="small" :loading="invoking" @click="run">执行</NButton>
        <NButton size="small" quaternary @click="resetTemplate">重置模板</NButton>
        <NTag v-if="elapsed !== null" size="tiny" :bordered="false">{{ elapsed }} ms</NTag>
      </div>

      <NAlert v-if="err" type="error" :bordered="false" style="margin-top: 10px" title="执行失败">
        <span class="text-mono break">{{ err }}</span>
      </NAlert>

      <div v-if="result !== undefined" class="block" style="margin-top: 14px">
        <div class="block-label">
          <span>返回值</span>
          <NButton size="tiny" quaternary @click="copy(resultText, '返回值')">复制</NButton>
        </div>
        <!-- 不用 NCode：naive 的代码块要自己注入 hljs，没注入只会在控制台刷
             `[naive/code]: hljs is not set.` 且不高亮。这里直接用主题化的 pre。 -->
        <pre class="result-pre">{{ resultText }}</pre>
      </div>

      <NAlert type="warning" :bordered="false" style="margin-top: 16px" title="执行权限">
        与 <code>react</code> 一致：全量宿主授权 + Lua 沙箱。<code>db</code>/<code>kv</code>
        为内存态（重启即失）、<code>secret</code> 需内核已注入密钥源、<code>http</code>
        可访问任意 URL。未知能力 / 方法直接报错，不会静默执行。
      </NAlert>
    </NDrawerContent>
  </NDrawer>
</template>

<style scoped>
.drawer-title {
  font-size: 13px;
  color: var(--n-text-color-1);
}
.block-label {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 10px;
  font-size: 12px;
  color: var(--n-text-color-3);
  margin-bottom: 6px;
}
.param-grid {
  display: grid;
  grid-template-columns: 116px 98px 48px minmax(0, 1fr);
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
.run-row {
  display: flex;
  align-items: center;
  gap: 10px;
  margin-top: 10px;
}
.break {
  word-break: break-all;
}
.result-pre {
  margin: 0;
  padding: 10px 12px;
  max-height: 340px;
  overflow: auto;
  /* 与 ExecutionsView 的日志面板同款：主题化的浅底 + 主题正文色，别用写死黑白 */
  background: var(--n-action-color, rgba(0, 0, 0, 0.28));
  border: 1px solid var(--n-border-color);
  border-radius: 6px;
  color: var(--n-text-color-base);
  font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
  font-size: 12px;
  line-height: 1.7;
  white-space: pre-wrap;
  word-break: break-all;
}
</style>
