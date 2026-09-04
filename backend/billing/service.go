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
// dbPriceCachePath 是「拉取最新数据库价格」写出的本地 JSON 文件路径，出账时只读此文件，不连接数据库。
func GenerateBill(inputPath, templatePath, priceTablePath, dbPriceCachePath, outputDir string, params Params) (*GenerateResult, error) {
	var book *PriceBook
	var err error
	switch params.PriceSource {
	case PriceSourceDB:
		book, _, err = LoadPriceBookFromDBCacheFile(dbPriceCachePath)
		if err != nil {
			return nil, err
		}
	default:
		book, err = LoadPriceBook(priceTablePath)
		if err != nil {
			return nil, fmt.Errorf("加载报价表失败: %w", err)
		}
	}
	// official 模式下内置官方价优先；price_table/db 模式下报价表/数据库价格优先。
	preferPriceTable := params.PriceSource == PriceSourcePriceTable || params.PriceSource == PriceSourceDB

	headers, rows, err := LoadLogRows(inputPath, params.Sheet, params.Encoding)
	if err != nil {
		return nil, fmt.Errorf("读取日志失败: %w", err)
	}

	stem := strings.TrimSuffix(filepath.Base(inputPath), filepath.Ext(inputPath))
	billPath := filepath.Join(outputDir, defaultOutputName(stem)+".xlsx")

	var sanitizedWriter SanitizedWriter
	var sanitizedPath string
	if params.SanitizedLog {
		ext, delimiter, isDelimited := sanitizedFormatInfo(params.SanitizedFormat)
		sanitizedPath = filepath.Join(outputDir, defaultSanitizedName(stem)+ext)
		if isDelimited {
			sanitizedWriter, err = NewCSVSanitizedWriter(sanitizedPath, headers, delimiter)
		} else {
			sanitizedWriter, err = NewExcelSanitizedWriter(sanitizedPath, headers)
		}
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

	agg, aggErr := AggregateFromRows(rows, headers, book, exchangeRate, preferPriceTable, writerIface)
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

	missingPrices, err := WriteBillFromTemplate(templatePath, billPath, agg.Rows, year, month, book, params.Discount, exchangeRate, preferPriceTable)
	if err != nil {
		return nil, fmt.Errorf("写出账单失败: %w", err)
	}

	if params.KeepLog && !IsDelimitedText(inputPath) {
		if err := attachLogSheet(billPath, headers, rows); err != nil {
			return nil, fmt.Errorf("附带原日志失败: %w", err)
		}
	}

	summary := buildSummary(agg, book, exchangeRate, preferPriceTable, missingPrices)

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

// sanitizedFormatInfo 把「脱敏日志格式」参数解析为输出扩展名/分隔符；
// csv、tsv 是纯文本格式，没有 xlsx 单 sheet 104 万行的上限，适合超大日志。
func sanitizedFormatInfo(format string) (ext string, delimiter rune, isDelimited bool) {
	switch strings.ToLower(format) {
	case "csv":
		return ".csv", ',', true
	case "tsv":
		return ".tsv", '\t', true
	default:
		return ".xlsx", 0, false
	}
}

// attachLogSheet 把原始日志作为「日志查询」工作表附加到账单文件末尾。
// 行数超过 ExcelMaxRowsPerSheet 时自动拆分到「日志查询_2」「日志查询_3」……多个 sheet。
func attachLogSheet(billPath string, headers []string, rows [][]string) error {
	f, err := excelize.OpenFile(billPath)
	if err != nil {
		return err
	}
	defer f.Close()

	const sheetBase = "日志查询"
	if idx, _ := f.GetSheetIndex(sheetBase); idx != -1 {
		if err := f.DeleteSheet(sheetBase); err != nil {
			return err
		}
	}

	headerRow := make([]interface{}, len(headers))
	for i, h := range headers {
		headerRow[i] = h
	}

	sheetName := sheetBase
	sheetIndex := 1
	if _, err := f.NewSheet(sheetName); err != nil {
		return err
	}
	if err := f.SetSheetRow(sheetName, "A1", &headerRow); err != nil {
		return err
	}

	rowInSheet := 1 // 已写入当前 sheet 的行数（含表头）
	for _, row := range rows {
		if rowInSheet >= ExcelMaxRowsPerSheet {
			sheetIndex++
			sheetName = fmt.Sprintf("%s_%d", sheetBase, sheetIndex)
			if _, err := f.NewSheet(sheetName); err != nil {
				return err
			}
			if err := f.SetSheetRow(sheetName, "A1", &headerRow); err != nil {
				return err
			}
			rowInSheet = 1
		}

		values := make([]interface{}, len(row))
		for j, v := range row {
			values[j] = cellValueForSanitized(v)
		}
		rowInSheet++
		axis, _ := excelize.CoordinatesToCellName(1, rowInSheet)
		if err := f.SetSheetRow(sheetName, axis, &values); err != nil {
			return err
		}
	}

	return f.SaveAs(billPath)
}
