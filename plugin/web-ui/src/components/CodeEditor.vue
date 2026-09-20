<script setup lang="ts">
// 代码编辑器（CodeMirror 6）：JSON / YAML / Lua / Bash。
//
// 依赖声明遵循「**直接 import 的包必须显式声明**」——pnpm 是严格 node_modules 布局，
// 传递依赖不会提升到顶层，靠它碰运气迟早会在别的机器上炸（本组件早先引
// `@codemirror/state` 就是这么翻车的）。当前直接依赖：
//   codemirror                    → basicSetup / EditorView
//   @codemirror/lang-json|yaml     → JSON / YAML 语法
//   @codemirror/language           → StreamLanguage（把下面两个旧式 mode 接进 CM6）
//   @codemirror/legacy-modes       → Lua / Shell(bash) 的 mode 实现
//
// 为什么 Lua/Bash 走 legacy-modes 而不是 `@codemirror/lang-*`：
// CM6 官方只为少数语言出了 lang 包，Lua / Shell 一直没有；legacy-modes 是官方的
// 「旧式 mode 移植层」，`StreamLanguage.define(mode)` 就是它给的标准接法。
// Shell mode 覆盖 POSIX sh/bash 的词法（关键字、变量、字符串、注释、内建命令）。
import { onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { basicSetup, EditorView } from 'codemirror'
import { StreamLanguage } from '@codemirror/language'
import { json } from '@codemirror/lang-json'
import { yaml } from '@codemirror/lang-yaml'
import { lua } from '@codemirror/legacy-modes/mode/lua'
import { shell } from '@codemirror/legacy-modes/mode/shell'
import { oneDark } from '@codemirror/theme-one-dark'

/** 支持的语言。`bash` 用的是 legacy-modes 的 shell mode（POSIX sh / bash 通用词法）。
 *  （注意：`<script setup>` 里不能写 `export`，所以这里只声明类型，不对外导出。） */
type EditorLanguage = 'json' | 'yaml' | 'lua' | 'bash' | 'text'

const props = withDefaults(
  defineProps<{
    modelValue: string
    language?: EditorLanguage
    height?: string
  }>(),
  { language: 'json', height: '200px' },
)

const emit = defineEmits<{ (e: 'update:modelValue', v: string): void }>()

const host = ref<HTMLElement | null>(null)
let view: EditorView | null = null

function langExt() {
  switch (props.language) {
    case 'json':
      return json()
    case 'yaml':
      return yaml()
    case 'lua':
      return StreamLanguage.define(lua)
    case 'bash':
      return StreamLanguage.define(shell)
    default:
      return []
  }
}

onMounted(() => {
  if (!host.value) return
  view = new EditorView({
    parent: host.value,
    doc: props.modelValue,
    extensions: [
      basicSetup,
      langExt(),
      oneDark,
      EditorView.updateListener.of((u) => {
        if (u.docChanged) emit('update:modelValue', u.state.doc.toString())
      }),
      EditorView.theme({
        '&': { fontSize: '12px', height: props.height },
        '.cm-scroller': { overflow: 'auto', fontFamily: 'var(--font-mono, monospace)' },
      }),
    ],
  })
})

// 外部值变化时同步进编辑器（避免自身 emit 造成的回环）。
watch(
  () => props.modelValue,
  (v) => {
    if (view && v !== view.state.doc.toString()) {
      view.dispatch({ changes: { from: 0, to: view.state.doc.length, insert: v } })
    }
  },
)

onBeforeUnmount(() => {
  view?.destroy()
  view = null
})
</script>

<template>
  <div ref="host" class="editor-host" />
</template>

<style scoped>
.editor-host {
  /* width: 100% 放在组件里：NFormItem 的内容区是 flex，子 div 不撑开就会缩成很窄一条 */
  width: 100%;
  border: 1px solid var(--n-border-color);
  border-radius: 6px;
  overflow: hidden;
}
</style>
