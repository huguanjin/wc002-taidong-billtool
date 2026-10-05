package billing

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
)

// 成本利润表在账单 29 列（A~AC）之后追加的列。
//
// 保持 A~AC 与账单逐列同构，是为了让成本利润表能与账单并排比对：
// 同一行的 S/T/V 是「对客户的结算」，AD~AG 是「上游成本」，AH 直接给出两者的差。
// 如果把渠道信息塞进 C 列，行与账单就对不上了。
const (
	CostColumnChannelID    = "AD"
	CostColumnChannelName  = "AE"
	CostColumnUpstreamOff  = "AF"
	CostColumnUpstreamCost = "AG"
	CostColumnProfit       = "AH"
)

// WriteCostFromTemplate 写出成本利润表：账单全部列 + 渠道 ID / 渠道名称 / 上游折扣 / 上游成本 / 利润。
//
// 渠道信息一律放追加列，不动 C 列的分组标识——那样会破坏与账单的行对应关系。
func WriteCostFromTemplate(templatePath, outputPath string, rows []*CostRow, year, month int,
	book *PriceBook, discount *float64, exchangeRate float64, preferPriceTable bool, manualMarkers []string) error {

	extras := []extraColumn{
		{
			Title: "渠道ID",
			write: func(rowIdx, r int, _ float64) (interface{}, string) {
				if rowIdx >= len(rows) {
					return nil, ""
				}
				return rows[rowIdx].ChannelID, ""
			},
		},
		{
			Title: "渠道名称",
			write: func(rowIdx, r int, _ float64) (interface{}, string) {
				if rowIdx >= len(rows) {
					return nil, ""
				}
				return rows[rowIdx].ChannelName, ""
			},
		},
		{
			Title: "上游折扣",
			write: func(rowIdx, r int, _ float64) (interface{}, string) {
				if rowIdx >= len(rows) {
					return nil, ""
				}
				d, ok := rows[rowIdx].UpstreamDiscount()
				if !ok {
					// 未维护倍率：留空。写 0 会被读成「上游免费」，把毛利虚高。
					return nil, ""
				}
				return d, ""
			},
		},
		{
			Title: "上游成本（人民币）",
			money: true,
			// 成本与结算同源：成本 = 官方刊例(人民币) × 上游折扣 = AC × AF × 汇率。
			// 写成公式而不是数值，财务/客户可在 Excel 里自行追溯到 AC 列。
			write: func(rowIdx, r int, exchangeRate float64) (interface{}, string) {
				if rowIdx >= len(rows) {
					return nil, ""
				}
				if _, ok := rows[rowIdx].UpstreamDiscount(); !ok {
					return nil, ""
				}
				return nil, fmt.Sprintf("AC%d*AF%d*%s", r, r, formatFloat(exchangeRate))
			},
			sumInTotal: true,
		},
		{
			Title: "利润（人民币）",
			money: true,
			// 利润 = 客户结算额 − 上游成本 = V − AG。
			// 未维护倍率时 AG 为空，这里必须跟着留空：Excel 把空当 0，
			// 直接写 V-AG 等于按「上游免费」算利润，毛利会虚高。
			// 用 IF 判空，SUM 求和时也会自动跳开这些行。
			write: func(rowIdx, r int, _ float64) (interface{}, string) {
				if rowIdx >= len(rows) {
					return nil, ""
				}
				if _, ok := rows[rowIdx].UpstreamDiscount(); !ok {
					return nil, ""
				}
				return nil, fmt.Sprintf(`IF(AG%d="","",V%d-AG%d)`, r, r, r)
			},
			sumInTotal: true,
		},
	}

	// 成本利润表要按渠道的行序写出，而共享核心接收的是 []*AggRow。
	// 这里把 CostRow 的内嵌 AggRow 摘出来，顺序保持一致——
	// 追加列的 write 用 rowIdx 回查原始 CostRow，所以两者必须同序。
	aggs := make([]*AggRow, len(rows))
	for i, c := range rows {
		aggs[i] = c.AggRow
	}

	// 未维护倍率的渠道在备注里说明，且其成本不参与合计（公式里 AF 为空，SUM 自动跳过）。
	if _, err := writeTemplateSheet(templatePath, outputPath, aggs, extras, year, month,
		book, discount, exchangeRate, preferPriceTable, manualMarkers); err != nil {
		return err
	}
	return nil
}

// SummarizeCost 汇总成本利润表的合计，并生成一段可直接复制的说明文字。
//
// 口径与表内公式严格一致：结算额 = Σ(官方刊例人民币 × 结算系数)（成本利润表 V 列），
// 成本 = Σ(官方刊例人民币 × 上游折扣)（AG 列），利润 = 结算额 − 成本（AH 列）。
// 未维护倍率的行两边都跳过：成本不计入，结算额也不计入——否则会出现
// 「利润 = 全量结算 − 部分成本」这种把毛利算虚高的组合。
func SummarizeCost(rows []*CostRow, book *PriceBook, discount *float64, exchangeRate float64,
	preferPriceTable bool, manualMarkers []string, year, month int) (CostTotals, string) {

	// 结算系数按 Group 取，与写出时同一套 ComputeGroupDiscounts。
	discountResult := ComputeGroupDiscounts(aggRowsOf(rows), book, exchangeRate, discount, preferPriceTable, manualMarkers)

	var totals CostTotals
	channelSeen := map[int]bool{}
	for _, cr := range rows {
		totals.TotalRows++
		if cr.ChannelID != 0 {
			channelSeen[cr.ChannelID] = true
		}
		cost, ok := cr.UpstreamCostCNY(exchangeRate)
		if !ok {
			// 未维护倍率：这一行不参与任何合计（见上面的口径说明）。
			continue
		}
		list := OfficialListCNY(cr.AggRow, exchangeRate)
		totals.SettleCNY += round(list*discountResult.SettleFactor(cr.Group), MoneyDecimals)
		totals.CostCNY += cost
		totals.PricedRows++
	}
	totals.CostCNY = round(totals.CostCNY, MoneyDecimals)
	totals.SettleCNY = round(totals.SettleCNY, MoneyDecimals)
	totals.ProfitCNY = round(totals.SettleCNY-totals.CostCNY, MoneyDecimals)
	totals.ChannelCount = len(channelSeen)

	return totals, FormatCostSummary(totals, year, month)
}

// FormatCostSummary 生成结果区那段可复制的文字。
//
// 分开成一个纯函数是为了能用测试固定住文案：它是给人复制到聊天/邮件里的，
// 数字口径改了却忘了改这里，会直接导致对外报错数。
func FormatCostSummary(t CostTotals, year, month int) string {
	period := fmt.Sprintf("%d-%02d", year, month)
	margin := 0.0
	if t.SettleCNY > 0 {
		margin = t.ProfitCNY / t.SettleCNY * 100
	}

	var b strings.Builder
	fmt.Fprintf(&b, "账期：%s\n", period)
	fmt.Fprintf(&b, "结算金额：¥%s\n", trimMoney(t.SettleCNY))
	fmt.Fprintf(&b, "上游成本：¥%s\n", trimMoney(t.CostCNY))
	fmt.Fprintf(&b, "利润：¥%s（毛利率 %s%%）\n", trimMoney(t.ProfitCNY), trimPercent(margin))
	fmt.Fprintf(&b, "覆盖渠道：%d 个；明细行：%d 行", t.ChannelCount, t.PricedRows)

	// 有行没参与合计时必须说出来。否则这段文字会被读成「整体利润」，
	// 而它实际上只覆盖了已维护倍率的那些渠道。
	if t.PricedRows < t.TotalRows {
		fmt.Fprintf(&b, "\n注：另有 %d 行因渠道未维护上游倍率未计入成本与结算额合计",
			t.TotalRows-t.PricedRows)
	}
	return b.String()
}

// trimMoney 金额去掉多余的尾零：对外文案里「¥15146.6056」比「¥15146.605600」好读，
// 而「¥100」也不该写成「¥100.0000」。
func trimMoney(v float64) string {
	s := strconv.FormatFloat(round(v, MoneyDecimals), 'f', MoneyDecimals, 64)
	s = strings.TrimRight(s, "0")
	return strings.TrimSuffix(s, ".")
}

// trimPercent 毛利率保留两位小数。
func trimPercent(v float64) string {
	return strconv.FormatFloat(round(v, 2), 'f', 2, 64)
}

// aggRowsOf 摘出内嵌的 AggRow，供 ComputeGroupDiscounts 复用同一套折扣口径。
func aggRowsOf(rows []*CostRow) []*AggRow {
	out := make([]*AggRow, len(rows))
	for i, c := range rows {
		out[i] = c.AggRow
	}
	return out
}

// costOutputPath 由账单路径推出成本利润表路径（同目录、同扩展名）。
func costOutputPath(billPath string) string {
	dir := filepath.Dir(billPath)
	base := filepath.Base(billPath)
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	// 账单_xxx → 成本利润_xxx
	if strings.HasPrefix(stem, "账单") {
		stem = "成本利润" + strings.TrimPrefix(stem, "账单")
	} else {
		stem = stem + "_成本利润"
	}
	return filepath.Join(dir, stem+ext)
}
