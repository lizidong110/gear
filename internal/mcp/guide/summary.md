# Gear 心智模型（常驻摘要）

Gear 是「机制内核」：只给 能力原语 + Tool（执行单元）+ Flow（编排）+ Hook（切面），不含业务策略。
「应用」运行时 = 一个含 `mode: service` 长驻 step 的 Flow，用 `gear_app(action=start)` 起为后台进程；
注册时还有一份 `kind: App` 清单落 registry `apps` 表（命名空间户主，`--scope` 守卫与 `app start` 端口回退都读它）。
**「跨会话存活」不是无条件的**（Windows）：未能脱离调用方 Job Object 时，`start` 会返回 `escaped_job:false`
并附 warning——该进程会随**发起启动的那次会话**结束被回收。它是 running，但活不到会话之后。

## 四层模型
- **能力原语（宿主提供）**：受门禁 7 个 `http/call/db/secret/event/subscribe/config`（须声明才可用）；
  免门禁纯函数 5 个 `time/template/crypto/id/validate`（始终可用，**声明了反而被拒**）；`ext.*`（须注册且须声明）。
  `json` **不是能力**（只是 Lua 的 `ctx.json` 绑定，Flow 不能直调）。
- **Tool** = 能力 + 业务逻辑，可复用。**可复用的才建 Tool；一次性的直接在 `step.call` 写「能力.方法」**。
- **Flow** = 编排执行单元（谁先谁后 / 何时跑 / 是否长驻）。
- **Hook** = 生命周期插入与门禁（`before_flow/before_step/after_step/on_error/on_success/before_promote`）。

锚点：一处定义多入口执行（CLI/REST/MCP 同源）；能静态校验的绝不留到运行期（fail-closed）。

## 标准流程：创建并运行应用
① `gear_doctor` → ② `gear_guide(topic)` + `gear_capabilities` → ③ **用你自己的文件工具**落盘静态资源
→ ④ `gear_validate`（不落库，可无限重试）→ ⑤ `gear_apply`（原子落库）→ ⑥ `gear_app(start, flow_id)`
→ ⑦ `gear_app(status)` + 端口探活 + 日志行 → ⑧ 汇报并**告知如何停止**。

做带 UI 的应用：前端**别手写裸 HTML+JS**，先 `gear_guide("frontend")` 用 petite-vue（下载到 `www/vendor/` 离线引入、保持模块化）。

## 八条铁律
1. 写「能力.方法」前先 `gear_capabilities`，不许猜（`crypto/time/template` 免门禁、`json` 不是能力、`ext.*` 必须声明）。
2. 先 `gear_validate` 再 `gear_apply`，不要「先写进去再改」。
3. 长驻应用只能用 `gear_app(action=start)`；`gear_run` 对 service Flow 必拒（设计如此，不是 bug）。
4. 凭据只走 `${secrets.NAME}`，绝不内联明文；扩展原子禁止读环境变量。
5. 所有 id 必须以 `<app-id>__` 或 `<app-id>.` 开头。
6. 失败先读 `gear_execution` / `gear_app` 的日志定位到 step 再改；**禁止未验证就宣称成功**。
7. **缺能力不许凑**（不 shell 外调、不造 `ext.*` 名、不读 env），按 `gear_guide("primitive-gap")` 上报并标注阻塞。
8. 动手前先 `gear_guide("security")`：**破坏性清单**（`gear_delete` / `gear_app(stop, prune)` / `gear_promote(deprecated)` / `gear_apply(overwrite)`）与**密钥规则**（MCP 不写密钥、由人在 CLI 播种）都在那里。

**读到错误先看 `[CODE]`**（按码去 `gear_guide("errors")` 查修法）。**唯一例外**：缺必填参数的报错由协议层的入参校验直接挡下，**不带 `[CODE]`** ——读里面的 `missing properties` 字段名即可定位，别当成 Gear 的 bug。

## 工具
`gear_guide`(手册) · `gear_catalog`(摘要) · `gear_describe`(全量) · `gear_capabilities`(能力与参数)
· `gear_validate`(校验 YAML) · `gear_apply`(落库) · `gear_delete` · `gear_promote` · `gear_invoke`(能力直调)
· `gear_run`(一次性编排) · `gear_execution`(执行记录/日志) · `gear_app`(起停查应用) · `gear_doctor`(环境自检)
· `gear_export`(导出产物，可备份/换机器/交付) · `gear_import`(导回产物)

**造完一个应用，就用 `gear_export` 把它导出一份**：只有 Tool/Flow 在 registry 里不算「你的产品」——
静态资源不在库里、清单也带不走，换台机器就只剩一堆散落状态。产物是目录形态（可 git 版本化）或 `.gearapp` 包。

## 完成的证据（缺一不可）
`gear_validate` 的 `valid:true` + `gear_apply` 的 `applied` 清单 +
（长驻：`gear_app(status)` 的 `state:running` 且端口 `listening:true`｜一次性：`gear_execution` 的 `status:succeeded`）
+ 告知停止方式（应用是独立后台进程，需显式 stop）。
  **别只看 `state:running` 就报成功**：`escaped_job:false` 时须如实告诉用户「它只活到本次会话结束」，
  别用「已在后台常驻」这种措辞——智能体最贵的错误是把「现在能访问」说成了「一直能访问」。
