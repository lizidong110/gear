package model

import (
	"strings"
	"testing"
)

// 覆盖 model.MigrationSet.Validate / Migration.Target / Migration.Label 的每一条分支。
//
// 为什么必须测：这条校验是「迁移声明的静态约束」唯一执行的地方（MCP 的 gear_validate 与
// artifact.ApplyBundle 都调它）。漏掉任何一个分支，换来的都是运行期一条
// 「table xxx already exists」——而那时 schema 已经写过半个库了。

func TestMigrationSetValidateHappyPath(t *testing.T) {
	s := MigrationSet{
		AppID:  "demo",
		Driver: "sqlite",
		DSN:    "demo.db",
		Migrations: []Migration{
			{Version: 1, Name: "init", SQL: "CREATE TABLE t(a);"},
			{Version: 2, SQL: "ALTER TABLE t ADD COLUMN b;"},
		},
	}
	if err := s.Validate(); err != nil {
		t.Fatalf("合法迁移集被拒: %v", err)
	}

	// 单条覆盖文档级默认值：Target 走「本条显式 > 文档默认」这条回退。
	m := s.Migrations[1]
	if drv, dsn := m.Target(s); drv != "sqlite" || dsn != "demo.db" {
		t.Fatalf("Target 未继承文档默认: %s %s", drv, dsn)
	}
	// 本条自我覆盖时才用本条的值。
	m2 := Migration{Version: 3, Driver: "sqlite", DSN: "other.db", SQL: "X"}
	if drv, dsn := m2.Target(s); drv != "sqlite" || dsn != "other.db" {
		t.Fatalf("Target 未取本条覆盖: %s %s", drv, dsn)
	}

	// Label：有 name 带括号，没有就用版本号（版本号唯一，name 不是，所以 name 可以省）。
	if got := m.Label(); got != "version=2" {
		t.Fatalf("Label 没 name = %q", got)
	}
	if got := s.Migrations[0].Label(); got != "version=1(init)" {
		t.Fatalf("Label 有 name = %q", got)
	}
}

func TestMigrationSetValidateEmpty(t *testing.T) {
	err := MigrationSet{}.Validate()
	if err == nil || !strings.Contains(err.Error(), "migrations 为空") {
		t.Fatalf("空迁移集应报错，got %v", err)
	}
}

func TestMigrationSetValidateRejects(t *testing.T) {
	ok := Migration{Version: 1, DSN: "a.db", SQL: "CREATE TABLE t(a);"}
	cases := []struct {
		name string
		set  MigrationSet
		want string
	}{
		{
			"version 非正",
			MigrationSet{DSN: "a.db", Migrations: []Migration{{Version: 0, DSN: "a.db", SQL: "X"}}},
			"version 必须为正整数",
		},
		{
			"sql 为空",
			MigrationSet{DSN: "a.db", Migrations: []Migration{{Version: 1, DSN: "a.db"}}},
			"sql 不能为空",
		},
		{
			"缺 dsn（不能落内存 scratch 库）",
			MigrationSet{Migrations: []Migration{{Version: 1, SQL: "X"}}},
			"缺少 dsn",
		},
		{
			"同库同版本重复",
			MigrationSet{DSN: "a.db", Migrations: []Migration{ok, ok}},
			"重复",
		},
		{
			// 不同库同版本也要拒：version 号在**文档内**唯一，不是「每库唯一」。
			// 走的是「严格递增」这一支——重复判定按 (driver,dsn,version) 分组，换库就绕过了它，
			// 但排序后的递增检查绕不过：两条 version=1 相邻，第二个必然 `<=` 第一个。
			"两条同版本落在不同库",
			MigrationSet{
				DSN: "a.db",
				Migrations: []Migration{
					{Version: 1, DSN: "a.db", SQL: "X"},
					{Version: 1, DSN: "b.db", SQL: "Y"},
				},
			},
			"必须严格递增",
		},
		{
			// 「重复」判定在逐条循环里，跑在递增检查之前，所以 1,2,2 报的是「重复」而不是
			// 「必须严格递增」——报错文案由**先命中的那条**决定，不是由最像根因的那条决定。
			// 这条锁的是这个先后顺序：将来若有人调换两个循环，这里会先红。
			"同版本出现在靠后位置（报重复而非递增）",
			MigrationSet{DSN: "a.db", Migrations: []Migration{
				{Version: 1, DSN: "a.db", SQL: "X"},
				{Version: 2, DSN: "a.db", SQL: "Y"},
				{Version: 2, DSN: "a.db", SQL: "Z"},
			}},
			"重复",
		},
		{
			// 断号（1 → 3）**合法**：Validate 只要求严格递增，不要求连续。
			// 版本号是给人排顺序的，中间跳一个既无法人也无法机观察到差异，但它会误导
			// 「下一个版本号」的直觉，故在此**锁住**这个口径——将来若有人想改成「禁止断号」，
			// 测试用例会先红。
			"version 断号（1 → 3）允许",
			MigrationSet{DSN: "a.db", Migrations: []Migration{
				{Version: 1, DSN: "a.db", SQL: "X"},
				{Version: 3, DSN: "b.db", SQL: "Y"},
			}},
			"",
		},
	}
	for _, c := range cases {
		err := c.set.Validate()
		if c.want == "" {
			if err != nil {
				t.Fatalf("%s 本该合法却被拒: %v", c.name, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s：应报 %q，实际 %v", c.name, c.want, err)
		}
	}
}

// TestMigrationSetValidateSorted：校验按 version 排序后再判递增，
// 所以「声明顺序打乱、version 本身递增」也必须通过。
func TestMigrationSetValidateSorted(t *testing.T) {
	s := MigrationSet{
		DSN: "a.db",
		Migrations: []Migration{
			{Version: 3, DSN: "a.db", SQL: "C"},
			{Version: 1, DSN: "a.db", SQL: "A"},
			{Version: 2, DSN: "a.db", SQL: "B"},
		},
	}
	if err := s.Validate(); err != nil {
		t.Fatalf("声明顺序打乱但 version 递增，应通过: %v", err)
	}
	// 报错点名的是**排序后**的下一条与它的前一条，而不是声明里的第 i、i+1 个。
	s2 := MigrationSet{
		DSN: "a.db",
		Migrations: []Migration{
			{Version: 1, DSN: "a.db", SQL: "A"},
			{Version: 5, DSN: "a.db", SQL: "B"},
			{Version: 5, DSN: "b.db", SQL: "C"}, // 排序后与 version=5 撞车
		},
	}
	err := s2.Validate()
	if err == nil || !strings.Contains(err.Error(), "必须严格递增") {
		t.Fatalf("排序后撞车应报递增错误，got %v", err)
	}
}
