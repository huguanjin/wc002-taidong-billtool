package billing

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/xuri/excelize/v2"
)

// GenerateResult 一次出账运行的完整结果：文件路径 + 摘要数据。
type GenerateResult struct {
	BillPath      string
	SanitizedPath string // 为空表示未生成
	Summary       Summary
}

// GenerateBill 对应 log_to_bill.py 的 main()：读日志→提取缓存→聚合定价→写账单模板→（可选）写脱敏日志。
func GenerateBill(inputPath, templatePath, priceTablePath, outputDir string, params Params) (*GenerateResult, error) {
	book, err := LoadPriceBook(priceTablePath)
	if err != nil {
		return nil, fmt.Errorf("加载报价表失败: %w", err)
	}

	headers, rows, err := LoadLogRows(inputPath, params.Sheet, params.Encoding)
	if err != nil {
		return nil, fmt.Errorf("读取日志失败: %w", err)
	}

	stem := strings.TrimSuffix(filepath.Base(inputPath), filepath.Ext(inputPath))
	billPath := filepath.Join(outputDir, defaultOutputName(stem)+".xlsx")

	var sanitizedWriter *ExcelSanitizedWriter
	var sanitizedPath string
	if params.SanitizedLog {
		sanitizedPath = filepath.Join(outputDir, defaultSanitizedName(stem)+".xlsx")
		sanitizedWriter, err = NewExcelSanitizedWriter(sanitizedPath, headers)
		if err != nil {
			return nil, fmt.Errorf("初始化脱敏日志写出失败: %w", err)
		}
	}

	exchangeRate := params.ExchangeRate
	if exchangeRate <= 0 {
		exchangeRate = DefaultExchangeRate
	}

	var writerIface SanitizedRowWriter
	if sanitizedWriter != nil {
		writerIface = sanitizedWriter
	}

	agg, aggErr := AggregateFromRows(rows, headers, book, exchangeRate, params.PreferPriceTable, writerIface)
	if sanitizedWriter != nil {
		if closeErr := sanitizedWriter.Close(); closeErr != nil && aggErr == nil {
			return nil, fmt.Errorf("写出脱敏日志失败: %w", closeErr)
		}
	}
	if aggErr != nil {
		return nil, aggErr
	}

	month := params.Month
	if month == 0 {
		if m, ok := monthFromFilename(stem); ok {
			month = m
		} else {
			month = agg.Month
		}
	}
	if month < 1 || month > 12 {
		return nil, fmt.Errorf("无效月份: %d", month)
	}
	year := params.Year
	if year == 0 {
		year = agg.Year
	}

	missingPrices, err := WriteBillFromTemplate(templatePath, billPath, agg.Rows, year, month, book, params.Discount, exchangeRate, params.PreferPriceTable)
	if err != nil {
		return nil, fmt.Errorf("写出账单失败: %w", err)
	}

	if params.KeepLog && !IsDelimitedText(inputPath) {
		if err := attachLogSheet(billPath, headers, rows); err != nil {
			return nil, fmt.Errorf("附带原日志失败: %w", err)
		}
	}

	summary := buildSummary(agg, book, exchangeRate, params.PreferPriceTable, missingPrices)

	return &GenerateResult{BillPath: billPath, SanitizedPath: sanitizedPath, Summary: summary}, nil
}

func buildSummary(agg *AggregateResult, book *PriceBook, exchangeRate float64, preferPriceTable bool, missingPrices []string) Summary {
	rowSummaries := make([]RowSummary, 0, len(agg.Rows))
	settleTotal, listTotal := 0.0, 0.0
	groupDiscounts := ComputeGroupDiscounts(agg.Rows, book, exchangeRate, nil, preferPriceTable)

	for _, a := range agg.Rows {
		price, _ := ResolvePrice(a.Model, book, preferPriceTable, exchangeRate)
		settle := SettleCNY(a)
		list := 0.0
		hasPrice := price != nil
		if hasPrice {
			list = OfficialListCNY(a, exchangeRate)
		}
		settleTotal += settle
		listTotal += list
		rowSummaries = append(rowSummaries, RowSummary{
			Model: a.Model, Group: a.Group,
			Uncached: a.Uncached, CacheRead: a.CacheRead, Output: a.Output,
			CacheWrite5m: a.CacheWrite5m, CacheWrite1h: a.CacheWrite1h, Quota: a.Quota,
			SettleCNY: settle, ListCNY: list, Discount: groupDiscounts[a.Group],
			Rows: a.Rows, HasPrice: hasPrice,
		})
	}

	overallDisc := 0.0
	if listTotal > 0 {
		overallDisc = settleTotal / listTotal
	}

	return Summary{
		Year: agg.Year, Month: agg.Month, Rows: rowSummaries,
		SettleCNYTotal: settleTotal, ListCNYTotal: listTotal, OverallDiscount: overallDisc,
		MissingPriceModels: missingPrices, RowCount: agg.RowCount,
		CacheHitRows: agg.CacheHitRows, WebSearchRows: agg.WebSearchRows,
	}
}

var monthFilenameRe = regexp.MustCompile(`(\d{1,2})\s*月份?`)

func monthFromFilename(stem string) (int, bool) {
	m := monthFilenameRe.FindStringSubmatch(stem)
	if m == nil {
		return 0, false
	}
	var month int
	if _, err := fmt.Sscanf(m[1], "%d", &month); err != nil {
		return 0, false
	}
	if month < 1 || month > 12 {
		return 0, false
	}
	return month, true
}

func defaultOutputName(stem string) string {
	if strings.Contains(stem, "日志查询") {
		return strings.Replace(stem, "日志查询", "账单", 1)
	}
	if strings.Contains(stem, "日志") {
		return strings.Replace(stem, "日志", "账单", 1)
	}
	return stem + "_账单"
}

func defaultSanitizedName(stem string) string {
	if strings.Contains(stem, "日志查询") {
		return strings.Replace(stem, "日志查询", "脱敏日志", 1)
	}
	if strings.Contains(stem, "日志") {
		return strings.Replace(stem, "日志", "脱敏日志", 1)
	}
	return stem + "_脱敏日志"
}

// attachLogSheet 把原始日志作为「日志查询」工作表附加到账单文件末尾。
func attachLogSheet(billPath string, headers []string, rows [][]string) error {
	f, err := excelize.OpenFile(billPath)
	if err != nil {
		return err
	}
	defer f.Close()

	const sheetName = "日志查询"
	if idx, _ := f.GetSheetIndex(sheetName); idx != -1 {
		if err := f.DeleteSheet(sheetName); err != nil {
			return err
		}
	}
	if _, err := f.NewSheet(sheetName); err != nil {
		return err
	}

	headerRow := make([]interface{}, len(headers))
	for i, h := range headers {
		headerRow[i] = h
	}
	if err := f.SetSheetRow(sheetName, "A1", &headerRow); err != nil {
		return err
	}
	for i, row := range rows {
		values := make([]interface{}, len(row))
		for j, v := range row {
			values[j] = cellValueForSanitized(v)
		}
		axis, _ := excelize.CoordinatesToCellName(1, i+2)
		if err := f.SetSheetRow(sheetName, axis, &values); err != nil {
			return err
		}
	}

	return f.SaveAs(billPath)
}
