package webui

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/rdone44/trading-agent-go/internal/auth"
	"github.com/rdone44/trading-agent-go/internal/config"
	"github.com/rdone44/trading-agent-go/internal/localcreds"
)

// sessionCookie is the HttpOnly cookie carrying the signed user session.
const sessionCookie = "ta_session"

// ---------------------------------------------------------------- credentials

// applyUserCredentials overlays the session user's vault credentials onto the
// effective config: the Binance keys and the LLM endpoint/key. Each value is
// applied only when the user actually stored one. When username is empty the
// user has no stored credentials and the config is returned untouched.
//
// Account mode also sets LLM.NoEnvKey so llm.New never falls back to the
// deployer's LLM_API_KEY / OPENAI_API_KEY environment variables: the model
// runs with the credential the user stored, or not at all. Otherwise a user
// who pointed BaseURL at their own endpoint (or simply generated traffic on
// the deployer's budget) would ride on the deployer's key. The CLI /
// desktop path (no vault) keeps the historical env fallback.
//
// Live.NoEnvKeys applies the same rule to the exchange credentials: an
// account with no stored Binance keys must not trade the deployer's
// BINANCE_API_KEY / BINANCE_SECRET_KEY. The session layer turns that into a
// clear "store your keys first" error instead of silently trading someone
// else's account.
func (s *Server) applyUserCredentials(cfg *config.Config, username string) {
	// The desktop edition has no vault and no login: its single user's saved
	// connection settings are overlaid instead. Values that were never saved
	// stay empty, so the environment-variable fallback keeps working.
	if s.LocalCreds != nil {
		s.LocalCreds.Apply(cfg)
	}
	if s.Auth == nil || username == "" {
		return
	}
	cfg.LLM.NoEnvKey = true
	cfg.Live.NoEnvKeys = true
	user, ok := s.Auth.Get(username)
	if !ok {
		return
	}
	if user.BinAPIKey != "" {
		cfg.Live.ExchangeAPIKey = user.BinAPIKey
	}
	if user.BinSecretKey != "" {
		cfg.Live.ExchangeSecretKey = user.BinSecretKey
	}
	if user.LLMAPIKey != "" {
		cfg.LLM.APIKey = user.LLMAPIKey
	}
	if user.LLMBaseURL != "" {
		cfg.LLM.BaseURL = user.LLMBaseURL
	}
	if user.LLMModel != "" {
		cfg.LLM.Model = user.LLMModel
	}
	if user.LLMPrompt != "" {
		cfg.LLM.Prompt = user.LLMPrompt
	}
}

// requestUsername resolves the session cookie to the account's username, or
// "" when auth is disabled or the request has no session. It is carried on
// BacktestRequest.AuthUser (json:"-") so the credential-bearing handlers can
// inject the right vault values without changing the wire format.
func (s *Server) requestUsername(r *http.Request) string {
	if s.Auth == nil {
		return ""
	}
	cookie, err := r.Cookie(sessionCookie)
	if err != nil {
		return ""
	}
	username, valid := s.Auth.VerifySession(cookie.Value)
	if !valid {
		return ""
	}
	return username
}

// userFromRequest resolves the session cookie to a vault user.
func (s *Server) userFromRequest(r *http.Request) (*auth.User, bool) {
	if s.Auth == nil {
		return nil, false
	}
	cookie, err := r.Cookie(sessionCookie)
	if err != nil {
		return nil, false
	}
	username, valid := s.Auth.VerifySession(cookie.Value)
	if !valid {
		return nil, false
	}
	return s.Auth.Get(username)
}

// ---------------------------------------------------------------- handlers

// localAuthView is the /api/config answer in desktop single-user mode. It has
// the same shape as authView so the page needs no second code path, but there
// is no login: the username is a fixed label, "enabled" is true, and "local"
// tells the UI to hide the account-only controls (logout) and to describe the
// credential file instead of an account.
func (s *Server) localAuthView() map[string]any {
	st := s.LocalCreds.Status()
	return map[string]any{
		"enabled":        true,
		"local":          true,
		"username":       "本机",
		"binance_api":    st.BinAPIKey,
		"binance_secret": st.BinSecretKey,
		"llm_key":        st.LLMAPIKey,
		"llm_base_url":   st.LLMBaseURL,
		"llm_model":      st.LLMModel,
		"llm_prompt":     st.LLMPrompt,
		"allow_live":     st.AllowLive,
		"path":           s.LocalCreds.Path(),
	}
}

// handleLocalCredentials stores the desktop user's connection settings. It is
// registered only when LocalCreds is set (the desktop shell), so the server
// edition never exposes an unauthenticated credential write.
func (s *Server) handleLocalCredentials(w http.ResponseWriter, r *http.Request) {
	if s.LocalCreds == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "本机凭据存储未启用"})
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, fmt.Errorf("该接口只接受 POST 请求"))
		return
	}
	var body struct {
		BinanceAPIKey    string `json:"binance_api_key"`
		BinanceSecretKey string `json:"binance_secret_key"`
		LLMBaseURL       string `json:"llm_base_url"`
		LLMModel         string `json:"llm_model"`
		LLMPrompt        string `json:"llm_prompt"`
		LLMAPIKey        string `json:"llm_api_key"`
		// AllowLive is a pointer so an omitted field leaves the gate as it is;
		// the checkbox always sends it, the credential form never does.
		AllowLive *bool `json:"allow_live"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("请求体不是合法的 JSON: %w", err))
		return
	}
	if base := strings.TrimSpace(body.LLMBaseURL); base != "" {
		parsed, err := url.Parse(base)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			writeError(w, http.StatusBadRequest, fmt.Errorf("AI 服务 URL 必须以 http:// 或 https:// 开头，例如 https://api.openai.com/v1"))
			return
		}
	}
	if err := s.LocalCreds.Set(localcreds.Patch{
		BinanceAPIKey:    body.BinanceAPIKey,
		BinanceSecretKey: body.BinanceSecretKey,
		LLMBaseURL:       body.LLMBaseURL,
		LLMModel:         body.LLMModel,
		LLMPrompt:        body.LLMPrompt,
		LLMAPIKey:        body.LLMAPIKey,
	}); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("保存本机凭据失败: %w", err))
		return
	}
	if body.AllowLive != nil {
		if err := s.LocalCreds.SetAllowLive(*body.AllowLive); err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Errorf("保存实盘开关失败: %w", err))
			return
		}
	}
	// The local file now holds the fresh credentials; push them into any
	// running session so switching model/key/persona works without a restart.
	s.refreshRunningSessions(r)
	view := s.localAuthView()
	view["ok"] = true
	writeJSON(w, http.StatusOK, view)
}

// handleAuthRegister creates an account and opens a session in one step:
// registration is only meaningful for a browser that is about to use it.
func (s *Server) handleAuthRegister(w http.ResponseWriter, r *http.Request) {
	if s.Auth == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "账号登录未启用"})
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, fmt.Errorf("该接口只接受 POST 请求"))
		return
	}
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("请求体不是合法的 JSON: %w", err))
		return
	}
	user, err := s.Auth.Register(body.Username, body.Password)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	s.writeSessionCookie(w, body.Username)
	writeJSON(w, http.StatusOK, s.authView(user.Username))
}

func (s *Server) handleAuthLogin(w http.ResponseWriter, r *http.Request) {
	if s.Auth == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "账号登录未启用"})
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, fmt.Errorf("该接口只接受 POST 请求"))
		return
	}
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("请求体不是合法的 JSON: %w", err))
		return
	}
	user, err := s.Auth.Authenticate(body.Username, body.Password)
	if err != nil {
		writeError(w, http.StatusUnauthorized, err)
		return
	}
	s.writeSessionCookie(w, user.Username)
	writeJSON(w, http.StatusOK, s.authView(user.Username))
}

// handleAuthLogout drops the session; the vault and stored credentials are
// untouched.
func (s *Server) handleAuthLogout(w http.ResponseWriter, r *http.Request) {
	if s.Auth == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "账号登录未启用"})
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, fmt.Errorf("该接口只接受 POST 请求"))
		return
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleAuthMe reports the session user plus which credentials are stored,
// without ever echoing the values themselves.
func (s *Server) handleAuthMe(w http.ResponseWriter, r *http.Request) {
	if s.Auth == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "账号登录未启用"})
		return
	}
	user, ok := s.userFromRequest(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "未登录"})
		return
	}
	writeJSON(w, http.StatusOK, s.authView(user.Username))
}

// handleAuthCredentials stores the exchange and model credentials for the
// logged-in user. Empty fields are ignored, so a partial save only updates
// what the user typed.
func (s *Server) handleAuthCredentials(w http.ResponseWriter, r *http.Request) {
	if s.Auth == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "账号登录未启用"})
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, fmt.Errorf("该接口只接受 POST 请求"))
		return
	}
	user, ok := s.userFromRequest(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "未登录"})
		return
	}
	var body struct {
		BinanceAPIKey    string `json:"binance_api_key"`
		BinanceSecretKey string `json:"binance_secret_key"`
		LLMBaseURL       string `json:"llm_base_url"`
		LLMModel         string `json:"llm_model"`
		LLMPrompt        string `json:"llm_prompt"`
		LLMAPIKey        string `json:"llm_api_key"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("请求体不是合法的 JSON: %w", err))
		return
	}
	if err := s.Auth.SetCredentials(user.Username,
		strings.TrimSpace(body.BinanceAPIKey), strings.TrimSpace(body.BinanceSecretKey),
		strings.TrimSpace(body.LLMBaseURL), strings.TrimSpace(body.LLMModel), strings.TrimSpace(body.LLMPrompt), strings.TrimSpace(body.LLMAPIKey)); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	// The vault now holds the fresh credentials; push them into any running
	// session so switching model/key/persona works without a restart.
	s.refreshRunningSessions(r)
	// The same shape as /api/auth/me: presence flags, never the values.
	view := s.authView(user.Username)
	view["ok"] = true
	writeJSON(w, http.StatusOK, view)
}

// authView is what /api/auth/* answers with: the name plus presence flags.
func (s *Server) authView(username string) map[string]any {
	view, err := s.Auth.Status(username)
	if err != nil {
		view = auth.Status{}
	}
	return map[string]any{
		"username":       username,
		"binance_api":    view.BinAPIKey,
		"binance_secret": view.BinSecretKey,
		"llm_key":        view.LLMAPIKey,
		"llm_base_url":   view.LLMBaseURL,
		"llm_model":      view.LLMModel,
		"llm_prompt":     view.LLMPrompt,
	}
}

// writeSessionCookie issues a 24h signed session for the account.
func (s *Server) writeSessionCookie(w http.ResponseWriter, username string) {
	token, err := s.Auth.IssueSession(username, auth.SessionTTL)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: token, Path: "/",
		MaxAge: int(auth.SessionTTL.Seconds()), HttpOnly: true, SameSite: http.SameSiteLaxMode,
	})
}

// ---------------------------------------------------------------- middleware

// requireUserSession guards the credential-bearing routes when accounts are
// enabled: /api/session*, /api/backtest, /api/tune and the report routes need
// a logged-in user so their keys can be injected. Market data, the config and
// the login endpoints themselves stay public, so an unauthenticated browser
// can still watch prices and see the login card.
func (s *Server) requireUserSession(next http.Handler) http.Handler {
	if s.Auth == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.requestUsername(r) != "" || publicAuthPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "需要登录：请先注册或登录"})
	})
}

func publicAuthPath(path string) bool {
	switch {
	case path == "/api/auth/register", path == "/api/auth/login", path == "/api/auth/logout", path == "/api/auth/me":
		return true
	case path == "/api/config", path == "/api/strategies", path == "/api/market", path == "/api/symbols", path == "/healthz":
		return true
	default:
		return false
	}
}
