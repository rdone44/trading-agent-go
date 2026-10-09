package llm

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// ListModels asks an OpenAI-compatible endpoint which models it serves, so the
// dashboard can offer the real list instead of making the user type a name
// they may misremember. It is the model-side counterpart of the exchange's
// "all symbols" call.
//
// "OpenAI-compatible" covers a wide range of gateways, so the response shape
// is read leniently: the OpenAI {"data":[{"id":…}]} envelope, Ollama's
// {"models":[{"name":…}]}, and a bare ["a","b"] array are all accepted. An
// endpoint that answers none of them produces a clear error instead of an
// empty list, because "no models" and "unrecognised answer" are different
// problems for the person staring at the form.
func ListModels(baseURL, apiKey string) ([]string, error) {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if base == "" {
		base = "https://api.openai.com/v1"
	}
	if apiKey == "" {
		return nil, fmt.Errorf("获取模型列表需要 AI Token：先填写 Token 再获取，或直接手写模型名")
	}
	client := newClient(20 * time.Second)
	response, err := doWithRetry(client, func() (*http.Request, error) {
		request, err := http.NewRequest(http.MethodGet, base+"/models", nil)
		if err != nil {
			return nil, fmt.Errorf("构造模型列表请求失败: %w", err)
		}
		request.Header.Set("Authorization", "Bearer "+apiKey)
		request.Header.Set("Accept", "application/json")
		return request, nil
	})
	if err != nil {
		return nil, fmt.Errorf("连接模型服务失败: %s", describeTransportError(err))
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("模型服务返回 HTTP %d: %s", response.StatusCode, summarizeError(raw))
	}
	models := parseModelList(raw)
	if len(models) == 0 {
		return nil, fmt.Errorf("模型服务没有返回可识别的模型列表：该地址可能不是 OpenAI 兼容接口")
	}
	return models, nil
}

// parseModelList accepts the response shapes listed on ListModels and returns
// the model names, deduplicated, with obvious non-chat models last so the
// useful entries are on top of the picker.
func parseModelList(raw []byte) []string {
	var seen = map[string]bool{}
	var models []string
	add := func(name string) {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			return
		}
		seen[name] = true
		models = append(models, name)
	}

	// {"data":[{"id":"…"}]} and {"models":[{"name":"…"}]}.
	var envelope struct {
		Data []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"data"`
		Models []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.Unmarshal(raw, &envelope); err == nil {
		for _, m := range envelope.Data {
			add(firstNonEmpty(m.ID, m.Name))
		}
		for _, m := range envelope.Models {
			add(firstNonEmpty(m.Name, m.ID))
		}
	}

	// A bare array, either of strings or of objects.
	var bare []json.RawMessage
	if err := json.Unmarshal(raw, &bare); err == nil {
		for _, item := range bare {
			var name string
			if err := json.Unmarshal(item, &name); err == nil {
				add(name)
				continue
			}
			var obj struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			}
			if err := json.Unmarshal(item, &obj); err == nil {
				add(firstNonEmpty(obj.ID, obj.Name))
			}
		}
	}

	// Chat-capable families first, embeddings/audio/image last; ties keep the
	// provider's own order so a gateway's ranking is not thrown away.
	sort.SliceStable(models, func(i, j int) bool {
		return chatLikelihood(models[i]) > chatLikelihood(models[j])
	})
	return models
}

// nonChatMarkers are substrings of model names that cannot hold a chat
// conversation. They are only used for ordering, never to hide an entry: a
// gateway may name its chat model something unusual, and the user must still
// be able to pick it.
var nonChatMarkers = []string{
	"embed", "whisper", "tts", "dall-e", "moderation", "transcribe",
	"rerank", "sora", "audio", "image", "babbage", "davinci", "vision-preview",
}

func chatLikelihood(name string) int {
	lower := strings.ToLower(name)
	for _, marker := range nonChatMarkers {
		if strings.Contains(lower, marker) {
			return 0
		}
	}
	return 1
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// summarizeError keeps an error body short enough to display in the panel; a
// gateway that answers with an HTML page must not paste it into the UI.
func summarizeError(raw []byte) string {
	text := strings.TrimSpace(string(raw))
	if text == "" {
		return "(无内容)"
	}
	var envelope struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(raw, &envelope); err == nil {
		if msg := firstNonEmpty(envelope.Error.Message, envelope.Message); msg != "" {
			text = msg
		}
	}
	return truncate(strings.Join(strings.Fields(text), " "), 200)
}
