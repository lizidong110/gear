// bind.go 守「监听地址」这道闸：无鉴权的网络面默认只许绑回环地址，
// 非回环必须显式 allowRemote（fail-closed）。
//
// 为什么是共享函数而不是各面自写：Gear 有多个无鉴权网络面（MCP、REST 执行面），
// 若各自实现判定，迟早一边收紧一边漂移（评审 B-2 的教训正是「注释写着只应绑 127.0.0.1、
// 实现从不校验」）。判定只有一份，各面只是传入自己的名字与暴露面描述。
//
// 空 host（`:8787`）在 net.Listen 里绑**全网卡**，与 0.0.0.0 等价，故不算回环——
// 这是最容易踩的写法：比 `0.0.0.0:8787` 顺手，却同样静默铺到局域网。
package httpsafe

import (
	"fmt"
	"net"
	"os"
	"strings"
)

// CheckBindAddr 校验监听地址：非回环地址必须显式 allowRemote。
//
// surface 是面名（如 "MCP"、"REST 执行面"），exposure 是一句话说明放开后暴露了什么
// （如 "写 registry / 删对象 / 执行编排"），用于拒绝文案与放行警告——静默放行等于
// 替使用者做了一个他们不知情的决定，必须把代价说清楚。
//
// 必须在 ListenAndServe **之前**调用：等监听上了再报错就晚了。
func CheckBindAddr(surface, addr, exposure string, allowRemote bool) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("%s 监听地址 %q 非法（应为 host:port）: %w", surface, addr, err)
	}
	if IsLoopbackHost(host) {
		return nil
	}
	if allowRemote {
		fmt.Fprintf(os.Stderr,
			"gear: ⚠ %s 监听非回环地址 %s（当前无鉴权：任何能访问该地址的人都可%s）\n",
			surface, addr, exposure)
		return nil
	}
	return fmt.Errorf("拒绝 %s 监听非回环地址 %q：当前无鉴权，绑全网卡（0.0.0.0 或空 host）会把「%s」"+
		"暴露给局域网；请改用 127.0.0.1:<port>（确有远程需求时显式加 --allow-remote）",
		surface, addr, exposure)
}

// IsLoopbackHost 判定 host 是否回环。空 host（`:8788`）视为**全网卡**，不算回环。
func IsLoopbackHost(host string) bool {
	h := strings.ToLower(strings.TrimSpace(host))
	switch h {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	if ip := net.ParseIP(h); ip != nil {
		return ip.IsLoopback()
	}
	return false
}
