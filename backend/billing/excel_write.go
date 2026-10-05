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
	includeBilling   bool
	file             *excelize.File
	sheetBase        string
	sheetIndex       int
	stream           *excelize.StreamWriter
	rowNum           int // 当前 sheet 内已写入的行数（含表头）
	totalRows        int // 全部 sheet 累计写入的数据行数
	path             string
}

func NewExcelSanitizedWriter(path string, headers []string, includeBilling bool) (*ExcelSanitizedWriter, error) {
	sanitizedHeaders := buildSanitizedHeaders(headers, includeBilling)
	f := excelize.NewFile()
	sheetBase := "日志查询"
	if err := f.SetSheetName(f.GetSheetName(0), sheetBase); err != nil {
		return nil, err
	}
	w := &ExcelSanitizedWriter{
		headers: headers, sanitizedHeaders: sanitizedHeaders, includeBilling: includeBilling,
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

func buildSanitizedHeaders(headers []string, includeBilling bool) []string {
	base := make([]string, 0, len(headers))
	for _, h := range headers {
		if h != "" && !SanitizedDropColumns[h] {
			base = append(base, h)
		}
	}
	return append(base, SanitizedColumns(includeBilling)...)
}

// WriteRow 实现 SanitizedRowWriter。
func (w *ExcelSanitizedWriter) WriteRow(row []string, cacheRead, cacheWrite5m, cacheWrite1h float64, details RowDetails) error {
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
	values = append(values, detailCells(details)...)
	if w.includeBilling {
		values = append(values, billingCells(details.Billing)...)
	}
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

// extraColumn 模板表在 AC 之后追加的一列，供成本利润表使用（账单不追加任何列）。
//
// 抽成数据而不是复制一份写出逻辑：成本利润表与账单要求前 29 列逐列同构，
// 一旦各写一份，两边必然随时间走偏（列宽、样式、公式引用的差异会悄悄累积）。
type extraColumn struct {
	Title string
	// write 写入第 rowIdx 行的该列。formula 非空时写公式，否则写 value。
	write func(rowIdx, r int, exchangeRate float64) (value interface{}, formula string)
	// sumInTotal 合计行是否对该列求 SUM。
	sumInTotal bool
	// money 该列是否套人民币金额格式。
	money bool
}

// WriteBillFromTemplate 按账单模板列写出账单，返回缺少定价的 (model/group) 列表。
func WriteBillFromTemplate(templatePath, outputPath string, rows []*AggRow, year, month int, book *PriceBook, discount *float64, exchangeRate float64, preferPriceTable bool, manualMarkers []string) ([]string, error) {
	return writeTemplateSheet(templatePath, outputPath, rows, nil, year, month, book,
		discount, exchangeRate, preferPriceTable, manualMarkers)
}

// writeTemplateSheet 是账单与成本利润表共用的写出核心：前 29 列语义完全一致，
// extras 为空即账单，非空则在 AC 之后追加各列（成本利润表用）。
func writeTemplateSheet(templatePath, outputPath string, rows []*AggRow, extras []extraColumn, year, month int, book *PriceBook, discount *float64, exchangeRate float64, preferPriceTable bool, manualMarkers []string) ([]string, error) {
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

	// 追加列的表头由代码写：模板文件里 AC 之后是空的，成本利润表要自己补表头。
	// 起始列固定为 AC 之后，与 buildLogExportColumns 的既有列布局对齐。
	const extraStartCol = 29 + 1 // AD
	for i, ex := range extras {
		if err := f.SetCellValue(sheet, mustAxis(extraStartCol+i, 1), ex.Title); err != nil {
			return nil, err
		}
	}

	firstDataRow := 3
	discountResult := ComputeGroupDiscounts(rows, book, exchangeRate, discount, preferPriceTable, manualMarkers)
	derivedDiscounts := discountResult.Derived
	underivable := discountResult.Underivable
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
	// setPrice 写单价列。单价列一律不套金额格式：它们的单位是「美金/百万 token」，
	// 不是人民币金额，套上 MoneyCNYFmt 会让 10 显示成 10.0000，与同类列口径不一。
	setPrice := func(col, r int, v float64) {
		f.SetCellValue(sheet, axisOf(col, r), v)
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
		// C 列：同分组下出现多种倍率时会被拆成多行，光看同名两行会困惑，
		// 所以把本次实际倍率缀在后面（Codex(0.4) / Codex(1.0)）。倍率唯一时保持原名。
		setStr(3, r, agg.DisplayGroup())

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

		// T 列折扣：公式引用它算结算额，所以写「结算系数」而不是显示值。
		// 按倍率结算的行它的精确值是 倍率/7（如 0.057142857），
		// 单元格套着 3 位小数格式，客户看到 0.057，而 S×T=V 仍然严格成立。
		disc := discountResult.SettleFactor(agg.Group)
		axisDisc := axisOf(20, r)
		f.SetCellValue(sheet, axisDisc, disc)
		f.SetCellStyle(sheet, axisDisc, axisDisc, styleDiscount)

		// 阶梯表达式模型：刊例由表达式逐条累加得出，同一行的请求可能分处不同档位
		// （如 base / tier_2）。跨档行没有单一单价能还原金额，此时不写「单价×用量」。
		hasExpr := agg.BillingMode == BillingModeTieredExpr && agg.BillingExpr != ""
		_, isTiered := TieredModelPrices[agg.Model]
		useTokenFormula := price != nil && price.Source != "per_call" && !isTiered && !hasExpr

		// 表达式行的等效单价只在「该行只命中一个档位、且单价加总恰好还原官方刊例」
		// 时才有意义，判定统一走 ExprRowReconcile，与 X 列判据同源。
		exprRates := ExprRates{}
		exprOK := false
		exprAt := agg.LastAt
		if exprAt.IsZero() {
			exprAt = time.Now()
		}
		if hasExpr {
			exprRates, exprOK = ExprRowReconcile(agg.BillingExpr, agg, exprAt, exchangeRate)
		}

		// AC 列（官方刊例-美金）：能由 E/G/I/K/M 还原的行（普通价表行，或阶梯单档行），
		// AC 直接是「单价×用量」公式，与单价列同源；不能还原的行（跨档、按次计费、
		// isTiered 兜底展示价等），AC 落官方刊例本身——这是全表唯一允许出现裸数值的
		// 单元格，且只出现在这一列，不再散落进 S/W 的公式字符串里。
		// S/W 一律引用 AC，不再各写一套分支：S = (AC+O*10/1000)*汇率，W = S÷汇率。
		// 价格列在系数为 0 时会被留空（见下方 hasExpr 分支），留空写入的是空字符串
		// 而不是数字 0；AC 公式若直接引用会在 Excel 里算出 #VALUE!（数字×文本）。
		// 用 N() 包一层：N(空文本)=0，N(数字)=原数字，两种情况都安全。
		reconcilable := useTokenFormula || exprOK
		if reconcilable {
			acExpr := fmt.Sprintf("(D%d*N(E%d)+F%d*N(G%d)+H%d*N(I%d)+J%d*N(K%d)+L%d*N(M%d))/1000000", r, r, r, r, r, r, r, r, r, r)
			setFormula(29, r, acExpr, styleMoney)
		} else {
			setNum(29, r, agg.OfficialUSD, styleMoney)
		}
		setFormula(19, r, fmt.Sprintf("(AC%d+O%d*10/1000)*%s", r, r, formatFloat(exchangeRate)), styleMoney)

		// 结算金额：口径统一为「总金额 × 折扣」，不再直接取日志 quota 折算的金额。
		// 两者的差额就是商务折扣与日志里 groupRatio 的差额，差异来源写在 AB 列。
		setFormula(22, r, fmt.Sprintf("S%d*T%d", r, r), styleMoney)
		setFormula(23, r, fmt.Sprintf("V%d/%s", r, formatFloat(exchangeRate)), styleMoney)

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
				// 单价列取自表达式在该行实际请求时刻、该上下文长度下的等效系数。
				// 时刻不能用 now()：峰谷倍率（hour()）依赖请求当时的时间点。
				// 表达式含图片/音频等附加项、或本行跨档时无法折算成单价，留空。
				// X 列不再是「表达式是否线性」，而是「单价×用量能否还原刊例」，
				// 判据与 S 列公式同源，见 ExprRowReconcile。
				consistent = exprOK
				if exprOK {
					display = ModelPrice{InputPerM: exprRates.InputPerM, OutputPerM: exprRates.OutputPerM, Currency: "USD", Source: "expr"}
					readP, w5P, w1P = exprRates.CacheReadPerM, exprRates.CacheWritePerM, exprRates.CacheWrite1hPerM
					// Y/Z/AA 三个「列表价」列按表达式系数回填：
					// Y 缓存未命中（输入）单价，同 E 列口径；
					// Z 缓存读单价，同 G 列口径（表达式未引用 cr 时为 0，留空）；
					// AA 输出单价。表达式含图片/音频附加项时单价不足以还原金额，三列都留空。
					setPrice(25, r, exprRates.InputPerM)
					if exprRates.CacheReadPerM != 0 {
						setPrice(26, r, exprRates.CacheReadPerM)
					}
					setPrice(27, r, exprRates.OutputPerM)
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

			// 跨档行（或表达式含附加项/常数项）单价列留空，交由下面的 AB 列说明原因。
			// 表达式未引用某档时，该档系数为 0——这里也必须留空而不是写 0：
			// 「单价 0」会被读成「这项免费」，而实际含义是「该档不参与本行计价」。
			if hasExpr && !exprOK {
				for _, col := range []int{5, 7, 9, 11, 13} {
					f.SetCellValue(sheet, axisOf(col, r), "")
				}
			}
			if exprOK {
				if exprRates.CacheReadPerM == 0 {
					f.SetCellValue(sheet, axisOf(7, r), "")
				}
				if exprRates.CacheWritePerM == 0 {
					f.SetCellValue(sheet, axisOf(11, r), "")
				}
				if exprRates.CacheWrite1hPerM == 0 {
					f.SetCellValue(sheet, axisOf(13, r), "")
				}
			}

			if agg.WebSearchCalls != 0 || (display.InputPerM == 0 && display.OutputPerM == 0) {
				consistent = false
			}
			if consistent {
				setStr(24, r, "是")
			} else {
				setStr(24, r, "否")
			}
		}

		// 阶梯计费模型在备注里留下价档说明：整月都落在同一档时只记档位名，
		// 跨档时写明是哪几档混合，并说明单价为什么不适用，便于客户核对
		// 刊例为什么不是「单价×总量」。
		var notes []string
		if hasExpr && len(agg.ExprTiers) > 0 {
			if len(agg.ExprTiers) == 1 {
				notes = append(notes, "阶梯计费，本行命中档位："+strings.Join(agg.ExprTiers, "、"))
			} else {
				notes = append(notes, fmt.Sprintf("阶梯计费，本行跨 %d 档混合计价：%s",
					len(agg.ExprTiers), strings.Join(agg.ExprTiers, "、")))
			}
		}
		if hasExpr && !exprOK {
			notes = append(notes, "本行跨档，单价不适用，请按总金额核对")
		}
		// 折扣来源必须可追：价表/合同里查到的折扣是商务谈定值，
		// 反推值只能保证账面对得上，不等于谈定的折扣。
		// 按本次请求倍率结算的行说明白，客户拿到表能自己复核这一行为什么金额刚好等于实收。
		if agg.HasRatioDiscount() {
			notes = appendNote(notes, fmt.Sprintf(
				"折扣按本次请求实际使用的分组倍率结算（倍率 %s ÷ %s = %s），金额与站内实收一致",
				trimRatio(agg.GroupRatio), trimRatio(DiscountBaseFactor),
				trimRatio(round(agg.RatioDiscount(), DiscountDecimals))))
		}
		if reason, bad := underivable[agg.Group]; bad {
			notes = appendNote(notes, reason+"；本行折扣取站点实际计费倍率，请人工确认合同折扣")
		}
		if derivedDiscounts[agg.Group] {
			notes = appendNote(notes, "折扣为反推值（价表无该分组折扣，按 Σ结算/Σ总金额倒算）")
		} else if discount != nil {
			notes = appendNote(notes, "折扣为手工指定值")
		} else if _, bad := underivable[agg.Group]; !bad && !agg.HasRatioDiscount() {
			notes = appendNote(notes, "折扣取自价表")
		}
		if len(notes) > 0 {
			setStr(28, r, strings.Join(notes, "；"))
		}

		// 追加列（成本利润表的渠道/上游折扣/上游成本）。写在这里是为了让它的值
		// 能引用同行的 AC，公式在 Excel 里可追溯而不是写死的数。
		for i, ex := range extras {
			col := extraStartCol + i
			value, formula := ex.write(offset, r, exchangeRate)
			axis := axisOf(col, r)
			switch {
			case formula != "":
				f.SetCellFormula(sheet, axis, formula)
				if ex.money {
					f.SetCellStyle(sheet, axis, axis, styleMoney)
				}
			case value == nil:
				// 形如「上游倍率没维护」：必须留空而不是写 0。
				// 写 0 会被读成「上游免费」，把毛利虚高。
				f.SetCellValue(sheet, axis, "")
			default:
				f.SetCellValue(sheet, axis, value)
				switch {
				case ex.money:
					f.SetCellStyle(sheet, axis, axis, styleMoney)
				case isDiscountColumn(ex):
					f.SetCellStyle(sheet, axis, axis, styleDiscount)
				}
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
	setTotalFormula(29, styleMoneyBold)
	for i, ex := range extras {
		if ex.sumInTotal {
			setTotalFormula(extraStartCol+i, styleMoneyBold)
		}
	}

	axisT := axisOf(20, totalRow)
	f.SetCellFormula(sheet, axisT, fmt.Sprintf("IF(S%d=0,0,V%d/S%d)", totalRow, totalRow, totalRow))
	f.SetCellStyle(sheet, axisT, axisT, styleDiscountBold)

	if err := f.SaveAs(outputPath); err != nil {
		return nil, err
	}
	return missingPrices, nil
}

// mustAxis 把 (列,行) 转成单元格坐标。列/行都是代码里算出来的常量或循环变量，
// 不会越界，出错时返回空串让 Excel 侧报错而不是掩盖。
func mustAxis(col, row int) string {
	axis, err := excelize.CoordinatesToCellName(col, row)
	if err != nil {
		return ""
	}
	return axis
}

// isDiscountColumn 判断追加列是否该套「折扣」格式（3 位小数）。
// 折扣与金额用不同格式：折扣是比值（0.057），金额是人民币（43.6205），
// 共用金额格式会把折扣显示成 0.0570，与 T 列的既有观感不一致。
func isDiscountColumn(ex extraColumn) bool {
	return !ex.money && !ex.sumInTotal
}

// appendNote 追加一条备注，空串忽略，避免在账单 AB 列留下孤立的「；」。
func appendNote(notes []string, note string) []string {
	if strings.TrimSpace(note) == "" {
		return notes
	}
	return append(notes, note)
}

// formatFloat 生成不带科学计数法的十进制字符串，供拼接进 Excel 公式。
func formatFloat(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}
