package registry

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"gear/internal/model"
)

// ④「留旧/回滚」这组测试守的是一条工程结论：**快照必须下沉到唯一写入口**。
// 每个用例都从「如果快照漏了/写错会发生什么」倒着写，而不是先写实现再补测试。

func histTool(id, entry string) *model.Tool {
	return &model.Tool{
		ID: id, Name: id, Runtime: model.RuntimeLua, Entry: entry,
		InputSchema:  map[string]interface{}{"type": "object"},
		OutputSchema: map[string]interface{}{"type": "object"},
		Versions:     []model.ToolVersion{{Version: "1.0.0", State: model.StateStable}},
	}
}

func latest(t *testing.T, reg *Registry, kind, id string) (HistoryRow, bool) {
	t.Helper()
	rows, err := reg.ListHistory(kind, id, 1)
	if err != nil {
		t.Fatalf("查历史失败: %v", err)
	}
	if len(rows) == 0 {
		return HistoryRow{}, false
	}
	return rows[0], true
}

// H-1：任何一次 upsert 都要留痕——**新建与更新都算**，否则回滚链在第 2 步就断了。
func TestUpsertAlwaysLeavesHistory(t *testing.T) {
	reg := openHist(t)
	defer reg.Close()

	if err := reg.UpsertTool(histTool("demo__a", "return 1")); err != nil {
		t.Fatal(err)
	}
	first, ok := latest(t, reg, "tool", "demo__a")
	if !ok {
		t.Fatal("新建后应当有历史")
	}
	// 第一条历史记录的是「当时还不存在」⇒ data 为 NULL，restore 它等于撤回这次新建。
	if first.Seq != 1 {
		t.Fatalf("seq 应从 1 开始，实际 %d", first.Seq)
	}

	if err := reg.UpsertTool(histTool("demo__a", "return 2")); err != nil {
		t.Fatal(err)
	}
	second, _ := latest(t, reg, "tool", "demo__a")
	if second.Seq <= first.Seq {
		t.Fatalf("再次 upsert 应产生新 seq：%d vs %d", second.Seq, first.Seq)
	}
	rows, err := reg.ListHistory("tool", "demo__a", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("新建 + 更新应共 2 条历史，实际 %d", len(rows))
	}
}

// H-2：回滚把「那一行所记录的状态」写回——它记的是**那一刻的内容**，不是「那次改成了什么」。
// 所以最典型的一幕是：改了两版之后，用上一版的那条历史回滚，得到上一版。
func TestRestoreSnapshotRewritesOldValue(t *testing.T) {
	reg := openHist(t)
	defer reg.Close()

	if err := reg.UpsertTool(histTool("demo__a", "return 1")); err != nil {
		t.Fatal(err)
	}
	if err := reg.UpsertTool(histTool("demo__a", "return 2")); err != nil {
		t.Fatal(err)
	}
	// 最新的那条历史记录的就是「return 1」那份内容。
	prev, ok := latest(t, reg, "tool", "demo__a")
	if !ok {
		t.Fatal("应至少有 1 条历史")
	}
	if _, err := reg.RestoreSnapshot(prev.Seq); err != nil {
		t.Fatalf("回滚失败: %v", err)
	}
	tool, err := reg.GetTool("demo__a")
	if err != nil || tool == nil {
		t.Fatalf("回滚后 Tool 应当还在: %v err=%v", tool, err)
	}
	if tool.Entry != "return 1" {
		t.Fatalf("回滚应回到「return 1」，实际 %q", tool.Entry)
	}
}

// H-3：回滚「新建」⇒ 对象被删除（撤销一次新建是同一条通道，不是特例）。
func TestRestoreOfCreationDeletesObject(t *testing.T) {
	reg := openHist(t)
	defer reg.Close()

	if err := reg.UpsertTool(histTool("demo__a", "return 1")); err != nil {
		t.Fatal(err)
	}
	creation, _ := latest(t, reg, "tool", "demo__a")
	if _, err := reg.RestoreSnapshot(creation.Seq); err != nil {
		t.Fatalf("回滚失败: %v", err)
	}
	if tool, err := reg.GetTool("demo__a"); err != nil || tool != nil {
		t.Fatalf("回滚「新建」后对象应被删除，实际 %v err=%v", tool, err)
	}
}

// H-4：回滚本身也是一次变更，必须**再次留痕**且能连续回滚——
// 若回滚不写历史，第二次想回到「坏版本」时链就断了。
func TestRestoreIsItselfSnapshotted(t *testing.T) {
	reg := openHist(t)
	defer reg.Close()

	if err := reg.UpsertTool(histTool("demo__a", "return 1")); err != nil {
		t.Fatal(err)
	}
	s1, _ := latest(t, reg, "tool", "demo__a")
	if err := reg.UpsertTool(histTool("demo__a", "return 2")); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.RestoreSnapshot(s1.Seq); err != nil {
		t.Fatal(err)
	}
	// 回滚产生的那条历史：note 应自证来源。
	note, ok := latest(t, reg, "tool", "demo__a")
	if !ok || !strings.Contains(note.Note, "restore from") {
		t.Fatalf("回滚应留下 note，实际 %+v", note)
	}
	// 再滚一次：朝前走回「return 2」。
	if err := reg.UpsertTool(histTool("demo__a", "return 2")); err != nil {
		t.Fatal(err)
	}
	rows, err := reg.ListHistory("tool", "demo__a", 0)
	if err != nil {
		t.Fatal(err)
	}
	// 1 新建 + 1 改坏 + 1 回滚 + 1 再改动 = 4 条，链路线性不丢。
	if len(rows) != 4 {
		t.Fatalf("历史应线性累积 4 条，实际 %d：%+v", len(rows), rows)
	}
}

// H-5：不存在的 seq 要报错（而不是静默 no-op）——回滚的唯一入口报错才能被听见。
func TestRestoreUnknownSeqIsAnError(t *testing.T) {
	reg := openHist(t)
	defer reg.Close()

	_, err := reg.RestoreSnapshot(999)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("不存在的 seq 应报 ErrNotFound，实际 %v", err)
	}
	if _, err := reg.RestoreSnapshot(0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("seq<=0 应报 ErrNotFound，实际 %v", err)
	}
	// 关键：失败时**不能留下半截写入**（回滚自己也会写历史，失败必须整体回滚）。
	n, err := countHistory(t, reg)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("失败的回滚不应留下历史，实际 %d 条", n)
	}
}

// H-6：回滚会**重新校验**被写回的内容，规则收紧过的快照宁可拒绝，也不给用户一个
// 回滚成功却跑不起来的定义。失败必须原子——一个字节都不写。
func TestRestoreRejectsBlobThatNoLongerValidates(t *testing.T) {
	reg := openHist(t)
	defer reg.Close()

	// 手工塞一条「内容不完整」的历史（等价于一处旧快照在规则收紧后不再合法）。
	res, err := reg.db.Exec("INSERT INTO definition_history(kind,id,data,actor,at,note) "+
		"VALUES('tool','demo__b','{\"id\":\"demo__b\"}','',?, '')", time.Now().UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	seq, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	before, err := countHistory(t, reg)
	if err != nil {
		t.Fatal(err)
	}

	_, err = reg.RestoreSnapshot(int(seq))
	if err == nil {
		t.Fatal("非法快照应被拒绝，实际回滚成功了")
	}
	if !strings.Contains(err.Error(), "不合法") {
		t.Fatalf("拒绝信息要指向「快照太旧」，实际 %v", err)
	}
	// 原子性：回滚事务里已写下的「当前状态」快照也要跟着撤销。
	after, err := countHistory(t, reg)
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("失败的回滚不该留下历史：%d -> %d", before, after)
	}
	if tool, err := reg.GetTool("demo__b"); err != nil || tool != nil {
		t.Fatalf("失败的回滚不该写入对象: %v err=%v", tool, err)
	}
}

// H-7：actor 是可选的，但传了就必须落到历史里；不传时如实记成空串
// （「没主体」与「有个叫 anonymous 的主体」必须分得开，③ 的审计要基于它）。
func TestUpsertActorIsRecorded(t *testing.T) {
	reg := openHist(t)
	defer reg.Close()

	if err := reg.UpsertTool(histTool("demo__a", "return 1"), WithActor("alice")); err != nil {
		t.Fatal(err)
	}
	if err := reg.UpsertTool(histTool("demo__a", "return 2")); err != nil {
		t.Fatal(err)
	}
	rows, err := reg.ListHistory("tool", "demo__a", 0) // DESC：新的在前
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("应有 2 条历史，实际 %d", len(rows))
	}
	if rows[1].Actor != "alice" {
		t.Fatalf("新建那次应记下 alice，实际 %q", rows[1].Actor)
	}
	if rows[0].Actor != "" {
		t.Fatalf("未传 actor 的写入应记成空串，实际 %q", rows[0].Actor)
	}
}

// ③ 审计（2026-09-26）：④ 只把快照下沉进了 upsert，而**删除根本不走 upsert**——
// 于是 definition_history 里查不到任何一次删除。这组用例把那个洞堵上。

// H-8：删除也要留痕，且带上「谁删的」。
func TestDeleteLeavesTraceWithActor(t *testing.T) {
	reg := openHist(t)
	defer reg.Close()

	if err := reg.UpsertTool(histTool("demo__a", "return 1")); err != nil {
		t.Fatal(err)
	}
	before, err := countHistory(t, reg)
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.DeleteTool("demo__a", WithActor("alice")); err != nil {
		t.Fatal(err)
	}

	rows, err := reg.ListHistory("tool", "demo__a", 0)
	if err != nil {
		t.Fatal(err)
	}
	top := rows[0] // DESC：新的在前
	if top.Note != "delete" {
		t.Fatalf("删除那行的 note 应为 delete，实际 %q", top.Note)
	}
	if top.Actor != "alice" {
		t.Fatalf("删除那行的 actor 应为 alice，实际 %q——「谁删的」答不出来", top.Actor)
	}
	if len(rows) != before+1 {
		t.Fatalf("删除应新增 1 条历史，实际 %d -> %d", before, len(rows))
	}
	// 对象确实不在了
	if tool, err := reg.GetTool("demo__a"); err != nil || tool != nil {
		t.Fatalf("删除后应查不到该 Tool: %v err=%v", tool, err)
	}
}

// H-9：删除那行**保留**被删内容 ⇒ RestoreSnapshot 恰好能把对象带回来。
// 「撤销一次删除」因此不需要新代码，回滚语义天然覆盖它。
func TestDeleteCanBeRolledBack(t *testing.T) {
	reg := openHist(t)
	defer reg.Close()

	if err := reg.UpsertTool(histTool("demo__a", "return 1"), WithActor("bob")); err != nil {
		t.Fatal(err)
	}
	if err := reg.DeleteTool("demo__a", WithActor("mallory")); err != nil {
		t.Fatal(err)
	}

	rows, err := reg.ListHistory("tool", "demo__a", 0)
	if err != nil {
		t.Fatal(err)
	}
	del := rows[0]
	if _, err := reg.RestoreSnapshot(del.Seq); err != nil {
		t.Fatalf("回滚删除失败: %v", err)
	}
	tool, err := reg.GetTool("demo__a")
	if err != nil {
		t.Fatal(err)
	}
	if tool == nil {
		t.Fatal("回滚删除后对象应回来")
	}
	// 内容也要一致（回滚不是留个空壳）
	if tool.Entry != "return 1" {
		t.Fatalf("回滚后内容应保持一致，实际 entry=%q", tool.Entry)
	}
}

// H-10（负向）：**被守卫拒绝的删除不得留痕**。
// 守卫与留痕必须原子：否则「删失败却多了一条删除记录」会让时间线凭空长出一格，
// 顺着它回滚会把一个活对象删掉。
func TestGuardedDeleteLeavesNoTrace(t *testing.T) {
	reg := openHist(t)
	defer reg.Close()

	if err := reg.UpsertTool(histTool("demo__a", "return 1")); err != nil {
		t.Fatal(err)
	}
	if err := reg.UpsertFlow(&model.Flow{ID: "demo__f", Steps: []model.Step{
		{ID: "s1", Call: "demo__a", Input: map[string]interface{}{}},
	}}); err != nil {
		t.Fatal(err)
	}
	before, err := countHistory(t, reg)
	if err != nil {
		t.Fatal(err)
	}

	if err := reg.DeleteTool("demo__a", WithActor("alice")); !errors.Is(err, ErrInUse) {
		t.Fatalf("被引用时删除应返回 ErrInUse，实际 %v", err)
	}
	after, err := countHistory(t, reg)
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("被拒绝的删除不该留下历史：%d -> %d", before, after)
	}
}

// H-12：重复删除同一个对象既要**报错**，也要**不留痕**。
// 2026-09-26 回归：下沉进 deleteWithSnapshot 后没人查 RowsAffected，
// DELETE 对不存在的行「成功」却影响 0 行 ⇒ 重复删除静默 no-op（TestHookLifecycle 逮到）。
func TestRepeatedDeleteErrorsAndLeavesNoTrace(t *testing.T) {
	reg := openHist(t)
	defer reg.Close()

	if err := reg.UpsertTool(histTool("demo__a", "return 1")); err != nil {
		t.Fatal(err)
	}
	if err := reg.DeleteTool("demo__a", WithActor("alice")); err != nil {
		t.Fatalf("首次删除应成功，实际 %v", err)
	}
	before, err := countHistory(t, reg)
	if err != nil {
		t.Fatal(err)
	}

	err = reg.DeleteTool("demo__a", WithActor("alice"))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("重复删除应返回 ErrNotFound，实际 %v", err)
	}
	after, err := countHistory(t, reg)
	if err != nil {
		t.Fatal(err)
	}
	// 报错的那次连快照都不该落——否则「我删过两次」在时间线上是个假事件。
	if after != before {
		t.Fatalf("删除不存在的对象不该留下历史：%d -> %d", before, after)
	}
}

// H-11：回滚自身也留痕，且 note 指回被回滚的 seq。
func TestRestoreLeavesTraceWithNote(t *testing.T) {
	reg := openHist(t)
	defer reg.Close()

	if err := reg.UpsertTool(histTool("demo__a", "return 1"), WithActor("alice")); err != nil {
		t.Fatal(err)
	}
	if err := reg.UpsertTool(histTool("demo__a", "return 2")); err != nil {
		t.Fatal(err)
	}
	// 挑那条**内容真的是 return 1** 的历史。注意别顺着 seq 下标猜：
	// 首次写入那一条的 data 是 NULL（=「当时还不存在」），回滚它会把对象删掉。
	rows, err := reg.ListHistory("tool", "demo__a", 0)
	if err != nil {
		t.Fatal(err)
	}
	var target int
	for _, h := range rows {
		if strings.Contains(histData(t, reg, h.Seq), "return 1") {
			target = h.Seq
			break
		}
	}
	if target == 0 {
		t.Fatalf("历史里找不到内容含 return 1 的那一条: %+v", rows)
	}
	if _, err := reg.RestoreSnapshot(target); err != nil {
		t.Fatal(err)
	}
	rows, err = reg.ListHistory("tool", "demo__a", 0)
	if err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf("restore from %d", target); rows[0].Note != want {
		t.Fatalf("回滚应留一条 note=%q 的历史，实际 %q（快照写的 seq 对不上会被误读成回滚了另一步）", want, rows[0].Note)
	}
	// 回滚后内容回到第一版
	tool, _ := reg.GetTool("demo__a")
	if tool == nil || tool.Entry != "return 1" {
		t.Fatalf("回滚后内容应回到第一版，实际 %v", tool)
	}
}

// histData 直读 definition_history.data（HistoryRow 不带这一列，而「这条历史记的是什么」
// 恰恰是挑回滚点时唯一靠谱的依据——只看 seq 会和下标错位）。
func histData(t *testing.T, reg *Registry, seq int) string {
	t.Helper()
	var s string
	if err := reg.db.QueryRow("SELECT data FROM definition_history WHERE seq=?", seq).Scan(&s); err != nil {
		t.Fatalf("读历史 %d 失败: %v", seq, err)
	}
	return s
}

func openHist(t *testing.T) *Registry {
	t.Helper()
	return mustOpen(t)
}

func countHistory(t *testing.T, reg *Registry) (int, error) {
	t.Helper()
	rows, err := reg.ListHistory("", "", 100)
	return len(rows), err
}
