package app

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"time"

	"gear/internal/engine"
	"gear/internal/model"
)

// probeLoop 为单个 service 句柄运行健康检查探针（REQ-APP-HEALTH / #75）。
//
// 设计要点：
//   - 探针与 Supervisor 的「退出→重启」路径解耦：连续失败达到阈值时只杀掉当前进程，
//     由 superviseOne 的 waitAny 收到退出后走既有的 Relaunch（共用崩溃退避）。
//   - 进程被重启（Proc 换柄）后重置失败计数，避免把「重启后的观察窗口」误算成失败。
//   - h.Probe 为 nil（未配置探针）时直接返回，不影响既有无探针服务。
//   - ctx 取消时立即退出，配合 Supervise 的平滑停机。
func (s *Supervisor) probeLoop(ctx context.Context, h *engine.ServiceHandle) {
	probe := h.Probe
	if probe == nil {
		return
	}
	rp, err := probe.Resolve()
	if err != nil {
		// 注册期已校验，运行期不应再出错；出错仅放弃探针，不阻断服务运行。
		fmt.Printf("! step %s 健康检查配置非法，跳过探针: %v\n", h.StepID, err)
		return
	}

	ticker := time.NewTicker(rp.Interval)
	defer ticker.Stop()
	fails := 0
	lastProc := h.CurrentProc() // 记住当前进程，重启后换柄即重置计数

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		if cur := h.CurrentProc(); cur != lastProc {
			fails = 0
			lastProc = cur
		}

		if checkProbe(ctx, rp) {
			fails = 0
			continue
		}
		fails++
		if fails >= rp.Threshold {
			fmt.Printf("⚠ step %s 健康检查连续 %d 次失败，重启 service\n", h.StepID, fails)
			// 杀掉后 waitAny 收到退出，superviseOne 走重启路径（共用退避）。
			_ = h.ProcKill()
			return
		}
	}
}

// checkProbe 按探针类型执行一次健康检查，健康返回 true。
// 任何错误（拨号失败 / 超时 / 非零退出）均视为不健康。
func checkProbe(ctx context.Context, rp *model.ResolvedProbe) bool {
	switch rp.Kind {
	case model.ProbeKindHTTP:
		reqCtx, cancel := context.WithTimeout(ctx, rp.Timeout)
		defer cancel()
		req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, "http://"+rp.Addr+rp.Path, nil)
		if err != nil {
			return false
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		return resp.StatusCode < 400

	case model.ProbeKindTCP:
		conn, err := net.DialTimeout("tcp", rp.Addr, rp.Timeout)
		if err != nil {
			return false
		}
		_ = conn.Close()
		return true

	case model.ProbeKindCmd:
		cmdCtx, cancel := context.WithTimeout(ctx, rp.Timeout)
		defer cancel()
		// 与 process runtime 一致，经 sh -c 执行健康检查命令（开发环境为 Git Bash）。
		if err := exec.CommandContext(cmdCtx, "sh", "-c", rp.Cmd).Run(); err != nil {
			return false
		}
		return true
	}
	return false
}
