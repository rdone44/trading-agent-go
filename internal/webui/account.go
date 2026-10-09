package webui

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/rdone44/trading-agent-go/internal/broker"
)

// handleAccount reads the authenticated user's USDT-margined futures wallet.
// It does not initialize a trading runner, alter exchange settings, or start
// a session. The live gate controls writes, not this read-only account view.
func (s *Server) handleAccount(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, fmt.Errorf("该接口只接受 GET 请求"))
		return
	}
	cfg := s.applyUserConfig(r)
	key, secret := cfg.Live.ExchangeAPIKey, cfg.Live.ExchangeSecretKey
	if !cfg.Live.NoEnvKeys {
		if key == "" {
			key = os.Getenv("BINANCE_API_KEY")
		}
		if secret == "" {
			secret = os.Getenv("BINANCE_SECRET_KEY")
		}
	}
	if key == "" || secret == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("请先在设置中保存 Binance API Key 与 Secret Key，才能读取合约账户"))
		return
	}
	options := broker.FuturesConfig{APIKey: key, SecretKey: secret, DryRun: true}
	loader := s.AccountLoader
	if loader == nil {
		loader = func(opts broker.FuturesConfig) (broker.AccountBalance, error) {
			return broker.NewFutures(opts).Account()
		}
	}
	balance, err := loader(options)
	if err != nil {
		var transport *url.Error
		if errors.As(err, &transport) {
			// The HTTP client's transport error contains the signed URL, so
			// never echo it (including its signature) to a browser or log.
			writeError(w, http.StatusBadGateway, fmt.Errorf("连接 Binance 合约接口失败，请检查网络或代理"))
			return
		}
		writeError(w, http.StatusBadGateway, fmt.Errorf("读取 Binance 合约账户失败: %w", err))
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, struct {
		broker.AccountBalance
		Venue     string    `json:"venue"`
		Source    string    `json:"source"`
		UpdatedAt time.Time `json:"updated_at"`
	}{balance, "futures", "binance", time.Now().UTC()})
}
