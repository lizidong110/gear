package app

// launch_openurl_test.go：scanOpenURLs 的纯函数用例（从应用日志捞「打开地址」标记）。

import (
	"os"
	"path/filepath"
	"testing"
)

func TestScanOpenURLs(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "app.log")

	// 不存在的日志 → nil（不把「没日志」变成错误）。
	if got := scanOpenURLs(log); got != nil {
		t.Fatalf("不存在的日志应返回 nil，实际 %v", got)
	}

	cases := []struct {
		name string
		body string
		want []string
	}{
		{
			name: "单地址",
			body: "some log line\nGEAR_OPEN_URL=http://127.0.0.1:8080/\nother line\n",
			want: []string{"http://127.0.0.1:8080/"},
		},
		{
			name: "多地址去重",
			body: "GEAR_OPEN_URL=http://127.0.0.1:8080/\nGEAR_OPEN_URL=http://127.0.0.1:8080/\nGEAR_OPEN_URL=http://127.0.0.1:8090/\n",
			want: []string{"http://127.0.0.1:8080/", "http://127.0.0.1:8090/"},
		},
		{
			name: "带前后空格",
			body: "  GEAR_OPEN_URL= http://127.0.0.1:8080/  \n",
			want: []string{"http://127.0.0.1:8080/"},
		},
		{
			name: "无标记 → nil",
			body: "boot ok\nlistening on :8080\n",
			want: nil,
		},
		{
			name: "空值标记 → 忽略",
			body: "GEAR_OPEN_URL=\nGEAR_OPEN_URL=http://x/\n",
			want: []string{"http://x/"},
		},
		{
			// 台账是追加日志的投影：旧版本把绑定地址（0.0.0.0/::）当访问地址打进了日志，
			// 只修生产端的话这些历史行仍会以坏链接出现在面板上（REQ-EXT-SERVE-URL）。
			name: "通配绑定归一化为回环（兼容旧日志）",
			body: "GEAR_OPEN_URL=http://0.0.0.0:8080/\n" +
				"GEAR_OPEN_URL=http://[::]:8090/\n" +
				"GEAR_OPEN_URL=http://127.0.0.1:8080/\n",
			want: []string{"http://127.0.0.1:8080/", "http://[::1]:8090/"},
		},
		{
			// 负向：显式声明的具体地址是用户主动给的访问入口，不得改写。
			name: "具体地址保持原样",
			body: "GEAR_OPEN_URL=http://192.168.1.23:8080/\n",
			want: []string{"http://192.168.1.23:8080/"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := os.WriteFile(log, []byte(c.body), 0o644); err != nil {
				t.Fatal(err)
			}
			got := scanOpenURLs(log)
			if len(got) != len(c.want) {
				t.Fatalf("长度不符：got=%v want=%v", got, c.want)
			}
			for i := range c.want {
				if got[i] != c.want[i] {
					t.Fatalf("[%d] 不符：got=%v want=%v", i, got, c.want)
				}
			}
		})
	}
}
