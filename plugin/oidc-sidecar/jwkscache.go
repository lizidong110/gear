package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// jwksEntry 是某个 issuer 的一份 JWKS 缓存。
//
// keys 是**已经验过的**：从 jwks_uri 拉回来的原样数组，不挑不滤（挑等于替 IdP 改了密钥集）。
type jwksEntry struct {
	keys      []interface{}
	fetchedAt time.Time
}

// jwksCache 是跨请求、**跨进程生命期**的 JWKS 缓存。
//
// 为什么必须长驻：每个应用每接一次 IdP 都去拉一遍 JWKS，等于把 IdP 的可用性绑在
// 应用流量上——IdP 一抖就是全网抖动。缓存的代价只是「新签的钥匙要等一次刷新」，
// 而 AC-E1-3 用「未知 kid 自动刷新」把那一格补上了。
type jwksCache struct {
	mu     sync.Mutex
	byID   map[string]*jwksEntry // key = issuer 的 scheme://host（小写）
	client *http.Client
}

func newJWKSCache(client *http.Client) *jwksCache {
	return &jwksCache{byID: map[string]*jwksEntry{}, client: client}
}

// get 返回某个 issuer 的 JWKS 键集。
//
// 语义（AC-E1-3）：
//   - 命中缓存 ⇒ **一次 HTTP 都不发**（调用方拿它做「缓存是否真的生效」的判据）；
//   - 未命中 ⇒ 拉取并落缓存；拉取失败 ⇒ **报错**，不返回空集（空集会让人以为 IdP 没钥匙）；
//   - 需要强制刷新（未知 kid）时走 refresh，失败则**退回旧缓存并置 stale=true**——
//     fail-soft 但不是静默：旧钥匙能用是运气好，stale 就是那句提醒。
func (c *jwksCache) get(issuer string) ([]interface{}, bool, error) {
	key := normOrigin(issuer)
	c.mu.Lock()
	e, hit := c.byID[key]
	c.mu.Unlock()
	if hit {
		cp := make([]interface{}, len(e.keys))
		copy(cp, e.keys)
		return cp, false, nil
	}
	if _, err := c.refresh(issuer); err != nil {
		return nil, false, err
	}
	c.mu.Lock()
	e = c.byID[key]
	c.mu.Unlock()
	cp := make([]interface{}, len(e.keys))
	copy(cp, e.keys)
	return cp, false, nil
}

// getForKid 返回某个 issuer 的 JWKS 键集，并保证其中**含有** kid。
//
// 这是给 code_exchange 之后的应用用的：它拿到的 id_token 带 kid，要拿 JWKS 去验签，
// 而缓存可能是上一个小时抓的（那期间 IdP 转过钥匙）。所以「未知 kid ⇒ 强制刷新一次」。
//
// kid 为空且缓存里有**多把**钥匙时报错（"无 kid 且多把钥匙即报错，不猜"）——
// 这条口径与内核 `crypto.jwt_verify` 的 key 选择一致：猜钥匙是验签失败里最难查的一类。
func (c *jwksCache) getForKid(issuer, kid string) ([]interface{}, bool, error) {
	c.mu.Lock()
	e, hit := c.byID[normOrigin(issuer)]
	c.mu.Unlock()
	if hit {
		if kid == "" {
			// 无 kid + 多把钥匙：不猜。这一支**必须**覆盖「缓存已命中」，
			// 否则「第一次调用没缓存 ⇒ 放行、第二次才报错」会变成一个随调用次数变化的
			// 随机行为——同一个令牌验两次，一次过一次不过。
			if len(e.keys) > 1 {
				return nil, false, &oidcErr{code: errAmbiguousKid,
					msg: fmt.Sprintf("JWKS 里有 %d 把钥匙而令牌没带 kid：不猜用哪把（建议按 kid 取）",
						len(e.keys))}
			}
			cp := make([]interface{}, len(e.keys))
			copy(cp, e.keys)
			return cp, false, nil // 只有一把 ⇒ 无需猜
		}
		if kidIn(e.keys, kid) {
			cp := make([]interface{}, len(e.keys))
			copy(cp, e.keys)
			return cp, false, nil
		}
	}
	// 走到这里：首次拉取，或命中但 kid 不在里面（⇒ 强制刷新一次）。
	keys, err := c.refresh(issuer)
	if err != nil {
		if hit {
			// 刷新失败：旧缓存比没有强（fail-soft），但 stale=true 必须跟着出去。
			cp := make([]interface{}, len(e.keys))
			copy(cp, e.keys)
			return cp, true, nil
		}
		return nil, false, err
	}
	if kid == "" && len(keys) > 1 {
		return nil, false, &oidcErr{code: errAmbiguousKid,
			msg: fmt.Sprintf("JWKS 里有 %d 把钥匙而令牌没带 kid：不猜用哪把（建议按 kid 取）", len(keys))}
	}
	if kid != "" && !kidIn(keys, kid) {
		return nil, false, &oidcErr{code: errUnknownKid,
			msg: fmt.Sprintf("JWKS 刷新后仍没有 kid=%q（当前有 %d 把钥匙）", kid, len(keys))}
	}
	return keys, false, nil
}

// getOrVerifyKid 是给 `jwks` 方法用的入口：不给 kid 时就是 get（原样返回键集），
// 给了则保证「键集里确实有这把钥匙」——没得选就 Refresh 一次。
//
// 为什么不让 getForKid 兼办「无 kid 返回全量」：那会把「无 kid + 多把钥匙」那条
// ambiguous_kid 岔接到每一次裸取键集的调用上，而裸取键集（应用只是想看一眼现在有哪些钥匙）
// 是无辜的。口径分在两个入口上，各自都只做一件事。
func (c *jwksCache) getOrVerifyKid(issuer, kid string) ([]interface{}, bool, error) {
	if kid == "" {
		return c.get(issuer)
	}
	return c.getForKid(issuer, kid)
}

// refresh 强制拉一次 JWKS 并替换缓存。返回**新**的键集。
//
// 超时与 discovery 共用同一个 client，因此「统一 10s」这条口径在缓存这条路上
// 也没有第二条实现可以绕过去。
func (c *jwksCache) refresh(issuer string) ([]interface{}, error) {
	doc, err := discover(c.client, issuer)
	if err != nil {
		return nil, err
	}
	resp, err := c.client.Get(doc.jwksURI)
	if err != nil {
		return nil, &oidcErr{code: errNoJWKS, msg: fmt.Sprintf("JWKS 拉取失败: %v", err)}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, &oidcErr{code: errNoJWKS, msg: fmt.Sprintf("JWKS 响应读取失败: %v", err)}
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, &oidcErr{code: errNoJWKS,
			msg: fmt.Sprintf("JWKS 端点返回 HTTP %d", resp.StatusCode)}
	}
	var m map[string]interface{}
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, &oidcErr{code: errNoJWKS, msg: fmt.Sprintf("JWKS 不是合法 JSON: %v", err)}
	}
	raw, ok := m["keys"]
	if !ok {
		return nil, &oidcErr{code: errNoJWKS, msg: "JWKS 缺少 keys 字段"}
	}
	arr, ok := raw.([]interface{})
	if !ok || len(arr) == 0 {
		return nil, &oidcErr{code: errNoJWKS, msg: "JWKS 的 keys 是空数组或不是数组"}
	}
	c.mu.Lock()
	c.byID[normOrigin(issuer)] = &jwksEntry{keys: arr, fetchedAt: time.Now()}
	c.mu.Unlock()
	return arr, nil
}

func kidIn(keys []interface{}, kid string) bool {
	for _, k := range keys {
		m, ok := k.(map[string]interface{})
		if !ok {
			continue
		}
		if strings.EqualFold(fmt.Sprint(m["kid"]), kid) {
			return true
		}
	}
	return false
}
