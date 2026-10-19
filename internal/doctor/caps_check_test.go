package doctor

// caps_check_test.go —— doctor 的「声明 ↔ 实现」/caps 校验。
//
// 覆盖 ExtensionServiceSpecs 与 ProbeServiceCaps，四种状态各一例，并带判据自检：
// 只验「能跑通」的测试抓不到写反的比对（例如永远返回 ok），故 ok 用例额外断言
// Implemented 非空——若 /caps 压根没被解析却仍判 ok，说明判据是空的。
//
// 2026-10-03 从 internal/mcp 迁到 internal/doctor：判据本体已搬进本包（CLI 的 gear doctor
// 也要用同一份），测试跟着判据走，不留在调用方。

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExtensionServiceSpecsOnlyServiceExt(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "extensions.yaml")
	content := `ext.once:
  cmd: ["./x"]
  methods: [a]
  schemas:
    a: {description: d}
ext.svc:
  cmd: ["./y"]
  methods: [b, c]
  service: true
  listen: "127.0.0.1:1234"
  schemas:
    b: {description: d}
    c: {description: d}
ext.svc_nolisten:
  cmd: ["./z"]
  methods: [d]
  service: true
  schemas:
    d: {description: d}
`
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	got := ExtensionServiceSpecs(p)
	if len(got) != 1 {
		t.Fatalf("应只有 1 个可探测的 service ext，实际 %d: %+v", len(got), got)
	}
	if got[0].Name != "ext.svc" || got[0].Listen != "127.0.0.1:1234" {
		t.Fatalf("解析错: %+v", got[0])
	}
	if strings.Join(got[0].Methods, ",") != "b,c" {
		t.Fatalf("methods 解析错: %v", got[0].Methods)
	}
	// 负向：once 型（要起进程才能问、无 /caps 约定）与缺 listen 的都不得进入探测范围。
	for _, s := range got {
		if s.Name == "ext.once" || s.Name == "ext.svc_nolisten" {
			t.Fatalf("不该被探测的 ext 进入了清单: %s", s.Name)
		}
	}
}

func TestExtensionServiceSpecsBadConfigReturnsEmpty(t *testing.T) {
	// 负向：空路径 / 语法错都不该 panic，也不该假装校验过（返回空即未校验）。
	if got := ExtensionServiceSpecs(""); len(got) != 0 {
		t.Fatalf("空路径应返回空，实际 %+v", got)
	}
	p := filepath.Join(t.TempDir(), "bad.yaml")
	if err := os.WriteFile(p, []byte("ext.x: [unbalanced"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := ExtensionServiceSpecs(p); len(got) != 0 {
		t.Fatalf("语法错应返回空（装配期会报更准的错），实际 %+v", got)
	}
}

func TestProbeServiceCapsFourStates(t *testing.T) {
	cases := []struct {
		name     string
		handler  http.HandlerFunc
		declared []string
		want     string
		missing  []string
	}{
		{
			name: "一致",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"result":{"ext":"svc","methods":["a","b"]}}`))
			},
			declared: []string{"a", "b"},
			want:     "ok",
		},
		{
			name: "声明的方法侧车没实现",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"result":{"ext":"svc","methods":["a"]}}`))
			},
			declared: []string{"a", "b"},
			want:     "mismatch",
			missing:  []string{"b"},
		},
		{
			name:     "侧车起来了但没实现 /caps",
			handler:  func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "nope", http.StatusNotFound) },
			declared: []string{"a"},
			want:     "probe_failed",
		},
		{
			name: "/caps 返回的不是 JSON",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`<html>not json</html>`))
			},
			declared: []string{"a"},
			want:     "probe_failed",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.handler)
			defer srv.Close()
			listen := strings.TrimPrefix(srv.URL, "http://")
			got := ProbeServiceCaps(ServiceExtSpec{Name: "ext.svc", Listen: listen, Methods: tc.declared})
			if got.State != tc.want {
				t.Fatalf("state = %q，要 %q（missing=%v implemented=%v）", got.State, tc.want, got.Missing, got.Implemented)
			}
			if strings.Join(got.Missing, ",") != strings.Join(tc.missing, ",") {
				t.Fatalf("missing = %v，要 %v", got.Missing, tc.missing)
			}
			if tc.want == "ok" && len(got.Implemented) == 0 {
				t.Fatal("state=ok 但 Implemented 为空 —— /caps 没真被解析，判据是空的")
			}
		})
	}
}

func TestProbeServiceCapsNotRunningIsNotAnError(t *testing.T) {
	// 端口 1 没人听，connect 应立刻 refused —— 侧车未运行**不是错误**
	//（service ext 只在应用运行时才被拉起），报成错误会淹掉真问题。
	got := ProbeServiceCaps(ServiceExtSpec{Name: "ext.svc", Listen: "127.0.0.1:1", Methods: []string{"a"}})
	if got.State != "sidecar_not_running" {
		t.Fatalf("侧车未运行应报 sidecar_not_running，实际 %q", got.State)
	}
}
