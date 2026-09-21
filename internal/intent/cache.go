// Package intent — 判定结果缓存。
//
// 为什么缓存：下游 AI 编码客户端（IDE 插件、补全服务）会高频用
// 几乎相同的 prompt 反复打网关。每次命中都要跑「多组正则 + 内容扫描 +
// JSON 提取」的 Decide()，而这些判定结果对同一个 body 是确定性的
// （同一 rules + 同一 model 名 → 同一结论）。命中 LRU 后热路径从
// O(正则组数×文本长度) 降到 O(1)。
//
// 关键约束
//   - 仅缓存「auto 模型」路径的判定；显式模型名的判定是纯查表，不值得缓存。
//   - 缓存键 = SHA-1(排序序列化 body + requestedModel + 规则指纹)。
//     规则一改，指纹变，全部旧缓存静默失效，无需手动清。
//   - TTL 默认 10 分钟：防止上游模型清单更新后判定结果漂移。
package intent

import (
	"crypto/sha1"
	"encoding/json"
	"sync"
	"sync/atomic"
	"time"
)

// Cache 是带 TTL 的 LRU 意图判定缓存。
// 并发安全：单锁 + 切片（容量 ≤512，临界区极短）。
type Cache struct {
	mu        sync.Mutex
	items     map[string]*entry
	order     []string // 最近使用在前（order[0] 最新）
	ttl       time.Duration
	max       int

	// 全局命中/未命中计数（原子，控制台可观测）
	hits   atomic.Int64
	misses atomic.Int64
}

type entry struct {
	decidedAt time.Time
	res       Result
}

// NewCache 创建缓存。ttl ≤ 0 视为 10 分钟；max ≤ 0 视为 512。
func NewCache(ttl time.Duration, max int) *Cache {
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	if max <= 0 {
		max = 512
	}
	return &Cache{items: make(map[string]*entry, max), ttl: ttl, max: max}
}

// Hits 全局命中计数（控制台观测用）。
func (c *Cache) Hits() int64     { return c.hits.Load() }
// Misses 全局未命中计数（控制台观测用）。
func (c *Cache) Misses() int64   { return c.misses.Load() }
// Size 当前缓存条目数。
func (c *Cache) Size() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.items)
}

// TTL 返回当前 TTL（控制台观测用）。
func (c *Cache) TTL() time.Duration { return c.ttl }

// Key 计算缓存键。
//
// 排序序列化 body（Go map 迭代顺序不稳定，必须排键）+ requestedModel +
// 规则指纹（任何规则改动都改变指纹 → 旧缓存全部 miss，不手动清）。
func (c *Cache) Key(body map[string]any, rules Rules, minConf float64, contentScan bool, requestedModel string) string {
	keys := make([]string, 0, len(body))
	for k := range body {
		keys = append(keys, k)
	}
	// 简易插入排序（键数通常 < 30）
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	h := sha1.New()
	for _, k := range keys {
		h.Write([]byte(k))
		h.Write([]byte{0})
		v, _ := json.Marshal(body[k])
		h.Write(v)
		h.Write([]byte{0})
	}
	h.Write([]byte(requestedModel))
	h.Write([]byte(rulesFingerprint(rules, minConf, contentScan)))
	sum := h.Sum(nil)
	const hexchars = "0123456789abcdef"
	out := make([]byte, 40)
	for i := 0; i < 20; i++ {
		v := sum[i]
		out[i*2] = hexchars[v>>4]
		out[i*2+1] = hexchars[v&0x0f]
	}
	return string(out)
}

// Get 取缓存；未命中或过期返回 nil（并计一次 misses）。
func (c *Cache) Get(key string) *Result {
	c.mu.Lock()
	e, ok := c.items[key]
	if !ok {
		c.mu.Unlock()
		c.misses.Add(1)
		return nil
	}
	if time.Since(e.decidedAt) > c.ttl {
		delete(c.items, key)
		c.removeFromOrderLocked(key)
		c.mu.Unlock()
		c.misses.Add(1)
		return nil
	}
	// 命中：提到最前
	c.moveToFrontLocked(key)
	res := e.res
	c.mu.Unlock()
	c.hits.Add(1)
	return &res
}

// Put 写入缓存。已存在则更新时间戳并提前；新键按 LRU 淘汰策略插入。
func (c *Cache) Put(key string, res Result) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.items[key]; ok {
		c.moveToFrontLocked(key)
	} else {
		if len(c.items) >= c.max {
			evict := c.order[len(c.order)-1]
			delete(c.items, evict)
			c.order = c.order[:len(c.order)-1]
		}
		c.order = append([]string{key}, c.order...)
	}
	c.items[key] = &entry{decidedAt: time.Now(), res: res}
}

func (c *Cache) moveToFrontLocked(key string) {
	for i, k := range c.order {
		if k == key {
			c.order = append(c.order[:i], c.order[i+1:]...)
			c.order = append([]string{key}, c.order...)
			return
		}
	}
}

func (c *Cache) removeFromOrderLocked(key string) {
	for i, k := range c.order {
		if k == key {
			c.order = append(c.order[:i], c.order[i+1:]...)
			return
		}
	}
}

// rulesFingerprint 规则集 + 阈值 + 开关的指纹。任何一项变化都会改变指纹。
func rulesFingerprint(rules Rules, minConf float64, contentScan bool) string {
	// 用 sha1 简化拼接；长度固定、无 JSON 编码歧义
	h := sha1.New()
	_ = json.NewEncoder(h).Encode(minConf)
	_ = json.NewEncoder(h).Encode(contentScan)
	_ = json.NewEncoder(h).Encode(len(rules.ImagePatterns))
	_ = json.NewEncoder(h).Encode(len(rules.VideoPatterns))
	_ = json.NewEncoder(h).Encode(len(rules.NegativePatterns))
	// 逐条正则写 raw pattern 串（不是已编译字符串，长度更短且稳定）
	for _, p := range rules.ImagePatterns {
		h.Write([]byte(p.String()))
		h.Write([]byte{0})
	}
	for _, p := range rules.VideoPatterns {
		h.Write([]byte(p.String()))
		h.Write([]byte{0})
	}
	for _, p := range rules.NegativePatterns {
		h.Write([]byte(p.String()))
		h.Write([]byte{0})
	}
	return string(h.Sum(nil))
}
