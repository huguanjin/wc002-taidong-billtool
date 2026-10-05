package billing

import (
	"fmt"
	"path/filepath"
	"strings"
)

// 成本表在账单 29 列（A~AC）之后追加的列。
//
// 保持 A~AC 与账单逐列同构，是为了让成本表能与账单并排比对：
// 同一行的 S/T/V 是「对客户的结算」，AD~AG 是「上游成本」，两者相减就是毛利。
// 如果把渠道信息塞进 C 列，行与账单就对不上了。
const (
	CostColumnChannelID    = "AD"
	CostColumnChannelName  = "AE"
	CostColumnUpstreamOff  = "AF"
	CostColumnUpstreamCost = "AG"
)

// WriteCostFromTemplate 写出成本表：账单全部列 + 渠道 ID / 渠道名称 / 上游折扣 / 上游成本。
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
	}

	// 成本表要按渠道的行序写出，而共享核心接收的是 []*AggRow。
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

// defaultCostName 成本表文件名，与账单/脱敏日志的命名习惯一致。
func defaultCostName(stem string) string {
	if strings.Contains(stem, "日志查询") {
		return strings.Replace(stem, "日志查询", "成本", 1)
	}
	if strings.Contains(stem, "日志") {
		return strings.Replace(stem, "日志", "成本", 1)
	}
	return stem + "_成本"
}

// costOutputPath 由账单路径推出成本表路径（同目录、同扩展名）。
func costOutputPath(billPath string) string {
	dir := filepath.Dir(billPath)
	base := filepath.Base(billPath)
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	// 账单_xxx → 成本_xxx
	if strings.HasPrefix(stem, "账单") {
		stem = "成本" + strings.TrimPrefix(stem, "账单")
	} else {
		stem = stem + "_成本"
	}
	return filepath.Join(dir, stem+ext)
}
