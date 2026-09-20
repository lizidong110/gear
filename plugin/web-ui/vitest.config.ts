// 单测配置**单独一个文件**，刻意不与 vite.config.ts 合并。
//
// 原因：这条工具链里同时存在两个 vite 大版本 —— 项目构建用 `vite@6`，
// 而 `vitest@2` 内置的是 `vite@5`（见 node_modules/.pnpm/vite@5.0.0_...）。
// 运行时两者各用各的副本，互不干扰；但若在**同一个**配置里用 vitest 的 defineConfig
// 接收 vite 6 的 `vue()` 插件，类型上就会打架：
//   TS2769: Plugin<Api> is not assignable to PluginOption（vite 5 与 vite 6 的 UserConfig 不兼容）
//
// 拆开之后各用各的类型。单测用例是纯逻辑 TS，不 import 任何 .vue 文件，
// 所以这里也不需要 vue 插件与 @vitejs/plugin-vue。
//
// 将来若要写组件测试：把 vitest 升到 peer 支持 vite 6 的版本（^5||^6），
// 再把两个配置合并回一个，那时候才能安全地在 test 配置里挂 vue 插件。
import { fileURLToPath, URL } from 'node:url'
import { defineConfig } from 'vitest/config'

export default defineConfig({
  resolve: {
    alias: { '@': fileURLToPath(new URL('./src', import.meta.url)) },
  },
  test: {
    environment: 'happy-dom',
    include: ['src/**/*.spec.ts'],
  },
})
