package marketdata

import (
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/huijun/trading-agent-go/internal/model"
)

// dateFormats covers the layouts exported by common charting tools.
var dateFormats = []string{
	"2006-01-02", "2006/01/02", "02-01-2006", "2006-01-02 15:04:05",
	time.RFC3339, "20060102",
}

// FromCSV loads bars from a CSV file with a date column plus open/high/low/close
// (volume optional). Column names are matched case-insensitively, so exports
// from most platforms work without editing.
func FromCSV(path, symbol string) (model.Series, error) {
	file, err := os.Open(path)
	if err != nil {
		return model.Series{}, fmt.Errorf("打开数据文件 %s 失败: %w", path, err)
	}
	defer file.Close()

	reader := csv.NewReader(file)
	reader.FieldsPerRecord = -1
	header, err := reader.Read()
	if err != nil {
		return model.Series{}, fmt.Errorf("读取 %s 的表头失败: %w", path, err)
	}
	columns := map[string]int{}
	for i, name := range header {
		columns[strings.ToLower(strings.TrimSpace(name))] = i
	}

	index := func(candidates ...string) int {
		for _, candidate := range candidates {
			if i, ok := columns[candidate]; ok {
				return i
			}
		}
		return -1
	}
	dateCol := index("date", "timestamp", "time", "datetime")
	openCol := index("open")
	highCol := index("high")
	lowCol := index("low")
	closeCol := index("close", "adj close", "adj_close")
	volumeCol := index("volume", "vol")
	if dateCol < 0 || openCol < 0 || highCol < 0 || lowCol < 0 || closeCol < 0 {
		return model.Series{}, fmt.Errorf(
			"%s 必须包含 date、open、high、low、close 列（实际列：%s）",
			path, strings.Join(header, "、"))
	}

	bars := []model.Bar{}
	line := 1
	for {
		record, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return model.Series{}, fmt.Errorf("读取 %s 第 %d 行失败: %w", path, line+1, err)
		}
		line++

		ts, err := parseDate(record[dateCol])
		if err != nil {
			return model.Series{}, fmt.Errorf("%s 第 %d 行: %w", path, line, err)
		}
		values := make([]float64, 5)
		for i, col := range []int{openCol, highCol, lowCol, closeCol} {
			value, err := parseFloat(record, col)
			if err != nil {
				return model.Series{}, fmt.Errorf("%s line %d: %w", path, line, err)
			}
			values[i] = value
		}
		volume := 0.0
		if volumeCol >= 0 {
			if value, err := parseFloat(record, volumeCol); err == nil {
				volume = value
			}
		}
		bars = append(bars, model.Bar{
			Time: ts, Open: values[0], High: values[1], Low: values[2], Close: values[3], Volume: volume,
		})
	}
	if len(bars) == 0 {
		return model.Series{}, fmt.Errorf("%s 中没有解析到任何K线", path)
	}
	if symbol == "" {
		symbol = strings.ToUpper(strings.TrimSuffix(strings.TrimSuffix(baseName(path), ".csv"), ".CSV"))
	}
	return model.Series{Symbol: strings.ToUpper(symbol), Bars: bars, Source: "csv"}, nil
}

func parseFloat(record []string, col int) (float64, error) {
	if col >= len(record) {
		return 0, fmt.Errorf("缺少第 %d 列", col)
	}
	text := strings.TrimSpace(record[col])
	if text == "" || text == "null" || text == "nan" {
		return model.NaN(), nil
	}
	value, err := strconv.ParseFloat(strings.ReplaceAll(text, ",", ""), 64)
	if err != nil {
		return 0, fmt.Errorf("无法把 %q 解析为数字", text)
	}
	return value, nil
}

func parseDate(text string) (time.Time, error) {
	text = strings.TrimSpace(text)
	for _, layout := range dateFormats {
		if ts, err := time.Parse(layout, text); err == nil {
			return ts, nil
		}
	}
	return time.Time{}, fmt.Errorf("无法把 %q 解析为日期", text)
}

func baseName(path string) string {
	if i := strings.LastIndexAny(path, `\/`); i >= 0 {
		return path[i+1:]
	}
	return path
}
