package main

import (
	"gear/internal/sidecar/describe"
)

// 本文件是 ext.mongo 的**自描述**：`mongo-sidecar --describe` 会把这里的内容打成 JSON，
// 内核加载时（`extensions.yaml` 里写 `describe: true`）据此得到 methods 与 schemas。
//
// 为什么描述放在这里而不是 extensions.yaml：
// 参数真正被读取的地方是同包的 main.go（strArg / toDoc / intArg …）。描述与实现同处一包，
// 改一个 handler 的入参时描述就在眼前；写在 yaml 里则是第二份手工同步的副本，
// 漂移不报错，只在 agent 照着过期描述组装入参时才炸。
//
// 描述**刻意精简**：只说「这个参数是什么、要不要必填」，不复制 main.go 里的边界规则
// （那些是代码的事，抄一遍又会漂）。

// 连接参数：每个方法都要带（侧车无状态，不做配置库回退）。
var connParams = map[string]describe.Param{
	"uri":         {Type: "string", Description: "完整 mongodb:// 或 mongodb+srv:// URI；给了它就忽略 host/port/username/password"},
	"host":        {Type: "string", Description: "主机（与 uri 二选一，只含主机不含端口）"},
	"port":        {Type: "number", Description: "端口（默认 27017）"},
	"username":    {Type: "string", Description: "用户名（给了才带凭据，默认 authSource=admin）"},
	"password":    {Type: "string", Description: "密码"},
	"auth_source": {Type: "string", Description: "认证库（默认 admin）"},
	"timeout_ms":  {Type: "number", Description: "本次操作超时毫秒（默认 8000，上限 9000）"},
}

var (
	dbParam   = describe.Param{Type: "string", Description: "库名（禁用 / \\ . 空格 \" $）", Required: true}
	collParam = describe.Param{Type: "string", Description: "集合名", Required: true}
)

// m 组装一个方法的描述：needDB=true 时加必填的 database，再并入方法自有参数与连接参数。
func m(desc string, needDB bool, extra map[string]describe.Param) describe.Method {
	ps := make(map[string]describe.Param, len(extra)+len(connParams)+2)
	if needDB {
		ps["database"] = dbParam
	}
	for k, v := range extra {
		ps[k] = v
	}
	for k, v := range connParams {
		ps[k] = v
	}
	return describe.Method{Description: desc, Parameters: ps}
}

func mongoDescribe() describe.Spec {
	// 读
	readExtra := map[string]describe.Param{
		"filter":     {Type: "object", Description: "过滤条件（Extended JSON）"},
		"projection": {Type: "object", Description: "字段投影"},
		"sort":       {Type: "object", Description: "排序（有序文档）"},
	}
	findExtra := map[string]describe.Param{
		"limit": {Type: "number", Description: "最多返回条数（默认 100，上限 10000）"},
		"skip":  {Type: "number", Description: "跳过条数"},
	}
	for k, v := range readExtra {
		findExtra[k] = v
	}

	// 写
	updExtra := map[string]describe.Param{
		"filter":        {Type: "object", Description: "过滤条件"},
		"upsert":        {Type: "boolean", Description: "没命中就插入"},
		"array_filters": {Type: "array", Description: "数组过滤（配合 $[] 定位符）"},
	}
	oneExtra := map[string]describe.Param{
		"upsert":          {Type: "boolean", Description: "没命中就插入"},
		"projection":      {Type: "object", Description: "字段投影"},
		"sort":            {Type: "object", Description: "命中多条时按此排序取第一条"},
		"return_document": {Type: "string", Description: "before（默认，改前）或 after（改后）", Enum: []string{"before", "after"}},
	}
	for k, v := range updExtra {
		if k != "array_filters" {
			oneExtra[k] = v
		}
	}

	schemas := map[string]describe.Method{
		"ping": m("连通性探测（mongo.Connect 不发网络包，ping 才是真的连上了）", false, nil),

		"find":            m("查询文档（默认最多 100 条，上限 10000）", true, merge(collParamExtra(), findExtra)),
		"find_one":        m("查单条（没命中返回 document=null，不报错）", true, merge(collParamExtra(), readExtra, map[string]describe.Param{"skip": {Type: "number", Description: "跳过条数"}})),
		"count":           m("按条件计数（要快而不要准用 estimated_count）", true, merge(collParamExtra(), map[string]describe.Param{"filter": readExtra["filter"], "limit": {Type: "number", Description: "最多计多少条"}, "skip": {Type: "number", Description: "跳过条数"}})),
		"estimated_count": m("集合估算条数（读元数据，快；不带 filter）", true, collParamExtra()),
		"aggregate": m("聚合管道", true, merge(collParamExtra(), map[string]describe.Param{
			"pipeline":       {Type: "array", Description: "聚合阶段数组", Required: true},
			"allow_disk_use": {Type: "boolean", Description: "允许落盘"},
			"batch_size":     {Type: "number", Description: "每批取回条数"},
		})),
		"distinct": m("取某字段的去重值列表", true, merge(collParamExtra(), map[string]describe.Param{
			"field":  {Type: "string", Description: "字段名", Required: true},
			"filter": readExtra["filter"],
		})),

		"insert_one": m("插入一条（返回 inserted_id）", true, merge(collParamExtra(), map[string]describe.Param{
			"document": {Type: "object", Description: "文档", Required: true},
		})),
		"insert_many": m("批量插入（最多 10000 条）", true, merge(collParamExtra(), map[string]describe.Param{
			"documents": {Type: "array", Description: "文档数组", Required: true},
			"ordered":   {Type: "boolean", Description: "是否按序执行（默认 true）"},
		})),
		"update_one": m("更新第一条命中的（update 必须带 $set/$inc 等操作符）", true, merge(collParamExtra(), updExtra, map[string]describe.Param{
			"update": {Type: "object", Description: "更新表达式", Required: true},
		})),
		"update_many": m("更新全部命中的", true, merge(collParamExtra(), updExtra, map[string]describe.Param{
			"update": {Type: "object", Description: "更新表达式", Required: true},
		})),
		"replace_one": m("整条替换（replacement 不能含操作符）", true, merge(collParamExtra(), map[string]describe.Param{
			"filter":      {Type: "object", Description: "过滤条件"},
			"replacement": {Type: "object", Description: "新文档", Required: true},
			"upsert":      {Type: "boolean", Description: "没命中就插入"},
		})),
		"delete_one":  m("删除第一条命中的", true, merge(collParamExtra(), map[string]describe.Param{"filter": readExtra["filter"]})),
		"delete_many": m("删除全部命中的（filter 给 {} 即清空集合）", true, merge(collParamExtra(), map[string]describe.Param{"filter": readExtra["filter"]})),

		"find_one_and_update": m("原子「改一条并返回」（默认返回改前文档）", true, merge(collParamExtra(), oneExtra, map[string]describe.Param{
			"filter": {Type: "object", Description: "过滤条件"},
			"update": {Type: "object", Description: "更新表达式", Required: true},
		})),
		"find_one_and_replace": m("原子「替换一条并返回」", true, merge(collParamExtra(), oneExtra, map[string]describe.Param{
			"filter":      {Type: "object", Description: "过滤条件"},
			"replacement": {Type: "object", Description: "新文档", Required: true},
		})),
		"find_one_and_delete": m("原子「删一条并返回被删的」", true, merge(collParamExtra(), map[string]describe.Param{
			"filter":     readExtra["filter"],
			"projection": readExtra["projection"],
			"sort":       readExtra["sort"],
		})),
		"bulk_write": m("单次调用内的批量写（本能力提供的原子批量手段）", true, merge(collParamExtra(), map[string]describe.Param{
			"operations": {Type: "array", Description: "操作数组（每项带 type，最多 1000）", Required: true},
			"ordered":    {Type: "boolean", Description: "是否按序（默认 true）"},
		})),

		"create_index": m("建索引（返回服务端生成的索引名）", true, merge(collParamExtra(), map[string]describe.Param{
			"keys":                 {Type: "object", Description: "索引键（有序对象，如 {name: 1}）", Required: true},
			"name":                 {Type: "string", Description: "索引名（不给由服务端生成）"},
			"unique":               {Type: "boolean", Description: "唯一索引"},
			"sparse":               {Type: "boolean", Description: "稀疏索引"},
			"expire_after_seconds": {Type: "number", Description: "TTL 秒数"},
		})),
		"drop_index": m("删索引（不知道名字先调 list_indexes）", true, merge(collParamExtra(), map[string]describe.Param{
			"name": {Type: "string", Description: "索引名", Required: true},
		})),
		"list_indexes": m("列索引", true, collParamExtra()),

		"create_collection": m("建集合（可选固定容量 capped）", true, merge(collParamExtra(), map[string]describe.Param{
			"capped":     {Type: "boolean", Description: "是否固定容量集合"},
			"size_bytes": {Type: "number", Description: "capped 的字节上限"},
			"max_docs":   {Type: "number", Description: "capped 的条数上限"},
		})),
		"drop_collection": m("删集合（连数据带索引一起没了，不可恢复）", true, collParamExtra()),
		"rename_collection": m("同库改名", true, merge(collParamExtra(), map[string]describe.Param{
			"new_name":    {Type: "string", Description: "新集合名", Required: true},
			"drop_target": {Type: "boolean", Description: "目标已存在时先删掉它"},
		})),
		"list_collections": m("列本库的集合名", true, map[string]describe.Param{"filter": readExtra["filter"]}),
		"list_databases":   m("列全部库（不需要 database 参数）", false, map[string]describe.Param{"name_only": {Type: "boolean", Description: "只要库名，不要体积"}}),
		"stats":            m("集合统计（collStats）", true, collParamExtra()),

		"run_command": m("逃生口——在本库上跑任意数据库命令", true, map[string]describe.Param{
			"command": {Type: "object", Description: "命令文档（如 {ping: 1}）", Required: true},
		}),
		"watch": m("有界变更流（等最多 max_wait_ms 后一次返回；要求副本集）", true, merge(collParamExtra(), map[string]describe.Param{
			"pipeline":    {Type: "array", Description: "变更流过滤管道"},
			"max_wait_ms": {Type: "number", Description: "最长等待毫秒（默认 3000，上限 8000）"},
			"batch":       {Type: "number", Description: "最多取回条数（默认 100，上限 1000）"},
		})),
	}

	return describe.Spec{Methods: methods, Schemas: schemas}
}

// collParamExtra 返回只含必填 collection 的参数集（多数方法的起点）。
func collParamExtra() map[string]describe.Param {
	return map[string]describe.Param{"collection": collParam}
}

// merge 合并若干参数集（后者覆盖前者）。
func merge(ps ...map[string]describe.Param) map[string]describe.Param {
	out := map[string]describe.Param{}
	for _, p := range ps {
		for k, v := range p {
			out[k] = v
		}
	}
	return out
}
