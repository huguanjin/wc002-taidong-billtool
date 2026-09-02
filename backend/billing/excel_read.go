package billing

import (
	"encoding/csv"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/xuri/excelize/v2"
	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/transform"
)

// IsDelimitedText 判断日志文件是否为纯文本分隔格式（csv/tsv），而非 xlsx。
func IsDelimitedText(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	return ext == ".csv" || ext == ".tsv"
}

// detectDelimiter 优先按扩展名判断（.tsv 用 Tab），.csv 时再按首行 Tab/逗号出现次数兜底，
// 兼容部分导出工具把 TSV 内容存成 .csv 后缀的情况。
func detectDelimiter(path, firstLine string) rune {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".tsv":
		return '\t'
	default:
		if strings.Count(firstLine, "\t") > strings.Count(firstLine, ",") {
			return '\t'
		}
		return ','
	}
}

func decodeBytes(raw []byte, enc string) (string, error) {
	switch enc {
	case "utf-8-sig":
		b := raw
		if len(b) >= 3 && b[0] == 0xEF && b[1] == 0xBB && b[2] == 0xBF {
			b = b[3:]
		}
		if !utf8.Valid(b) {
			return "", fmt.Errorf("invalid utf-8")
		}
		return string(b), nil
	case "utf-8":
		if !utf8.Valid(raw) {
			return "", fmt.Errorf("invalid utf-8")
		}
		return string(raw), nil
	case "gbk":
		out, _, err := transform.Bytes(simplifiedchinese.GBK.NewDecoder(), raw)
		if err != nil {
			return "", err
		}
		return string(out), nil
	case "gb18030":
		out, _, err := transform.Bytes(simplifiedchinese.GB18030.NewDecoder(), raw)
		if err != nil {
			return "", err
		}
		return string(out), nil
	default:
		return "", fmt.Errorf("unsupported encoding: %s", enc)
	}
}

// DetectCSVEncoding 对应 detect_csv_encoding：按候选编码顺序尝试，
// 首行含 model_name+quota 或含逗号即认为命中。
func DetectCSVEncoding(path, preferred string) (encoding string, content string, err error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", "", err
	}
	candidates := []string{}
	if preferred != "" {
		candidates = append(candidates, preferred)
	}
	candidates = append(candidates, "utf-8-sig", "utf-8", "gbk", "gb18030")

	for _, enc := range candidates {
		text, decErr := decodeBytes(raw, enc)
		if decErr != nil {
			continue
		}
		firstLine := text
		if idx := strings.IndexAny(text, "\r\n"); idx >= 0 {
			firstLine = text[:idx]
		}
		if strings.Contains(firstLine, "model_name") && strings.Contains(firstLine, "quota") {
			return enc, text, nil
		}
		if strings.Contains(firstLine, ",") || strings.Contains(firstLine, "\t") {
			return enc, text, nil
		}
	}
	return "", "", fmt.Errorf("无法识别 CSV/TSV 编码，可手动指定 encoding。已尝试: %v", candidates)
}

// LoadLogRows 读取日志 xlsx/csv/tsv，返回表头与数据行（不含表头）。
func LoadLogRows(path, sheetName, encoding string) (headers []string, rows [][]string, err error) {
	if IsDelimitedText(path) {
		_, text, err := DetectCSVEncoding(path, encoding)
		if err != nil {
			return nil, nil, err
		}
		firstLine := text
		if idx := strings.IndexAny(text, "\r\n"); idx >= 0 {
			firstLine = text[:idx]
		}
		reader := csv.NewReader(strings.NewReader(text))
		reader.Comma = detectDelimiter(path, firstLine)
		reader.FieldsPerRecord = -1
		// other 列常带原始 JSON，其中的引号不是 CSV/TSV 转义引号，需宽松解析。
		reader.LazyQuotes = true
		all, err := reader.ReadAll()
		if err != nil {
			return nil, nil, err
		}
		if len(all) == 0 {
			return nil, nil, fmt.Errorf("CSV 为空")
		}
		headers = make([]string, len(all[0]))
		for i, h := range all[0] {
			headers[i] = strings.TrimSpace(h)
		}
		return headers, all[1:], nil
	}

	f, err := excelize.OpenFile(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()

	names := f.GetSheetList()
	chosen := ""
	if sheetName != "" {
		for _, n := range names {
			if n == sheetName {
				chosen = n
				break
			}
		}
		if chosen == "" {
			return nil, nil, fmt.Errorf("工作表不存在: %s", sheetName)
		}
	} else {
		for _, n := range names {
			if n == "日志查询" {
				chosen = n
				break
			}
		}
		if chosen == "" && len(names) > 0 {
			chosen = names[0]
		}
	}
	if chosen == "" {
		return nil, nil, fmt.Errorf("日志表为空")
	}

	allRows, err := f.GetRows(chosen)
	if err != nil {
		return nil, nil, err
	}
	if len(allRows) == 0 {
		return nil, nil, fmt.Errorf("日志表为空")
	}
	headers = make([]string, len(allRows[0]))
	for i, h := range allRows[0] {
		headers[i] = strings.TrimSpace(h)
	}
	return headers, allRows[1:], nil
}

func cellAt(row []string, idx int) string {
	if idx >= 0 && idx < len(row) {
		return row[idx]
	}
	return ""
}

// LoadPriceBook 从报价表 xlsx 加载模型单价与分组折扣（第一个 sheet 单价，第二个 sheet 折扣）。
func LoadPriceBook(path string) (*PriceBook, error) {
	book := NewPriceBook()
	if path == "" {
		return book, nil
	}
	if _, err := os.Stat(path); err != nil {
		return book, nil
	}

	f, err := excelize.OpenFile(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	sheets := f.GetSheetList()
	if len(sheets) == 0 {
		return book, nil
	}

	rows, err := f.GetRows(sheets[0])
	if err != nil {
		return nil, err
	}
	lastCategory, lastChannel := "", ""
	for i, row := range rows {
		if i < 7 { // 对应 openpyxl min_row=8（1-indexed）
			continue
		}
		category := strings.TrimSpace(cellAt(row, 1))
		channel := strings.TrimSpace(cellAt(row, 2))
		model := strings.TrimSpace(cellAt(row, 3))
		inp := strings.TrimSpace(cellAt(row, 4))
		out := strings.TrimSpace(cellAt(row, 5))
		if category != "" {
			lastCategory = category
		}
		if channel != "" {
			lastChannel = channel
		}
		if model == "" || inp == "" || out == "" {
			continue
		}
		inpF, err1 := strconv.ParseFloat(inp, 64)
		outF, err2 := strconv.ParseFloat(out, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		currency := "USD"
		if lastCategory == "国产模型" {
			currency = "CNY"
		}
		book.ByModel[model] = ModelPrice{
			InputPerM: inpF, OutputPerM: outF, Currency: currency,
			Source: "price_table", Category: lastCategory, Channel: lastChannel,
		}
	}

	if len(sheets) > 1 {
		rows2, err2 := f.GetRows(sheets[1])
		if err2 == nil {
			for i, row := range rows2 {
				if i < 7 {
					continue
				}
				category := strings.TrimSpace(cellAt(row, 4))
				if category == "" {
					continue
				}
				if d, ok := ParseDiscountText(cellAt(row, 6)); ok {
					book.Discounts[category] = d
				}
			}
		}
	}

	return book, nil
}
