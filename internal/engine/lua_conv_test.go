package engine

import (
	"testing"

	lua "github.com/yuin/gopher-lua"
)

// TestLuaRoundTripPreservesEmptySlice 锁住 2026-09-22 修的「0 行变 {}」缺陷。
//
// 场景：`ctx.db.query` 查到 0 行 → Go 的空 slice 进了 Lua → 回程时被 luaTableToGo 的
// 「非空且全数字键才算数组」判成 map，于是 `[]` 变成 `{}`，调用方**无法区分
// 「查到 0 行」与「出错」**（同一个 `{}` 两种含义）。
//
// 修法：Go→Lua 时给数组 table 挂 metatable 标记，回程据此定夺。这条用例是那个标记的回归锁。
func TestLuaRoundTripPreservesEmptySlice(t *testing.T) {
	L := lua.NewState()
	defer L.Close()

	got := fromLuaValue(L, toLuaValue(L, []interface{}{}))
	arr, ok := got.([]interface{})
	if !ok {
		t.Fatalf("空数组回程应仍是数组，实际 %T (%v)", got, got)
	}
	if len(arr) != 0 {
		t.Fatalf("空数组回程应为空，实际 %v", arr)
	}
}

// TestLuaRoundTripKeepsEmptyMapAsMap：空 map 仍必须是 `{}`。
// 与上一条成对存在——只把空表一律当数组，等于把另一个方向的类型也搞坏。
func TestLuaRoundTripKeepsEmptyMapAsMap(t *testing.T) {
	L := lua.NewState()
	defer L.Close()

	got := fromLuaValue(L, toLuaValue(L, map[string]interface{}{}))
	m, ok := got.(map[string]interface{})
	if !ok {
		t.Fatalf("空 map 回程应仍是 map，实际 %T (%v)", got, got)
	}
	if len(m) != 0 {
		t.Fatalf("空 map 回程应为空，实际 %v", m)
	}
}

// TestLuaRoundTripNonEmptyAndNested：非空数组 / 嵌套结构不能在修复中被误伤。
func TestLuaRoundTripNonEmptyAndNested(t *testing.T) {
	L := lua.NewState()
	defer L.Close()

	// 具名元素类型的 slice 也要走数组分支（它此前正是踩中「空 slice 建出 0 键 table」那条路）。
	in := []map[string]interface{}{
		{"id": float64(1), "tags": []interface{}{"a"}},
		{"id": float64(2), "tags": []interface{}{}},
	}
	got := fromLuaValue(L, toLuaValue(L, in))
	arr, ok := got.([]interface{})
	if !ok || len(arr) != 2 {
		t.Fatalf("应回 2 元素数组，实际 %T (%v)", got, got)
	}
	row0, _ := arr[0].(map[string]interface{})
	if row0["id"] != int64(1) {
		t.Fatalf("整数值应回 int64，实际 %T %v", row0["id"], row0["id"])
	}
	row1, _ := arr[1].(map[string]interface{})
	inner, ok := row1["tags"].([]interface{})
	if !ok || len(inner) != 0 {
		t.Fatalf("嵌套空数组同样不应退化，实际 %T (%v)", row1["tags"], row1["tags"])
	}
}

// TestLuaTableToGoStillInfersArrayWithoutMarker：**Lua 侧新建**的 table 没有标记，
// 仍按「非空且全数字键」推断——不能因为引入标记就丢掉既有的推断能力。
func TestLuaTableToGoStillInfersArrayWithoutMarker(t *testing.T) {
	L := lua.NewState()
	defer L.Close()

	arrLike := L.NewTable()
	arrLike.RawSetInt(1, lua.LString("x"))
	arrLike.RawSetInt(2, lua.LString("y"))
	if got, ok := luaTableToGo(L, arrLike).([]interface{}); !ok || len(got) != 2 {
		t.Fatalf("非空数字键表应推断为数组，实际 %T (%v)", got, got)
	}

	mapLike := L.NewTable()
	mapLike.RawSetString("k", lua.LString("v"))
	if got, ok := luaTableToGo(L, mapLike).(map[string]interface{}); !ok || got["k"] != "v" {
		t.Fatalf("字符串键表应判为 map，实际 %T (%v)", got, got)
	}

	// 已知边界（写下来免得以后被当成回归）：Lua 里手写的空表没有标记，
	// 仍会落进 map 分支变成 `{}` —— 标记只保证「Go 产出 → 原样带回」这条路径。
	bare := L.NewTable()
	if got, ok := luaTableToGo(L, bare).(map[string]interface{}); !ok || len(got) != 0 {
		t.Fatalf("无标记的空表仍是 map（已知边界），实际 %T (%v)", got, got)
	}
}

// TestLuaArrayMarkerKeepsTableUsable：标记挂在 metatable 上，不得污染表内容
// （pairs/ipairs/# 看不到它，也不影响取值）。
func TestLuaArrayMarkerKeepsTableUsable(t *testing.T) {
	L := lua.NewState()
	defer L.Close()

	v := toLuaValue(L, []interface{}{"a", "b"})
	tb, ok := v.(*lua.LTable)
	if !ok {
		t.Fatalf("应是 table，实际 %T", v)
	}
	if tb.Len() != 2 {
		t.Fatalf("长度应为 2（标记不能混进数组部分），实际 %d", tb.Len())
	}
	count := 0
	tb.ForEach(func(_, _ lua.LValue) { count++ })
	if count != 2 {
		t.Fatalf("ForEach 应只看到 2 个元素（标记必须在 metatable 里），实际 %d", count)
	}
	if tb.RawGetString(luaArrayMarker) != lua.LNil {
		t.Fatal("标记不得出现在表本身")
	}
}
