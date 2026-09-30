package artifact

import (
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"gear/internal/engine"
	"gear/internal/model"
	"gear/internal/registry"
)

// ApplyBundle 按「先改库、再落定义」的顺序执行：Migrations → App → Tool → Flow → Hook。
//
// 迁移排在**最前**不是审美问题，是原子语义的requirement：迁移是纯粹的副作用（改目标库的
// schema），定义是纯粹的元数据（进 registry）。若先落定义、迁移中途失败，「导入成功了但
// 库没有那张表」就成了合法状态——而它 downstream 每一个 Tool 都会撞在「no such table」上。
// 先跑迁移、失败即整包拒绝，就不存在这种中间态。
//
// 顺序由**依赖**决定而不是文档顺序：Hook 的 action 常指向 Tool/Flow，Flow 的 step 常指向 Tool；
// 先落被依赖者，任何一步失败时前面已写的部分是可解释的（并在错误里列出）。
//
// overwrite=false 时，对「已存在且内容不同」的对象**拒绝覆盖**——防止一次性生成把
// 人手工维护的定义悄悄改掉。内容相同则视为幂等更新（updated 里如实列出）。
//
// db 可为 nil，但**仅当 bundle 没有 migrations 声明时**（此时它不被读取）；声明了却传 nil
// 按 fail-closed 拒绝（见 ApplyMigrations）。
//
// ## 为什么从 internal/mcp 上提到本包（2026-09-26）
//
// 此前它只活在 mcp 的 `persistBundle` 里。加了 `gear_import` 之后有两个写入口
// （MCP 的 apply、artifact 的 import），若各写一份，「导入进来的东西与 apply 进去的
// 东西行为不一致」只是时间问题——冲突语义、落库顺序、错误文案任何一处漂移，
// 智能体的自愈回路都会按错的那一套去改。故提升为本包的唯一实现，mcp 的
// persistBundle 改为委托。
func ApplyBundle(reg *registry.Registry, b *Bundle, db Migrator, overwrite bool) (created, updated []string, appliedMigrations []int, err error) {
	created, updated = []string{}, []string{}

	// 路由声明先查：与迁移同理，「能静态判定的错误」不留到写入期。放在最前面还因为
	// 冲突路由在起服时会 panic（net/http 同名 pattern），越早拒越省事。
	if rerr := routeRejectError(b.Tools); rerr != nil {
		return created, updated, nil, rerr
	}

	// 迁移先跑：失败即一个定义都不写（理由见本函数头）。
	appliedMigrations, err = ApplyMigrations(db, b, false)
	if err != nil {
		return nil, nil, nil, err
	}

	// 四类对象的处理完全同构，只有「待写入对象 / 读 / 写 / 名字」四处不同：用一张 step 表
	// 驱动，而不是抄四遍——抄四遍的下场是「改了 App 的冲突文案，忘了改 Hook 的」。
	type step struct {
		kind     string
		id       string
		incoming any
		get      func() (any, error)
		put      func() error
	}
	var steps []step
	for _, a := range b.Apps {
		a := a
		steps = append(steps, step{"App", a.ID, a,
			func() (any, error) { return nilIfMissing(reg.GetApp(a.ID)) },
			func() error { return reg.UpsertApp(a) }})
	}
	for _, t := range b.Tools {
		t := t
		steps = append(steps, step{"Tool", t.ID, t,
			func() (any, error) { return nilIfMissing(reg.GetTool(t.ID)) },
			func() error { return reg.UpsertTool(t) }})
	}
	for _, f := range b.Flows {
		f := f
		steps = append(steps, step{"Flow", f.ID, f,
			func() (any, error) { return nilIfMissing(reg.GetFlow(f.ID)) },
			func() error { return reg.UpsertFlow(f) }})
	}
	for _, h := range b.Hooks {
		h := h
		steps = append(steps, step{"Hook", h.ID, h,
			func() (any, error) { return nilIfMissing(reg.GetHook(h.ID)) },
			func() error { return reg.UpsertHook(h) }})
	}

	for _, s := range steps {
		existing, gerr := s.get()
		if gerr != nil {
			return created, updated, appliedMigrations, gerr
		}
		if existing != nil && !overwrite && !sameJSON(existing, s.incoming) {
			return created, updated, appliedMigrations, engine.Diagf("APPLY_CONFLICT", strings.ToLower(s.kind)+"("+s.id+")",
				"确需覆盖就显式传 overwrite=true；否则换个 id（带应用前缀）",
				"%s %s 已存在且内容不同，已拒绝覆盖（本次未写入；已写入：%v）", s.kind, s.id, created)
		}
		if err := s.put(); err != nil {
			return created, updated, appliedMigrations, fmt.Errorf("写入 %s %s 失败（已写入 %v）: %w", s.kind, s.id, created, err)
		}
		if existing == nil {
			created = append(created, strings.ToLower(s.kind)+":"+s.id)
		} else {
			updated = append(updated, strings.ToLower(s.kind)+":"+s.id)
		}
	}
	return created, updated, appliedMigrations, nil
}

// nilIfMissing 把 registry 的「(具体类型指针, error)」规范成 `any` 的**真 nil**。
//
// ⚠ 这是 typed-nil 陷阱的直接防御：`return reg.GetApp(id)`（返回 `*model.App`）赋给 `any`
// 时，未命中也得到「装着 (*model.App)(nil) 的非 nil 接口」——于是 `existing != nil` **永远成立**，
// 后果是① 所有对象都被判为「已存在」（created 永远为空）；② 冲突判定在空库上也触发。
// 这类缺陷不会编译报错，只会让 apply/import 静默做错事，故在这里显式收口并写清理由。
func nilIfMissing[T any](v *T, err error) (any, error) {
	if err != nil {
		return nil, err
	}
	if v == nil {
		return nil, nil
	}
	return v, nil
}

// DiffExisting 列出 bundle 里「新增」或「已存在」的对象（供 dry_run 用）。
//
// wantExisting=false → 只列新增；true → 只列已存在。两者合起来即 would_apply 的全貌。
func DiffExisting(reg *registry.Registry, b *Bundle, wantExisting bool) []string {
	out := []string{}
	note := func(kind, id string, exists bool) {
		if exists == wantExisting {
			out = append(out, kind+":"+id)
		}
	}
	for _, a := range b.Apps {
		if cur, err := reg.GetApp(a.ID); err == nil {
			note("app", a.ID, cur != nil)
		}
	}
	for _, t := range b.Tools {
		if cur, err := reg.GetTool(t.ID); err == nil {
			note("tool", t.ID, cur != nil)
		}
	}
	for _, f := range b.Flows {
		if cur, err := reg.GetFlow(f.ID); err == nil {
			note("flow", f.ID, cur != nil)
		}
	}
	for _, h := range b.Hooks {
		if cur, err := reg.GetHook(h.ID); err == nil {
			note("hook", h.ID, cur != nil)
		}
	}
	sort.Strings(out)
	return out
}

// sameJSON 比较两个对象的内容是否等价（用 YAML 往返，忽略结构体零值与字段顺序差异）。
//
// 用「序列化后比较」而不是 reflect.DeepEqual：后者对 map 的 nil/空切片差异敏感，
// 会把「人工填的标准定义」与「智能体重发的等价定义」判为不同，从而误报冲突。
func sameJSON(a, b any) bool {
	ja, err := yaml.Marshal(a)
	if err != nil {
		return false
	}
	jb, err := yaml.Marshal(b)
	if err != nil {
		return false
	}
	return string(ja) == string(jb)
}

// 编译期守卫：本包落库依赖 model 的四个类型都还在（改名会让这里红，而不是运行期 nil）。
var (
	_ = model.App{}
	_ = model.Tool{}
	_ = model.Flow{}
	_ = model.Hook{}
)
