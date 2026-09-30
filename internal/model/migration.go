package model

import (
	"fmt"
	"sort"
	"strings"
)

// Migration 是应用 bundle 里的一条版本化迁移（ENTERPRISE_PRD FR-2b，Q4 拍板：
// 迁移声明走 bundle 新字段 `migrations:`）。
//
// 它回答的是「这个应用的库长什么样」——**不是**内核的库。装饰器：
//
//   - 目标库由 driver + dsn 指定（dsn 允许相对库名，锚到宿主装配的 AppDataDir，
//     与 `ctx.db.*` 的口径完全一致，见 cap.MultiDB.resolveSQLiteDSN）；
//
//   - 内核在应用落库**之前**执行它（见 artifact.ApplyBundle），因此「导入即可用」；
//
//   - 每条迁移在**自己的事务**里执行，失败即整包拒绝。
//
//     deliberately 不提供「回滚迁移」：schema 一旦进过生产库，反向操作比正向操作危险得多，
//
// 而且没有任何业务能自动知道该删哪些行。要换库就换库（新 dsn + 新版本号）。
type Migration struct {
	// Version 必须严格递增且从 1 起；重复或断号在**校验期**即被拒（见 MigrationSet.Validate）。
	Version int `yaml:"version" json:"version"`
	// Name 只是给人看的标签，可以不写（报错时会被版本号顶上）。
	Name string `yaml:"name,omitempty" json:"name,omitempty"`
	// Target 覆盖本条的落库位置。留空则继承所属文档的 driver/dsn。
	Driver string `yaml:"driver,omitempty" json:"driver,omitempty"`
	DSN    string `yaml:"dsn,omitempty" json:"dsn,omitempty"`
	// SQL 是一整段 SQL 文本（可含多条语句，在单事务内执行）。
	SQL string `yaml:"sql" json:"sql"`
}

// Target 返回本条迁移最终使用的 (driver, dsn)：本条显式声明优先，否则回落到文档级默认。
func (m Migration) Target(set MigrationSet) (driver, dsn string) {
	driver, dsn = set.Driver, set.DSN
	if m.Driver != "" {
		driver = m.Driver
	}
	if m.DSN != "" {
		dsn = m.DSN
	}
	return driver, dsn
}

// Label 用于在诊断与日志里点名一条迁移（没写 name 就用版本号——版本号是唯一的，
// name 不是，所以 name 可以省）。
func (m Migration) Label() string {
	if strings.TrimSpace(m.Name) != "" {
		return fmt.Sprintf("version=%d(%s)", m.Version, m.Name)
	}
	return fmt.Sprintf("version=%d", m.Version)
}

// MigrationSet 是 bundle 里 `kind: Migrations` 那一份文档的内容。
//
// 为什么是一整份文档而不是散在 App 清单里：`migrations:` 需要「一组有序条目 + 一个默认
// 目标库」，塞进 App 清单会让「应用定义」与「库的形状」耦合，且 App 清单那份文档在
// 产物里是**必须存在**的，而迁移可以是可选的。故用独立的 kind，让「没迁移」这件事
// 保持零成本。
type MigrationSet struct {
	// AppID 只是标注这条迁移属于哪个应用（不参与判定）。写了有助于人读产物，
	// gear 不校验它与 App 清单是否一致——横竖 gear 也不替应用选库。
	AppID string `yaml:"app_id,omitempty" json:"app_id,omitempty"`
	// Driver / DSN 是本份文档里所有迁移的默认目标；单条可覆盖。
	Driver     string      `yaml:"driver,omitempty" json:"driver,omitempty"`
	DSN        string      `yaml:"dsn,omitempty" json:"dsn,omitempty"`
	Migrations []Migration `yaml:"migrations" json:"migrations"`
}

// Validate 校验迁移声明的**静态**约束（不涉及任何数据库）。
//
// 之所以在校验期就把「version 断号 / 重复」挡掉，是因为这两类错误与运行环境无关，
// 让它们等到执行期才炸，等于把 `gear_validate` 的功劳让给运行期——而运行期的失败
// 已经带着「半个库被改过」的副作用了。
//
// dsn **必须**给出：迁移的目标库不能是 `dsn` 缺省时的内存 scratch 库（进程级、重启即丢），
// 那样「迁移成功」这件事连自己都验证不了。
func (s MigrationSet) Validate() error {
	if len(s.Migrations) == 0 {
		return fmt.Errorf("migrations 为空：要么删掉这份 `kind: Migrations` 文档，要么至少写一条")
	}
	sorted := make([]Migration, len(s.Migrations))
	copy(sorted, s.Migrations)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Version < sorted[j].Version })

	seen := make(map[string]bool, len(sorted))
	for i, m := range sorted {
		label := m.Label()
		if m.Version < 1 {
			return fmt.Errorf("migrations[%d]: version 必须为正整数（第 %d 条是 version=%d）",
				i, i+1, m.Version)
		}
		if strings.TrimSpace(m.SQL) == "" {
			return fmt.Errorf("migrations[%d] %s: sql 不能为空", i, label)
		}
		_, dsn := m.Target(s)
		if strings.TrimSpace(dsn) == "" {
			return fmt.Errorf("migrations[%d] %s: 缺少 dsn（迁移的目标库不能是内存 scratch 库，"+
				"请在 dsn 或文档级写明，如 `dsn: myapp.db`）", i, label)
		}
		drv, dsn := m.Target(s)
		key := drv + "\x00" + dsn + "\x00" + fmt.Sprint(m.Version) // 同一库同一版本才算重复
		if seen[key] {
			return fmt.Errorf("migrations[%d] %s: 与本份文档里另一条(version=%d，同一 dsn)重复",
				i, label, m.Version)
		}
		seen[key] = true
	}
	// 严格递增：跳过这一步，「断号」就会变成执行期的谜题（为什么这条 CREATE TABLE 报「表已存在」）。
	for i := 1; i < len(sorted); i++ {
		if sorted[i].Version <= sorted[i-1].Version {
			return fmt.Errorf("migrations: version 必须严格递增（%d 与 %d 之间：%s 未大于 %s）",
				sorted[i-1].Version, sorted[i].Version, sorted[i].Label(), sorted[i-1].Label())
		}
	}
	return nil
}
