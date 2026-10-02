import { createRouter, createWebHashHistory } from 'vue-router'

// 用 hash 模式（#/tools）是刻意的：
// 生产环境部署只需「静态文件服务 + /api 反代」两件事，不需要为 SPA 配 fallback rewrite。
// 换 history 模式就得让用户配 `try_files ... /index.html`，对「可选插件」的定位来说太重。
const router = createRouter({
  history: createWebHashHistory(),
  routes: [
    { path: '/', name: 'dashboard', component: () => import('@/views/DashboardView.vue'), meta: { title: '概览' } },
    { path: '/tools', name: 'tools', component: () => import('@/views/ToolsView.vue'), meta: { title: 'Tools' } },
    { path: '/flows', name: 'flows', component: () => import('@/views/FlowsView.vue'), meta: { title: 'Flows' } },
    { path: '/hooks', name: 'hooks', component: () => import('@/views/HooksView.vue'), meta: { title: 'Hooks' } },
    {
      path: '/executions',
      name: 'executions',
      component: () => import('@/views/ExecutionsView.vue'),
      meta: { title: '执行记录' },
    },
    { path: '/apps', name: 'apps', component: () => import('@/views/AppsView.vue'), meta: { title: 'Apps 长驻' } },
    { path: '/secrets', name: 'secrets', component: () => import('@/views/SecretsView.vue'), meta: { title: '密钥' } },
    {
      path: '/capabilities',
      name: 'capabilities',
      component: () => import('@/views/CapabilitiesView.vue'),
      meta: { title: '能力目录' },
    },
    { path: '/:pathMatch(.*)*', redirect: '/' },
  ],
})

export default router
