<script setup lang="ts">
import { computed, h, onMounted, onUnmounted, ref } from 'vue'
import { RouterLink, useRoute } from 'vue-router'
import {
  NAlert,
  NButton,
  NConfigProvider,
  NDialogProvider,
  NInput,
  NLayout,
  NLayoutContent,
  NLayoutHeader,
  NLayoutSider,
  NLoadingBarProvider,
  NMenu,
  NMessageProvider,
  NNotificationProvider,
  NSpace,
  NTag,
  darkTheme,
  type MenuOption,
} from 'naive-ui'
import { useConnStore } from '@/stores/conn'

const conn = useConnStore()
const route = useRoute()

const MENU = [
  { key: '/', label: '概览', en: 'Dashboard' },
  { key: '/capabilities', label: '能力目录', en: 'Capabilities' },
  { key: '/tools', label: 'Tools 原子能力', en: 'Tools' },
  { key: '/flows', label: 'Flows 编排', en: 'Flows' },
  { key: '/hooks', label: 'Hooks 切面', en: 'Hooks' },
  { key: '/executions', label: '执行记录', en: 'Executions' },
  { key: '/apps', label: 'Apps 长驻', en: 'Apps' },
  { key: '/secrets', label: '密钥', en: 'Secrets' },
]

function renderLabel(item: { key: string; label: string; en: string }): ReturnType<typeof h> {
  return h(RouterLink, { to: item.key }, { default: () => item.label })
}

const menuOptions = computed<MenuOption[]>(() =>
  MENU.map((m) => ({ key: m.key, label: () => renderLabel(m) })),
)

const baseInput = ref(conn.base)
const showConn = ref(false)
let timer: ReturnType<typeof setInterval> | null = null

function applyBase() {
  conn.updateBase(baseInput.value)
  showConn.value = false
}

onMounted(() => {
  void conn.ping()
  timer = setInterval(() => void conn.ping(), 10000)
})
onUnmounted(() => {
  if (timer !== null) clearInterval(timer)
})
</script>

<template>
  <NConfigProvider :theme="darkTheme" :theme-overrides="{ common: { fontFamily: 'system-ui, sans-serif' } }">
    <NLoadingBarProvider>
      <NDialogProvider>
        <NNotificationProvider :max="4">
          <NMessageProvider>
            <NLayout has-sider style="height: 100vh">
              <NLayoutSider bordered :width="216" :native-scrollbar="false">
                <div class="brand">
                  <div class="brand-title">Gear</div>
                  <div class="brand-sub">自动化编排 · 管理端</div>
                </div>
                <NMenu :options="menuOptions" :value="route.path" />
                <div class="sider-foot">
                  <div class="foot-note">
                    插件形态：只经 <code>/api</code> 访问内核，内核不感知本 UI。
                  </div>
                </div>
              </NLayoutSider>

              <NLayout>
                <NLayoutHeader bordered class="header">
                  <NSpace align="center" :size="12">
                    <NTag :type="conn.online ? 'success' : 'error'" size="small" :bordered="false">
                      {{ conn.online ? '内核在线' : '内核不可达' }}
                    </NTag>
                    <span class="text-mono">{{ conn.displayBase }}</span>
                    <span v-if="conn.health" class="text-3">
                      Tools {{ conn.health.tools }} · Flows {{ conn.health.flows }}
                    </span>
                  </NSpace>
                  <NSpace align="center" :size="8">
                    <NButton size="small" :loading="conn.checking" @click="conn.ping()">重连</NButton>
                    <NButton size="small" quaternary @click="showConn = !showConn">连接设置</NButton>
                  </NSpace>
                </NLayoutHeader>

                <NLayoutContent :native-scrollbar="false" class="content">
                  <div v-if="showConn" class="conn-box">
                    <NAlert type="info" :bordered="false">
                      留空 = 同源（推荐）：开发期由 Vite 代理 <code>/api</code>，生产由反向代理转发。
                      内核**没有 CORS 头**，因此填跨域 origin 会被浏览器拦下——除非你也给内核套了反代。
                    </NAlert>
                    <NSpace align="center" style="margin-top: 8px">
                      <NInput
                        v-model:value="baseInput"
                        placeholder="例如 http://127.0.0.1:8787（留空为同源）"
                        style="width: 360px"
                      />
                      <NButton type="primary" @click="applyBase">应用并重连</NButton>
                    </NSpace>
                  </div>

                  <div v-if="!conn.online && conn.lastError" class="offline">
                    <NAlert type="error" title="连不上内核" :bordered="false">
                      <div class="text-mono err">{{ conn.lastError }}</div>
                      <div class="text-3">
                        启动内核：<code>gear serve</code>（默认 127.0.0.1:8787，可用
                        <code>--addr</code> 或 <code>GEAR_ADDR</code> 覆盖）
                      </div>
                    </NAlert>
                  </div>

                  <RouterView />
                </NLayoutContent>
              </NLayout>
            </NLayout>
          </NMessageProvider>
        </NNotificationProvider>
      </NDialogProvider>
    </NLoadingBarProvider>
  </NConfigProvider>
</template>

<style>
html,
body,
#app {
  height: 100%;
  margin: 0;
}
.brand {
  padding: 16px 18px 10px;
}
.brand-title {
  font-size: 17px;
  font-weight: 600;
  letter-spacing: 0.5px;
}
.brand-sub {
  font-size: 12px;
  color: var(--n-text-color-3);
  margin-top: 2px;
}
.sider-foot {
  position: absolute;
  bottom: 0;
  padding: 12px 18px 16px;
  font-size: 12px;
  color: var(--n-text-color-3);
  line-height: 1.7;
}
.header {
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: 0 18px;
  height: 52px;
}
.content {
  height: calc(100vh - 52px);
  padding: 18px;
}
.err {
  word-break: break-all;
  margin-bottom: 4px;
}
.conn-box {
  margin-bottom: 16px;
}
.offline {
  margin-bottom: 16px;
}
.page-title {
  font-size: 16px;
  font-weight: 500;
  margin: 0 0 4px;
}
.page-desc {
  font-size: 12px;
  color: var(--n-text-color-3);
  margin-bottom: 14px;
  line-height: 1.75;
}
</style>
