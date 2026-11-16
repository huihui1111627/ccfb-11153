// Package router picks which address records to answer with, based on
// client subnet, traffic weights, health status and backup addresses.
package router

import (
	"math/rand"
	"net"
	"sync"

	"gyndns/internal/zone"
)

// Health reports whether a record's target is currently considered up.
type Health interface {
	// Up returns false only when the record has a health check configured
	// and that check is currently failing.
	Up(recKey string) bool
}

// Router implements zone.Selector.
type Router struct {
	Health Health
	rnd    *rand.Rand
	mu     sync.Mutex
}

// New creates a Router. h may be nil (everything considered healthy).
func New(h Health) *Router {
	return &Router{Health: h, rnd: rand.New(rand.NewSource(1))}
}

// Select filters and weights candidate address records for one query.
//
// Fallback chain:
//  1. healthy records whose Subnets match the client (nearest DC)
//  2. healthy unscoped (global) records
//  3. when nothing is scoped at all, any healthy record
//  4. backup records (all normal addresses down)
//
// Within a tier, one record is picked by weighted random.
func (r *Router) Select(cands []*zone.Record, client net.IP, trace func(string, ...any)) []*zone.Record {
	if len(cands) == 0 {
		return nil
	}
	// 1. subnet scoping
	var scoped, unscoped []*zone.Record
	for _, c := range cands {
		if len(c.Subnets) == 0 {
			unscoped = append(unscoped, c)
			continue
		}
		if client != nil && matchAny(c.Subnets, client) {
			scoped = append(scoped, c)
		} else {
			trace("记录 %s 的子网限定 %v 不匹配客户端 %s, 跳过", c.RData, c.Subnets, ipStr(client))
		}
	}
	if len(scoped) > 0 {
		trace("客户端 %s 命中 %d 条子网专属记录", ipStr(client), len(scoped))
	}

	healthy := func(pool []*zone.Record) (up, backups []*zone.Record) {
		for _, c := range pool {
			if c.Backup {
				backups = append(backups, c)
				continue
			}
			if r.Health != nil && !r.Health.Up(c.Key()) {
				trace("记录 %s 健康检查失败或被强制下线, 摘除", c.RData)
				continue
			}
			up = append(up, c)
		}
		return up, backups
	}

	var allBackups []*zone.Record
	// tier 1+2: scoped healthy, then unscoped healthy
	tiers := [][]*zone.Record{scoped, unscoped}
	if len(scoped) == 0 && len(unscoped) == 0 {
		// nothing matches the client and nothing is global: last resort
		trace("无子网专属记录命中, 也无全局记录, 使用全部候选兜底")
		tiers = [][]*zone.Record{cands}
	}
	for _, pool := range tiers {
		up, backups := healthy(pool)
		allBackups = append(allBackups, backups...)
		if len(up) > 0 {
			return r.weighted(up, trace)
		}
	}
	// tier 3: backups
	if len(allBackups) > 0 {
		trace("常规地址全部不可用, 启用 %d 条兜底地址", len(allBackups))
		return r.weighted(allBackups, trace)
	}
	trace("无可用地址且无兜底记录")
	return nil
}

// weighted picks one record by weight and traces the decision.
func (r *Router) weighted(pool []*zone.Record, trace func(string, ...any)) []*zone.Record {
	total := 0
	for _, c := range pool {
		total += c.EffectiveWeight()
	}
	r.mu.Lock()
	n := r.rnd.Intn(total)
	r.mu.Unlock()
	acc := 0
	for _, c := range pool {
		acc += c.EffectiveWeight()
		if n < acc {
			trace("按权重选中 %s (权重 %d/%d)", c.RData, c.EffectiveWeight(), total)
			return []*zone.Record{c}
		}
	}
	return []*zone.Record{pool[len(pool)-1]}
}

func matchAny(cidrs []string, ip net.IP) bool {
	for _, c := range cidrs {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			continue
		}
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

func ipStr(ip net.IP) string {
	if ip == nil {
		return "未知"
	}
	return ip.String()
}
