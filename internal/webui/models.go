package webui

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/rdone44/trading-agent-go/internal/config"
	"github.com/rdone44/trading-agent-go/internal/llm"
)

// handleModels asks the configured model endpoint which models it serves, so
// the settings panel can offer a picker instead of a blank text box. It is the
// model-side counterpart of /api/symbols: the user should choose from what
// actually exists, not type a name from memory.
//
// The endpoint and token are taken from the request when the form supplies
// them (so a model can be picked before the settings are saved), otherwise
// from whatever is already stored for this account or this machine. The token
// is only ever sent to the endpoint the user configured, and it is never
// echoed back.
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, fmt.Errorf("该接口只接受 POST 请求"))
		return
	}
	var body struct {
		LLMBaseURL string `json:"llm_base_url"`
		LLMAPIKey  string `json:"llm_api_key"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("请求体不是合法的 JSON: %w", err))
		return
	}

	cfg := s.applyUserConfig(r)
	baseURL := strings.TrimSpace(body.LLMBaseURL)
	if baseURL == "" {
		baseURL = cfg.LLM.BaseURL
	}
	apiKey := strings.TrimSpace(body.LLMAPIKey)
	if apiKey == "" {
		apiKey = cfg.LLM.APIKey
	}
	if apiKey == "" && !cfg.LLM.NoEnvKey {
		apiKey = config.LLMAPIKey()
	}
	if apiKey == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("获取模型列表需要 AI Token：先在设置里填写并保存 Token"))
		return
	}

	lister := s.ModelList
	if lister == nil {
		lister = llm.ListModels
	}
	models, err := lister(baseURL, apiKey)
	if err != nil {
		writeError(w, http.StatusBadGateway, fmt.Errorf("获取模型列表失败：%w", err))
		return
	}
	// Model names are data, not secrets, so the list goes back verbatim.
	writeJSON(w, http.StatusOK, map[string]any{"base_url": baseURL, "models": models})
}
