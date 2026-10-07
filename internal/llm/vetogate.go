// VetoGate memoizes the soft model opinion for a repeated, identical entry
// proposal so a live loop that keeps re-proposing the same position does not
// bill the model every poll. The hard risk limits (stop distance, position
// size, kill switch) are NOT part of this cache — the engine re-checks those
// every cycle; only the second-opinion verdict is reused, and only while the
// proposal is unchanged.
package llm

import (
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/rdone44/trading-agent-go/internal/config"
)

// cachedVeto is one memoized verdict and when it was produced.
type cachedVeto struct {
	blocked bool
	reason  string
	at      time.Time
}

// VetoGate caches veto verdicts for a short TTL around VetoDecision. It is
// safe for concurrent use; a runner owns exactly one gate. The verdict cache
// is keyed on (symbol, side, rounded quantity) so the same proposed position
// reuses one model call, while a genuinely different proposal gets a fresh
// opinion. A non-positive TTL disables the cache (every call is live).
type VetoGate struct {
	cfg  config.LLM
	ttl  time.Duration
	mu   sync.Mutex
	seen map[string]cachedVeto
}

// NewVetoGate builds a gate over VetoDecision with the TTL from the config's
// VetoCacheSec (a non-positive value disables caching entirely).
func NewVetoGate(cfg config.LLM) *VetoGate {
	ttl := time.Duration(cfg.VetoCacheSec) * time.Second
	if ttl < 0 {
		ttl = 0
	}
	return &VetoGate{cfg: cfg, ttl: ttl, seen: make(map[string]cachedVeto)}
}

// Decide returns the gate's verdict for a proposal. Within the TTL an
// unchanged proposal reuses the last verdict (blocked or not); a new proposal
// or an expired verdict makes a fresh VetoDecision call.
func (g *VetoGate) Decide(req VetoRequest) (blocked bool, reason string) {
	if g.ttl <= 0 {
		return VetoDecision(g.cfg, req)
	}

	key := cacheKey(req)
	now := time.Now()

	g.mu.Lock()
	defer g.mu.Unlock()
	if v, ok := g.seen[key]; ok && now.Sub(v.at) < g.ttl {
		return v.blocked, v.reason
	}
	b, r := VetoDecision(g.cfg, req)
	g.seen[key] = cachedVeto{blocked: b, reason: r, at: now}
	// Opportunistic prune of expired entries so the map cannot grow without
	// bound over a long-running loop.
	for k, v := range g.seen {
		if k != key && now.Sub(v.at) >= g.ttl {
			delete(g.seen, k)
		}
	}
	return b, r
}

// cacheKey is the stable identity of "the same proposed position": the
// symbol, side and a coarsely-rounded quantity. Entry price is deliberately
// omitted so the same-size proposal is reused even as the live price ticks;
// the engine's per-cycle risk checks cover any price movement.
func cacheKey(r VetoRequest) string {
	return fmt.Sprintf("%s|%s|q=%s", r.Symbol, r.Side, strconv.FormatFloat(r.Quantity, 'f', 6, 64))
}
