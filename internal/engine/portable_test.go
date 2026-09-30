package engine

import "testing"

// TestCheckPortablePaths 锁死「机器相关绝对路径」的判定口径（与 tools/portable_audit.py 对齐）。
func TestCheckPortablePaths(t *testing.T) {
	cases := []struct {
		name  string
		yaml  string
		want  int // 期望命中次数
		codes []string
	}{
		{
			name: "db dsn 硬编码盘符绝对路径",
			yaml: "kind: Tool\nid: t\ndsn: D:/gadern/gear/build/mindstorm.db\n",
			want: 1,
		},
		{
			name: "ext.serve dir 硬编码 Unix 机器根",
			yaml: "dir: /Users/920/apps/mindstorm/www\n",
			want: 1,
		},
		{
			name: "盘符挂在 /d（Git-Bash/MSYS 风格）",
			yaml: "  local DSN = \"/d/gadern/gear/build/x.db\"\n",
			want: 1,
		},
		{
			name: "http 协议串不误伤",
			yaml: "base_url: http://127.0.0.1:8790/api\n",
			want: 0,
		},
		{
			name: "相对安装根不误伤",
			yaml: "dir: apps/mindstorm/www\n",
			want: 0,
		},
		{
			name: "裸名 entry 不误伤",
			yaml: "entry: \"gear serve --addr 127.0.0.1:8790\"\n",
			want: 0,
		},
		{
			name: "相对库名 dsn 不误伤",
			yaml: "dsn: mindstorm.db\n",
			want: 0,
		},
		{
			name: "多文档 YAML 各报各的",
			yaml: "kind: Tool\nid: a\ndsn: C:\\\\tmp\\\\a.db\n---\nkind: Tool\nid: b\ndir: /home/foo/www\n",
			want: 2,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			diags := CheckPortablePaths(c.yaml)
			if len(diags) != c.want {
				t.Fatalf("期望命中 %d 处，实际 %d 处：%v", c.want, len(diags), diags)
			}
			for _, d := range diags {
				if d.Code != CodePortablePathHardcoded {
					t.Fatalf("错误码应为 %s，实际 %s", CodePortablePathHardcoded, d.Code)
				}
				if d.Hint == "" || d.Message == "" {
					t.Fatalf("诊断必须带 message 与 hint：%v", d)
				}
			}
		})
	}
}
