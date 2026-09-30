package engine

// lua_db_batch_test.go：锁 `ctx.db.batch` 在**调用方（Lua）眼里**是什么（AC-B1~B3）。
//
// 与 cap 层那组测试的分工：cap 层验「回滚真的发生了」，这里验「应用拿得到该拿到的东西」——
// committed 是布尔、results 能按序号读出失败那条的 error、回滚后下一次 query 是空的。

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"gear/internal/cap"
)

func luaBatchHost(t *testing.T) *cap.Context {
	t.Helper()
	db := cap.NewMultiDB(filepath.Join(t.TempDir(), "batch.db"), t.TempDir())
	t.Cleanup(func() { _ = db.Close() })
	return luaHost(t, []string{"db"}, &cap.Providers{DB: db})
}

// AC-B1：批次中途失败 → 返回结构里 committed=false 且指出第几条；随后查不到任何部分写入。
func TestLuaDBBatchFailureIsVisibleAndRolledBack(t *testing.T) {
	host := luaBatchHost(t)
	out, err := luaRunHost(t, `
		ctx.db.exec({ driver = "sqlite", sql = "CREATE TABLE ledger(id INTEGER, amt INTEGER)" })
		local r = ctx.db.batch({ driver = "sqlite", statements = {
			{ sql = "INSERT INTO ledger VALUES(1, 100)" },
			{ sql = "INSERT INTO ledger VALUES(2, )" },
		} })
		local after = ctx.db.query({ driver = "sqlite", sql = "SELECT id FROM ledger" })
		return {
			committed = r.committed,
			nresults  = #r.results,
			err_at    = (r.results[2].error or ""),
			left      = #after,
		}
	`, nil, host)
	if err != nil {
		t.Fatalf("语句失败不该把调用本身抛断（应能读到 committed=false）: %v", err)
	}
	if out["committed"] != false {
		t.Fatalf("失败批应 committed=false，实际 %v", out["committed"])
	}
	if fmt.Sprint(out["nresults"]) != "2" {
		t.Fatalf("results 应有 2 条，实际 %v", out["nresults"])
	}
	if e, _ := out["err_at"].(string); !strings.Contains(e, "第 2 条") {
		t.Fatalf("应指明第 2 条失败，实际 %q", e)
	}
	if n, _ := out["left"].(int); n != 0 {
		t.Fatalf("回滚后应查不到写入，实际剩 %v 行", out["left"])
	}
}

// AC-B2：全部成功 → committed=true，rows_affected 与语句一一对应。
func TestLuaDBBatchSuccessShape(t *testing.T) {
	host := luaBatchHost(t)
	out, err := luaRunHost(t, `
		ctx.db.exec({ driver = "sqlite", sql = "CREATE TABLE kv(k TEXT PRIMARY KEY, v INTEGER)" })
		local r = ctx.db.batch({ driver = "sqlite", statements = {
			{ sql = "INSERT INTO kv VALUES('a', 1)" },
			{ sql = "UPDATE kv SET v = 2 WHERE k = 'a'" },
			{ sql = "UPDATE kv SET v = 3 WHERE k = 'nope'" },
		} })
		local after = ctx.db.query({ driver = "sqlite", sql = "SELECT v FROM kv WHERE k='a'" })
		return { committed = r.committed, rows = r.rows_affected, got = after[1].v }
	`, nil, host)
	if err != nil {
		t.Fatalf("全成功批不该报错: %v", err)
	}
	if out["committed"] != true {
		t.Fatalf("应 committed=true，实际 %v", out["committed"])
	}
	rows, ok := out["rows"].([]interface{})
	if !ok || len(rows) != 3 {
		t.Fatalf("rows_affected 应为 3 项数组，实际 %#v", out["rows"])
	}
	// 逐项核对受影响的条数： INSERT 1 行 / UPDATE 命中 1 行 / UPDATE 命中 0 行。
	want := []int64{1, 1, 0}
	for i, w := range want {
		if got, ok := rows[i].(int64); !ok || got != w {
			t.Fatalf("第 %d 条 rows_affected 应为 %d，实际 %#v", i+1, w, rows[i])
		}
	}
	if fmt.Sprint(out["got"]) != "2" {
		t.Fatalf("批后的写入应可见，实际 %v", out["got"])
	}
}

// AC-B3：DDL 与 DML 混批，失败时 DDL 也不残留（SQLite 语义；Q3 拍板接受混批）。
func TestLuaDBBatchRollsBackDDL(t *testing.T) {
	host := luaBatchHost(t)
	out, err := luaRunHost(t, `
		ctx.db.exec({ driver = "sqlite", sql = "CREATE TABLE orders(id INTEGER)" })
		ctx.db.batch({ driver = "sqlite", statements = {
			{ sql = "CREATE TABLE audit(id INTEGER)" },
			{ sql = "INSERT INTO orders VALUES(1)" },
			{ sql = "INSERT INTO no_such_table VALUES(1)" },
		} })
		local ddl = ctx.db.query({ driver = "sqlite",
			sql = "SELECT name FROM sqlite_master WHERE type='table' AND name='audit'" })
		local dml = ctx.db.query({ driver = "sqlite", sql = "SELECT id FROM orders" })
		return { ddl = #ddl, dml = #dml }
	`, nil, host)
	if err != nil {
		t.Fatalf("DDL 混批应能执行: %v", err)
	}
	if n, _ := out["ddl"].(int); n != 0 {
		t.Fatalf("DDL 应随回滚消失，实际查到 %v 条", out["ddl"])
	}
	if n, _ := out["dml"].(int); n != 0 {
		t.Fatalf("DML 应回滚，实际 %v 行", out["dml"])
	}
}

// AC-B6 的后半：能力目录（gear_capabilities 的数据源）里 db 族方法列表必须含 batch。
// 漏在这里 = 智能体查得到 db 却查不到 batch，只能靠猜参数——注册期不报错，运行期才发现。
func TestDBBatchIsInCapabilityCatalog(t *testing.T) {
	for _, c := range capabilityCatalog(nil) {
		if c["name"] != "db" {
			continue
		}
		methods, ok := c["methods"].(map[string]MethodSchema)
		if !ok {
			t.Fatalf("db 条目没有方法表: %#v", c)
		}
		for _, want := range []string{"query", "exec", "batch", "ping"} {
			if _, ok := methods[want]; !ok {
				got := make([]string, 0, len(methods))
				for m := range methods {
					got = append(got, m)
				}
				t.Fatalf("能力目录里 db 族应含方法 %s，实为 %v", want, got)
			}
		}
		return
	}
	t.Fatal("能力目录里没有 db 条目")
}

// 调用本身不成立（不是某条语句失败）：statements 缺 sql → 报错，不做静默 no-op。
func TestLuaDBBatchRejectsBadStatements(t *testing.T) {
	host := luaBatchHost(t)
	cases := []struct {
		desc string
		body string
		want string
	}{
		{"空数组", `ctx.db.batch({ driver = "sqlite", statements = {} })`, "statements"},
		{"缺 sql", `ctx.db.batch({ driver = "sqlite", statements = { { params = {} } } })`, "sql"},
		{"元素类型错", `ctx.db.batch({ driver = "sqlite", statements = { 42 } })`, "statements"},
	}
	for _, c := range cases {
		t.Run(c.desc, func(t *testing.T) {
			_, err := luaRunHost(t, c.body, nil, host)
			if err == nil {
				t.Fatalf("%s：应报错", c.desc)
			}
			if !strings.Contains(err.Error(), c.want) && !strings.Contains(err.Error(), "至少一条") {
				t.Fatalf("%s：错误信息应点名 %q，实际 %v", c.desc, c.want, err)
			}
		})
	}
}
