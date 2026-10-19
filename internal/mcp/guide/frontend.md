# frontend：带 UI 的应用如何写前端（用 petite-vue，别手搓裸 HTML+JS）

做「带界面的应用」时，前端是给终端用户用的，要**轻、可审、可复用**。本报告的主题是：
**别手写一大坨裸 HTML+JS（手动 DOM 操作 + 内联逻辑），改用 petite-vue，并下载到本地离线引入。**

## 1. 为什么别手搓 vanilla JS

手写裸 HTML+JS 在 Gear 的 App 里会很快失控：

- **代码量爆炸**：状态一多，手动 `document.getElementById().innerHTML=` 同步逻辑会铺满页面。
- **审核困难**：业务散在 HTML 属性、`<script>` 块、事件回调里，人工 review 找不到边界。
- **复用差**：每个页面重复写 `fetch`、重复写渲染，改一处要改 N 处。
- **Gear 是「机制」不是业务平台**：前端也应保持最小、可维护，别把 App 写重。

petite-vue 是 Vue 的 ~7KB（gzip）子集，浏览器直接跑、**零构建**，给响应式绑定 + 组件化，
正好压住上面三类问题，又不引入打包链。

## 2. 离线引入：下载到 www/vendor/，别引 CDN

`static_site.serve` 从你声明的 `dir`（如 `apps/myapp/www`）根目录提供静态文件，
所以放在 `www/vendor/` 下的库，页面用 `/vendor/petite-vue.iife.js` 就能加载，**断网也可用**，
不依赖任何 CDN——这能避免「部署环境没外网就白屏」。

**下载请用你自己的文件工具**（MCP 面不提供文件读写，见 `gear_guide("rest-api")` §5）：
静态资源要你用文件工具落盘，`gear_apply` 只做存在性校验，不搬运。

推荐用 **iife** 构建（挂全局 `PetiteVue`，最省心）。两条可达的直链（均验证 200）：

```bash
# 方式 A：unpkg 直链（跟随 301 重定向，curl 带 -L）
curl -sSL https://unpkg.com/petite-vue@0.4.1/dist/petite-vue.iife.js \
  -o apps/myapp/www/vendor/petite-vue.iife.js

# 方式 B：国内 npmmirror（tgz 解压，离线环境更稳）
curl -sSL https://registry.npmmirror.com/petite-vue/-/petite-vue-0.4.1.tgz -o /tmp/pv.tgz
tar xzf /tmp/pv.tgz -C /tmp
cp /tmp/package/dist/petite-vue.iife.js apps/myapp/www/vendor/
```

想要真 `import/export` 拆模块时，再额外下一份 **esm** 构建（文件名 `petite-vue.es.js`，见 §4）。

## 3. 三种构建有什么区别（按需选，别都下）

petite-vue 0.4.1 提供三种构建，体积都在 ~17KB（未压缩，gzip 后约 7KB），差别只在**暴露方式**：

| 构建 | 文件 | 暴露方式 | 用法 | 何时选 |
|---|---|---|---|---|
| iife | `petite-vue.iife.js` | 全局变量 **`PetiteVue`**（即 `window.PetiteVue`，一个命名空间对象，含 `.createApp/.nextTick/.reactive`） | `<script src="/vendor/petite-vue.iife.js"></script>` 之后直接 `PetiteVue.createApp({...}).mount('#app')` | **默认**。零心智、零模块系统，与现有 mindstorm/wms 的普通 `<script>` 写法一致 |
| esm | `petite-vue.es.js` | ES Module 命名导出 `createApp / nextTick / reactive` | `<script type="module">import { createApp } from '/vendor/petite-vue.es.js'</script>` | 想要真 `import/export` 拆模块、配合打包器 tree-shaking 时 |
| umd | `petite-vue.umd.js` | 把 `createApp/nextTick/reactive` **直接挂到全局**（等同全局函数）+ CommonJS `module.exports` 兼容 | 同 iife，但用 `createApp(...)` 而非 `PetiteVue.createApp(...)` | 既要浏览器全局、又要被打包器/Node 引用；**纯静态站场景基本多余** |

要点：

- **iife 与 umd 都能当全局用**，但形状不同：iife 是干净的 `PetiteVue` 命名空间，umd 把函数摊平到全局 + 多一层 CJS 包裹，体积也略大。纯静态站用 iife 就够了。
- **esm 是唯一的真模块化形态**：只有它支持 `import` 你自己的 `js/components/*.js`，也只有它能被打包器 tree-shake。
- 一句话：**默认 iife；要拆模块用 esm；umd 在 Gear 静态站里一般不用。**

## 4. 保持模块化、可复用（文件结构）

把 `www/` 按职责拆开，`index.html` 只留结构与挂载点，逻辑全进 `js/`：

```
www/
  index.html                 # 结构 + 挂载点 #app + 引下面脚本
  vendor/
    petite-vue.iife.js       # 离线引入（§2 下载）
  js/
    api.js                   # 【复用核心】所有 /api 调用集中一处
    app.js                   # PetiteVue.createApp({...}) 根实例
    components/
      list.js                # 每个页面/功能一个 .js（组件或普通函数）
      form.js
      approve-bar.js         # 重复 UI（表格/弹窗/审批条）做成组件复用
```

复用纪律：

- **`api.js` 是唯一 fetch 出口**：所有 Tool/Flow 调用走 `api.run(toolId, input)`，别在每个页面重复 `fetch`。
  端点与 flat body 规则见 `gear_guide("rest-api")` §2（`/api/tools/{id}/run`，body 是入参 map，**不要**包一层 `{"input":{...}}`）。
- **组件复用**：把重复出现的 UI（表格、弹窗、审批条）做成 petite-vue 组件，多处局部注册，别复制粘贴 HTML。
- **`index.html` 保持精简**：结构 + `#app` 挂载点 + 引 `vendor` 与 `js/`，业务逻辑不进 HTML。

## 5. 最小骨架（iife 版）

`www/index.html`：

```html
<!doctype html>
<html>
<head><meta charset="utf-8"><title>MyApp</title></head>
<body>
  <div id="app">
    <h1>{{ title }}</h1>
    <button @click="load">加载</button>
    <ul><li v-for="row in rows" :key="row.id">{{ row.name }}</li></ul>
  </div>
  <script src="/vendor/petite-vue.iife.js"></script>
  <script src="/js/api.js"></script>
  <script src="/js/app.js"></script>
</body>
</html>
```

`www/js/api.js`（复用出口）：

```js
async function runTool(toolId, input) {
  const r = await fetch('/api/tools/' + toolId + '/run', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(input),           // 扁平 body，不要包 {input:{...}}
  });
  if (!r.ok) throw new Error((await r.json()).message || ('HTTP ' + r.status));
  return (await r.json()).output;
}
```

`www/js/app.js`（根实例）：

```js
PetiteVue.createApp({
  data() {
    return { title: '我的应用', rows: [] };
  },
  methods: {
    async load() {
      this.rows = await runTool('myapp__list', {});   // 走 api.js 复用出口
    },
  },
}).mount('#app');
```

## 6. 可移植性提醒

- 若 App 用 bundle 的 `static_files` 做存在性校验，记得把 `www/vendor/**` 一并纳入，
  否则导出后运行时缺库白屏（bundle 只校验不搬运，见 `gear_guide("rest-api")` §5）。
- 路径一律相对 `dir` 根（如 `/vendor/...`、`/js/...`），**不要写绝对路径**（见 `gear_guide("portability")`）。

> 想看完整可跑的多页应用骨架，参考 `apps/mindstorm/www/` 的拆分方式（它是普通 `<script>` + 自建 REST 客户端的现网范例；把它换成 petite-vue 即本主题推荐的形态）。
