import { createApp } from 'vue'
import { createPinia } from 'pinia'
import App from './App.vue'
import router from './router'
// 次级文字的单一定义处（见文件头注释：不要在组件里用 opacity 压字）
import './styles/typography.css'

createApp(App).use(createPinia()).use(router).mount('#app')
