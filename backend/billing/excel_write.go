package billing

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/xuri/excelize/v2"
)

// ExcelSanitizedWriter 流式写出脱敏日志：展开缓存列，删除 other 列。
// 单个 sheet 写满 xlsx 行数上限（ExcelMaxRowsPerSheet）时自动切到下一个 sheet，避免超大日志写出失败。
type ExcelSanitizedWriter struct {
	headers          []string
	sanitizedHeaders []string
	file             *excelize.File
	sheetBase        string
	sheetIndex       int
	stream           *excelize.StreamWriter
	rowNum           int // 当前 sheet 内已写入的行数（含表头）
	totalRows        int // 全部 sheet 累计写入的数据行数
	path             string
}

func NewExcelSanitizedWriter(path string, headers []string) (*ExcelSanitizedWriter, error) {
	sanitizedHeaders := buildSanitizedHeaders(headers)
	f := excelize.NewFile()
	sheetBase := "日志查询"
	if err := f.SetSheetName(f.GetSheetName(0), sheetBase); err != nil {
		return nil, err
	}
	w := &ExcelSanitizedWriter{
		headers: headers, sanitizedHeaders: sanitizedHeaders,
		file: f, sheetBase: sheetBase, path: path,
	}
	if err := w.startSheet(sheetBase); err != nil {
		return nil, err
	}
	return w, nil
}

// startSheet 在给定 sheet 上新建 StreamWriter 并写入表头，重置行计数。
func (w *ExcelSanitizedWriter) startSheet(name string) error {
	sw, err := w.file.NewStreamWriter(name)
	if err != nil {
		return err
	}
	headerRow := make([]interface{}, len(w.sanitizedHeaders))
	for i, h := range w.sanitizedHeaders {
		headerRow[i] = h
	}
	if err := sw.SetRow("A1", headerRow); err != nil {
		return err
	}
	w.sheetIndex++
	w.stream = sw
	w.rowNum = 1
	return nil
}

func buildSanitizedHeaders(headers []string) []string {
	base := make([]string, 0, len(headers))
	for _, h := range headers {
		if h != "" && !SanitizedDropColumns[h] {
			base = append(base, h)
		}
	}
	return append(base, SanitizedCacheColumns...)
}

// WriteRow 实现 SanitizedRowWriter。
func (w *ExcelSanitizedWriter) WriteRow(row []string, cacheRead, cacheWrite5m, cacheWrite1h float64) error {
	if w.rowNum >= ExcelMaxRowsPerSheet {
		if err := w.stream.Flush(); err != nil {
			return err
		}
		nextSheet := fmt.Sprintf("%s_%d", w.sheetBase, w.sheetIndex+1)
		if _, err := w.file.NewSheet(nextSheet); err != nil {
			return err
		}
		if err := w.startSheet(nextSheet); err != nil {
			return err
		}
	}

	values := make([]interface{}, 0, len(w.sanitizedHeaders))
	for i, name := range w.headers {
		if name == "" || SanitizedDropColumns[name] {
			continue
		}
		values = append(values, cellValueForSanitized(cellAt(row, i)))
	}
	values = append(values, cacheRead, cacheWrite5m+cacheWrite1h, cacheWrite5m, cacheWrite1h)
	w.rowNum++
	w.totalRows++
	axis, err := excelize.CoordinatesToCellName(1, w.rowNum)
	if err != nil {
		return err
	}
	return w.stream.SetRow(axis, values)
}

// cellValueForSanitized 尽量把数字字符串写回数值类型，避免脱敏日志里数字变成文本。
func cellValueForSanitized(v string) interface{} {
	if v == "" {
		return nil
	}
	if f, err := strconv.ParseFloat(v, 64); err == nil {
		return f
	}
	return v
}

func (w *ExcelSanitizedWriter) RowsWritten() int { return w.totalRows }

func (w *ExcelSanitizedWriter) Close() error {
	if err := w.stream.Flush(); err != nil {
		return err
	}
	return w.file.SaveAs(w.path)
}

func strPtr(s string) *string { return &s }

// WriteBillFromTemplate 按账单模板列写出账单，返回缺少定价的 (model/group) 列表。
func WriteBillFromTemplate(templatePath, outputPath string, rows []*AggRow, year, month int, book *PriceBook, discount *float64, exchangeRate float64, preferPriceTable bool) ([]string, error) {
	if _, err := os.Stat(templatePath); err != nil {
		return nil, fmt.Errorf("账单模板不存在: %s", templatePath)
	}
	f, err := excelize.OpenFile(templatePath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	sheet := f.GetSheetName(0)
	existingRows, err := f.GetRows(sheet)
	if err != nil {
		return nil, err
	}
	maxRow := len(existingRows)
	if maxRow > 2 {
		for i := 0; i < maxRow-2; i++ {
			if err := f.RemoveRow(sheet, 3); err != nil {
				return nil, err
			}
		}
	}

	styleAccounting, err := f.NewStyle(&excelize.Style{CustomNumFmt: strPtr(AccountingFmt)})
	if err != nil {
		return nil, err
	}
	styleMoney, err := f.NewStyle(&excelize.Style{CustomNumFmt: strPtr(MoneyCNYFmt)})
	if err != nil {
		return nil, err
	}
	styleDiscount, err := f.NewStyle(&excelize.Style{CustomNumFmt: strPtr(DiscountFmt)})
	if err != nil {
		return nil, err
	}
	styleMonth, err := f.NewStyle(&excelize.Style{CustomNumFmt: strPtr(MonthFmt)})
	if err != nil {
		return nil, err
	}
	styleMoneyBold, err := f.NewStyle(&excelize.Style{CustomNumFmt: strPtr(MoneyCNYFmt), Font: &excelize.Font{Bold: true}})
	if err != nil {
		return nil, err
	}
	styleDiscountBold, err := f.NewStyle(&excelize.Style{CustomNumFmt: strPtr(DiscountFmt), Font: &excelize.Font{Bold: true}})
	if err != nil {
		return nil, err
	}
	styleBold, err := f.NewStyle(&excelize.Style{Font: &excelize.Font{Bold: true}})
	if err != nil {
		return nil, err
	}

	firstDataRow := 3
	groupDiscounts := ComputeGroupDiscounts(rows, book, exchangeRate, discount, preferPriceTable)
	periodDate := time.Date(year, time.Month(month), 1, 0, 0, 0, 0, time.UTC)

	axisOf := func(col, r int) string {
		axis, _ := excelize.CoordinatesToCellName(col, r)
		return axis
	}
	setNum := func(col, r int, v float64, style int) {
		axis := axisOf(col, r)
		f.SetCellValue(sheet, axis, v)
		f.SetCellStyle(sheet, axis, axis, style)
	}
	setFormula := func(col, r int, formula string, style int) {
		axis := axisOf(col, r)
		f.SetCellFormula(sheet, axis, formula)
		f.SetCellStyle(sheet, axis, axis, style)
	}
	setStr := func(col, r int, v string) {
		f.SetCellValue(sheet, axisOf(col, r), v)
	}

	var missingPrices []string
	for offset, agg := range rows {
		r := firstDataRow + offset
		price, note := ResolvePrice(agg.Model, book, preferPriceTable, exchangeRate)

		axisPeriod := axisOf(1, r)
		f.SetCellValue(sheet, axisPeriod, periodDate)
		f.SetCellStyle(sheet, axisPeriod, axisPeriod, styleMonth)
		setStr(2, r, agg.Model)
		setStr(3, r, agg.Group)

		setNum(4, r, agg.Uncached, styleAccounting)
		setNum(6, r, agg.CacheRead, styleAccounting)
		setNum(8, r, agg.Output, styleAccounting)
		setNum(10, r, agg.CacheWrite5m, styleAccounting)
		setNum(12, r, agg.CacheWrite1h, styleAccounting)

		setFormula(14, r, fmt.Sprintf("D%d+F%d+H%d+J%d+L%d", r, r, r, r, r), styleAccounting)
		setNum(15, r, agg.WebSearchCalls, styleAccounting)

		axis17 := axisOf(17, r)
		if agg.ImagePerCallCount != 0 {
			f.SetCellValue(sheet, axis17, agg.ImagePerCallCount)
		}
		f.SetCellStyle(sheet, axis17, axis17, styleAccounting)

		disc := groupDiscounts[agg.Group]
		axisDisc := axisOf(20, r)
		f.SetCellValue(sheet, axisDisc, disc)
		f.SetCellStyle(sheet, axisDisc, axisDisc, styleDiscount)

		setNum(22, r, SettleCNY(agg), styleMoney)

		// 阶梯表达式模型：刊例由表达式逐条累加得出，档位可能混用
		// （同一行的请求分处 base / tier_2），此时用「单价×总量」的公式
		// 反而算不准，所以总金额直接落数值，只有折扣折算仍用公式。
		hasExpr := agg.BillingMode == BillingModeTieredExpr && agg.BillingExpr != ""
		_, isTiered := TieredModelPrices[agg.Model]
		useTokenFormula := price != nil && price.Source != "per_call" && !isTiered && !hasExpr

		if useTokenFormula {
			listExpr := fmt.Sprintf("((D%d*E%d+F%d*G%d+H%d*I%d+J%d*K%d+L%d*M%d)/1000000+O%d*10/1000)", r, r, r, r, r, r, r, r, r, r, r)
			setFormula(19, r, fmt.Sprintf("%s*%s", listExpr, formatFloat(exchangeRate)), styleMoney)
			setFormula(23, r, fmt.Sprintf("%s*T%d", listExpr, r), styleMoney)
		} else {
			listUSD := agg.OfficialUSD
			setNum(19, r, listUSD*exchangeRate, styleMoney)
			setFormula(23, r, fmt.Sprintf("%s*T%d", formatFloat(listUSD), r), styleMoney)
		}

		switch {
		case !HasKnownListPrice(agg):
			missingPrices = append(missingPrices, agg.Model+"/"+agg.Group)
			setStr(24, r, "否")
		case price != nil && price.Source == "per_call":
			setStr(24, r, "否")
		default:
			display := ModelPrice{}
			var readP, w5P, w1P float64
			consistent := false
			switch {
			case hasExpr:
				// 单价列取自表达式在当前上下文长度下的等效系数，
				// 表达式含图片/音频等附加项时无法折算成单价，留空。
				rates, err := ExtractExprRates(agg.BillingExpr, time.Now(), agg.Uncached+agg.CacheRead+agg.CacheWrite5m+agg.CacheWrite1h)
				if err == nil {
					display = ModelPrice{InputPerM: rates.InputPerM, OutputPerM: rates.OutputPerM, Currency: "USD", Source: "expr"}
					readP, w5P, w1P = rates.CacheReadPerM, rates.CacheWritePerM, rates.CacheWrite1hPerM
					consistent = rates.Pure
				}
			case isTiered:
				tier := TieredModelPrices[agg.Model]
				display = ModelPrice{InputPerM: tier.Low[0], OutputPerM: tier.Low[1], Currency: "USD", Source: "tiered_low_display"}
				readP = tier.Low[2]
				w5P = display.InputPerM * CacheWrite5mMult
				w1P = display.InputPerM * CacheWrite1hMult
			default:
				display = *price
				readP, w5P, w1P = CacheUnitPrices(agg.Model, display.InputPerM)
				consistent = strings.HasPrefix(price.Source, "anthropic_official") ||
					strings.HasPrefix(price.Source, "gemini_official") ||
					strings.HasPrefix(price.Source, "kimi_official") ||
					strings.HasPrefix(price.Source, "image_official") ||
					(strings.HasPrefix(price.Source, "price_table") && note == "")
			}
			f.SetCellValue(sheet, axisOf(5, r), display.InputPerM)
			f.SetCellValue(sheet, axisOf(7, r), readP)
			f.SetCellValue(sheet, axisOf(9, r), display.OutputPerM)
			f.SetCellValue(sheet, axisOf(11, r), w5P)
			f.SetCellValue(sheet, axisOf(13, r), w1P)

			if agg.WebSearchCalls != 0 || (display.InputPerM == 0 && display.OutputPerM == 0) {
				consistent = false
			}
			if consistent {
				setStr(24, r, "是")
			} else {
				setStr(24, r, "否")
			}
		}
	}

	lastDataRow := firstDataRow + len(rows) - 1
	totalRow := lastDataRow + 1
	axisA := axisOf(1, totalRow)
	f.SetCellValue(sheet, axisA, "合计")
	f.SetCellStyle(sheet, axisA, axisA, styleBold)

	setTotalFormula := func(col int, style int) {
		colLetter, _ := excelize.ColumnNumberToName(col)
		formula := fmt.Sprintf("SUM(%s%d:%s%d)", colLetter, firstDataRow, colLetter, lastDataRow)
		axis := axisOf(col, totalRow)
		f.SetCellFormula(sheet, axis, formula)
		f.SetCellStyle(sheet, axis, axis, style)
	}
	setTotalFormula(19, styleMoneyBold)
	setTotalFormula(22, styleMoneyBold)
	setTotalFormula(23, styleMoneyBold)

	axisT := axisOf(20, totalRow)
	f.SetCellFormula(sheet, axisT, fmt.Sprintf("IF(S%d=0,0,V%d/S%d)", totalRow, totalRow, totalRow))
	f.SetCellStyle(sheet, axisT, axisT, styleDiscountBold)

	if err := f.SaveAs(outputPath); err != nil {
		return nil, err
	}
	return missingPrices, nil
}

// formatFloat 生成不带科学计数法的十进制字符串，供拼接进 Excel 公式。
func formatFloat(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}
