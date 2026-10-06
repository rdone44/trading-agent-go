package marketdata

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/huijun/trading-agent-go/internal/model"
)

// yahooChartResponse is the subset of the Yahoo Finance chart API we need.
// It is decoded with the standard library only: no third-party provider SDK.
type yahooChartResponse struct {
	Chart struct {
		Result []struct {
			Timestamp  []int64 `json:"timestamp"`
			Indicators struct {
				Quote []struct {
					Open   []*float64 `json:"open"`
					High   []*float64 `json:"high"`
					Low    []*float64 `json:"low"`
					Close  []*float64 `json:"close"`
					Volume []*float64 `json:"volume"`
				} `json:"quote"`
			} `json:"indicators"`
		} `json:"result"`
		Error *struct {
			Code        string `json:"code"`
			Description string `json:"description"`
		} `json:"error"`
	} `json:"chart"`
}

// Yahoo loads daily bars from the public Yahoo Finance chart endpoint.
func Yahoo(symbol string, days int, end time.Time) (model.Series, error) {
	if days < 5 {
		days = 5
	}
	// Ask for extra calendar days so weekends and holidays still yield `days` bars.
	start := end.AddDate(0, 0, -int(float64(days)*1.6))
	endpoint := fmt.Sprintf(
		"https://query1.finance.yahoo.com/v8/finance/chart/%s?period1=%d&period2=%d&interval=1d",
		url.PathEscape(strings.ToUpper(symbol)),
		start.Unix(),
		end.AddDate(0, 0, 1).Unix(),
	)

	client := &http.Client{Timeout: 30 * time.Second}
	request, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return model.Series{}, fmt.Errorf("构造请求失败: %w", err)
	}
	request.Header.Set("User-Agent", "Mozilla/5.0 (compatible; trading-agent-go)")
	request.Header.Set("Accept", "application/json")

	response, err := client.Do(request)
	if err != nil {
		return model.Series{}, fmt.Errorf("从 Yahoo Finance 下载 %s 失败: %w", symbol, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return model.Series{}, fmt.Errorf("Yahoo Finance 返回 %s 的 HTTP 状态码 %d", symbol, response.StatusCode)
	}

	var payload yahooChartResponse
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		return model.Series{}, fmt.Errorf("解析 Yahoo 返回内容失败: %w", err)
	}
	if payload.Chart.Error != nil {
		return model.Series{}, fmt.Errorf("Yahoo Finance 返回 %s 的错误: %s", symbol, payload.Chart.Error.Description)
	}
	if len(payload.Chart.Result) == 0 || len(payload.Chart.Result[0].Indicators.Quote) == 0 {
		return model.Series{}, fmt.Errorf("Yahoo Finance 没有返回 %s 的任何数据", symbol)
	}

	result := payload.Chart.Result[0]
	quote := result.Indicators.Quote[0]
	bars := make([]model.Bar, 0, len(result.Timestamp))
	for i, ts := range result.Timestamp {
		open, high, low, close := at(quote.Open, i), at(quote.High, i), at(quote.Low, i), at(quote.Close, i)
		if model.IsNaN(open) || model.IsNaN(high) || model.IsNaN(low) || model.IsNaN(close) {
			continue
		}
		bars = append(bars, model.Bar{
			Time:   time.Unix(ts, 0).UTC(),
			Open:   open,
			High:   high,
			Low:    low,
			Close:  close,
			Volume: model.NaNOr(at(quote.Volume, i), 0),
		})
	}
	if len(bars) == 0 {
		return model.Series{}, fmt.Errorf("Yahoo Finance 返回的 %s 数据全是残缺K线", symbol)
	}
	sort.Slice(bars, func(i, j int) bool { return bars[i].Time.Before(bars[j].Time) })
	if len(bars) > days {
		bars = bars[len(bars)-days:]
	}
	return model.Series{Symbol: strings.ToUpper(symbol), Bars: bars, Source: "yahoo"}, nil
}

func at(values []*float64, i int) float64 {
	if i >= len(values) || values[i] == nil {
		return model.NaN()
	}
	return *values[i]
}
