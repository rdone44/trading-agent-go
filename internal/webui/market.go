package webui

import (
	"fmt"
	"net/http"
	"regexp"
	"time"

	"github.com/rdone44/trading-agent-go/internal/marketdata"
)

var marketSymbolPattern = regexp.MustCompile(`^[A-Z0-9]{4,30}$`)

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
