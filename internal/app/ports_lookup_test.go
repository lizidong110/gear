package app

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"gear/internal/model"
	"gear/internal/registry"
)

// newApp 构造一个能通过 Validate 的最小 App 清单。
func newApp(id, entryFlow string, ports []int) *model.App {
	return &model.App{
		ID:        id,
		Name:      id,
		EntryFlow: entryFlow,
		Start:     model.AppStart{Restart: "always", Ports: ports},
	}
}

// regWithApps 打开一个临时 registry 并注册给定 App（探活回退的测试夹具）。
func regWithApps(t *testing.T, apps ...*model.App) string {
	t.Helper()
	db := filepath.Join(t.TempDir(), "registry.db")
	reg, err := registry.Open(db)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer reg.Close()
	for _, a := range apps {
		if err := reg.UpsertApp(a); err != nil {
			t.Fatalf("UpsertApp %s: %v", a.ID, err)
		}
	}
	return db
}

// TestResolveStartPorts 覆盖回退链「入参 > App 清单 start.ports > 无」的四条路径。
// 这是 REQ-MCP-PORTS-ONE-TRUTH 的核心口径：解析必须单一入口（ResolveStartPorts），
// 生效来源必须如实上报，任何一侧都不许再各自实现一份。
func TestResolveStartPorts(t *testing.T) {
	bundlePorts := []int{8080, 9090}
	lookup := func(flowID string) ([]int, bool, error) {
		return bundlePorts, true, nil
	}

	t.Run("显式入参优先，lookup 不该被问", func(t *testing.T) {
		asked := false
		ports, source, err := ResolveStartPorts(StartOptions{
			Ports:  []int{7000},
			FlowID: "f1",
			AppLookup: func(string) ([]int, bool, error) {
				asked = true
				return bundlePorts, true, nil
			},
		})
		if err != nil || source != PortsSourceExplicit || len(ports) != 1 || ports[0] != 7000 {
			t.Fatalf("期望 explicit [7000]，实际 %v %s %v", ports, source, err)
		}
		if asked {
			t.Fatal("显式入参时不应反查 App 清单")
		}
	})

	t.Run("入参为空回退清单", func(t *testing.T) {
		ports, source, err := ResolveStartPorts(StartOptions{FlowID: "f1", AppLookup: lookup})
		if err != nil || source != PortsSourceBundle || len(ports) != 2 {
			t.Fatalf("期望 bundle 回退，实际 %v %s %v", ports, source, err)
		}
	})

	t.Run("lookup 未命中报 none 而非错误", func(t *testing.T) {
		_, source, err := ResolveStartPorts(StartOptions{
			FlowID:    "f1",
			AppLookup: func(string) ([]int, bool, error) { return nil, false, nil },
		})
		if err != nil || source != PortsSourceNone {
			t.Fatalf("期望 none 且无错，实际 %s %v", source, err)
		}
	})

	t.Run("无 lookup 也是 none", func(t *testing.T) {
		_, source, err := ResolveStartPorts(StartOptions{FlowID: "f1"})
		if err != nil || source != PortsSourceNone {
			t.Fatalf("期望 none 且无错，实际 %s %v", source, err)
		}
	})

	t.Run("lookup 出错必须向上传播", func(t *testing.T) {
		_, _, err := ResolveStartPorts(StartOptions{
			FlowID: "f1",
			AppLookup: func(string) ([]int, bool, error) {
				return nil, false, errBoom
			},
		})
		if err == nil {
			t.Fatal("读 registry 失败要红，不能静默降级成 none")
		}
	})
}

var errBoom = errors.New("boom")

// TestAppPortsFromRegistry 覆盖标准 lookup：唯一命中才回退——0 个（裸 Flow）与
// 多个（多个 App 共用同一 entry_flow）都不回退，歧义即放弃。
func TestAppPortsFromRegistry(t *testing.T) {
	t.Run("唯一命中返回清单端口", func(t *testing.T) {
		db := regWithApps(t, newApp("myapp", "myapp.up", []int{8080}))
		ports, matched, err := AppPortsFromRegistry(db, "myapp.up")
		if err != nil || !matched || len(ports) != 1 || ports[0] != 8080 {
			t.Fatalf("期望命中 [8080]，实际 %v %v %v", ports, matched, err)
		}
	})

	t.Run("没有 App 是 miss 不是错", func(t *testing.T) {
		db := regWithApps(t)
		if _, matched, err := AppPortsFromRegistry(db, "x.up"); err != nil || matched {
			t.Fatalf("期望 miss，实际 %v %v", matched, err)
		}
	})

	t.Run("多个 App 共用 entry_flow 是 miss（歧义即放弃）", func(t *testing.T) {
		db := regWithApps(t,
			newApp("a1", "shared.up", []int{8080}),
			newApp("a2", "shared.up", []int{9090}),
		)
		if _, matched, err := AppPortsFromRegistry(db, "shared.up"); err != nil || matched {
			t.Fatalf("期望歧义 miss，实际 %v %v", matched, err)
		}
	})

	t.Run("命中但清单没声明端口也是 miss", func(t *testing.T) {
		db := regWithApps(t, newApp("noports", "np.up", nil))
		if _, matched, err := AppPortsFromRegistry(db, "np.up"); err != nil || matched {
			t.Fatalf("期望 miss，实际 %v %v", matched, err)
		}
	})
}

// TestResolveStartPortsBadLookupErrFormat 锁错误文案的可归因性：
// 反查失败的红必须说明是「反查 App 清单」这一步，否则和别的启动失败混在一起没法排。
func TestResolveStartPortsBadLookupErrFormat(t *testing.T) {
	_, _, err := ResolveStartPorts(StartOptions{
		FlowID: "f1",
		AppLookup: func(string) ([]int, bool, error) {
			return nil, false, errBoom
		},
	})
	if err == nil || !strings.Contains(err.Error(), "反查 App 清单") {
		t.Fatalf("错误应指明反查步骤，实际 %v", err)
	}
}
