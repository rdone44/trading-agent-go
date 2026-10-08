package webui

import (
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/rdone44/trading-agent-go/internal/marketdata"
)

var marketSymbolPattern = regexp.MustCompile(`^[A-Z0-9]{4,30}$`)

// symbolsCacheTTL: the all-pair list is a heavy exchange answer, so the
// console refreshes it at most once every 30 seconds per venue.
const symbolsCacheTTL = 30 * time.Second

// cachedSymbols is one venue's pair list plus the time it was fetched.
type cachedSymbols struct {
	infos []marketdata.SymbolInfo
	at    time.Time
}

func (s *Server) handleMarket(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, 405, fmt.Errorf("该接口只接受 GET"))
		return
	}
	symbol := marketdata.BinanceSymbol(r.URL.Query().Get("symbol"))
	if symbol == "" {
		symbol = s.Config.Agent.Symbol
	}
	interval := r.URL.Query().Get("interval")
	if interval == "" {
		interval = "1h"
	}
	venue := r.URL.Query().Get("venue")
	if venue == "" {
		venue = "spot"
	}
	if !marketSymbolPattern.MatchString(symbol) || !marketdata.ValidMarketInterval(interval) || (venue != "spot" && venue != "futures") {
		writeError(w, 400, fmt.Errorf("交易对、场所或周期无效"))
		return
	}
	key := symbol + "/" + venue + "/" + interval
	s.marketMu.Lock()
	defer s.marketMu.Unlock()
	if cached, ok := s.marketCache[key]; ok && time.Since(cached.UpdatedAt) < 10*time.Second {
		writeJSON(w, 200, cached)
		return
	}
	loader := s.MarketLoader
	if loader == nil {
		loader = marketdata.Snapshot
	}
	snapshot, err := loader(symbol, venue == "futures", interval)
	if err != nil {
		writeError(w, 502, fmt.Errorf("行情获取失败：%w", err))
		return
	}
	if s.marketCache == nil || len(s.marketCache) > 24 {
		s.marketCache = map[string]marketdata.MarketSnapshot{}
	}
	s.marketCache[key] = snapshot
	writeJSON(w, 200, snapshot)
}

// handleSymbols answers the console's "get me all the coins" request: it
// returns the venue's tradable USDT/USDC pairs, liquidity-ranked, so the
// symbol field offers the whole market instead of making the user type a
// ticker they may not know. Repeats within the 30-second cache window for a
// venue are served without another heavy exchange call.
func (s *Server) handleSymbols(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, 405, fmt.Errorf("该接口只接受 GET"))
		return
	}
	venue := r.URL.Query().Get("venue")
	if venue != "futures" {
		venue = "spot"
	}
	limit := 200
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			writeError(w, 400, fmt.Errorf("limit 无效"))
			return
		}
		limit = n
	}

	s.symbolsMu.Lock()
	defer s.symbolsMu.Unlock()
	key := venue
	if cached, ok := s.symbolsCache[key]; ok && time.Since(cached.at) < symbolsCacheTTL {
		infos := cached.infos
		if limit > 0 && len(infos) > limit {
			infos = infos[:limit]
		}
		writeJSON(w, 200, map[string]any{"venue": venue, "symbols": infos})
		return
	}

	loader := s.SymbolList
	if loader == nil {
		loader = marketdata.AllSymbols
	}
	infos, err := loader(venue, 0)
	if err != nil {
		writeError(w, 502, fmt.Errorf("币种列表获取失败：%w", err))
		return
	}
	if s.symbolsCache == nil {
		s.symbolsCache = map[string]cachedSymbols{}
	}
	s.symbolsCache[key] = cachedSymbols{infos: infos, at: time.Now()}

	if limit > 0 && len(infos) > limit {
		infos = infos[:limit]
	}
	writeJSON(w, 200, map[string]any{"venue": venue, "symbols": infos})
}
