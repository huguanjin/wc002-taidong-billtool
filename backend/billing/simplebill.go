package billing

import (
	"fmt"
	"sort"
	"strings"

	"github.com/xuri/excelize/v2"
)

// 账单模板二：按 (分组, 模型) 汇总的简易账单。
//
// 面向「只要每个分组每个模型调用了多少次、花了多少额度」的客户。与模板一的区别是
// **完全不参与定价**：不算刊例、不查价表、不反推折扣、不估成本，金额就是站点实收
// 额度按 QuotaPerCNY 折算。
//
// 这带来一个实际好处：没有价表（price_table.xlsx / db_price_cache.json）的部署也能出这张表。
// 代价是它无法回答「单价×用量是否成立」「折扣是多少」这类问题——需要那些的客户用模板一。

// SimpleBillRow 模板二的一行：一个 (分组, 模型) 的汇总。
//
// 字段与手工核对用的 SQL 逐列对应，便于客户拿 SQL 结果与本表逐格比对：
//
//	SELECT `group`, model_name, COUNT(*) AS hit_count,
//	       SUM(prompt_tokens), SUM(completion_tokens), SUM(quota), ROUND(SUM(quota)/500000, 4)
//	FROM logs WHERE type = 2 ... GROUP BY `group`, model_name
//
// 与那条 SQL 唯一的差别是**退款**：本表把 type=6 的退款冲抵掉了（见 AggregateSimpleBill），
// 而示例 SQL 只取 type=2，在有任务退款的账期上会比实收偏高。
type SimpleBillRow struct {
	Group string `json:"group"`
	Model string `json:"model"`
	// HitCount 请求次数（只数消费行；任务的结算/退款行不算一次请求）。
	HitCount int `json:"hitCount"`
	// TotalPrompt / TotalCompletion 输入、输出 token 合计。
	TotalPrompt     float64 `json:"totalPrompt"`
	TotalCompletion float64 `json:"totalCompletion"`
	// TotalQuota 额度合计，**净额**（消费 − 退款 + 补扣）。
	TotalQuota float64 `json:"totalQuota"`
	// TotalCostCNY 金额 = 额度 / QuotaPerCNY。
	TotalCostCNY float64 `json:"totalCostCny"`
}

// SimpleBillColumns 模板二的列名，账单与汇总脱敏日志共用同一套（客户已确认两者列一致）。
var SimpleBillColumns = []string{
	"分组", "模型", "次数", "输入Token", "输出Token", "额度", "金额（人民币）",
}

// AggregateSimpleBill 把日志行按 (分组, 模型) 汇总。
//
// 直接从原始行累加，不经过 AggRow：那是「带定价的桶」，而本模板不需要任何定价中间量。
// 分组取日志原值，不加 DisplayGroup() 那种倍率后缀——客户要与 SQL 结果对账，
// 后缀会让 `Codex` 变成 `Codex(0.4)`，反而对不上。
//
// 退款处理与主账单口径一致（见 IsTaskQuotaAdjustment）：任务的结算/退款行只改额度，
// 不累 token、不计次数。这一点在本模板上尤其要紧——它的金额就是额度本身，
// 若把任务失败已退还的预扣算进去，误差会全部落在账单上。
func AggregateSimpleBill(rows [][]string, headers []string) ([]SimpleBillRow, error) {
	col := map[string]int{}
	for i, h := range headers {
		if h != "" {
			col[h] = i
		}
	}
	required := []string{"model_name", "group", "prompt_tokens", "completion_tokens", "quota"}
	var missing []string
	for _, name := range required {
		if _, ok := col[name]; !ok {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("日志缺少列: %v；实际列: %v（简易账单需要 model_name/group/prompt_tokens/completion_tokens/quota）", missing, headers)
	}

	idxModel := col["model_name"]
	idxGroup := col["group"]
	idxPrompt := col["prompt_tokens"]
	idxCompletion := col["completion_tokens"]
	idxQuota := col["quota"]
	idxOther, hasOther := col["other"]
	idxType, hasType := col["type"]

	type key struct{ group, model string }
	buckets := map[key]*SimpleBillRow{}
	var order []key

	for _, row := range rows {
		if len(row) == 0 {
			continue
		}
		model := strings.TrimSpace(cellAt(row, idxModel))
		group := strings.TrimSpace(cellAt(row, idxGroup))
		if model == "" && group == "" {
			continue
		}
		quota := ToFloat(cellAt(row, idxQuota))
		other := ""
		if hasOther {
			other = cellAt(row, idxOther)
		}

		k := key{group, model}
		r, exists := buckets[k]
		if !exists {
			r = &SimpleBillRow{Group: group, Model: model}
			buckets[k] = r
			order = append(order, k)
		}

		// 任务的结算/退款行：只调整额度。
		if IsTaskQuotaAdjustment(other) {
			logType := ""
			if hasType {
				logType = cellAt(row, idxType)
			}
			if delta, ok := QuotaAdjustmentDelta(logType, quota); ok {
				// 净额 = 消费 − 退款 + 补扣，与 AggRow.SiteCNY 同一符号约定。
				r.TotalQuota -= delta
			}
			continue
		}

		r.HitCount++
		r.TotalPrompt += ToFloat(cellAt(row, idxPrompt))
		r.TotalCompletion += ToFloat(cellAt(row, idxCompletion))
		r.TotalQuota += quota
	}

	out := make([]SimpleBillRow, 0, len(buckets))
	for _, k := range order {
		r := buckets[k]
		// 金额保留 4 位：与手工 SQL 的 ROUND(..., 4) 一致，客户逐格比对时不会差在精度上。
		r.TotalCostCNY = round(r.TotalQuota/QuotaPerCNY, MoneyDecimals)
		out = append(out, *r)
	}

	// 分组按首次出现顺序（与日志里各分组的自然顺序一致），组内按次数降序——
	// 与示例 SQL 的 ORDER BY `group`, hit_count DESC 对齐，客户对账时行序不用重新找。
	groupRank := map[string]int{}
	for i, k := range order {
		if _, seen := groupRank[k.group]; !seen {
			groupRank[k.group] = i
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Group != out[j].Group {
			return groupRank[out[i].Group] < groupRank[out[j].Group]
		}
		if out[i].HitCount != out[j].HitCount {
			return out[i].HitCount > out[j].HitCount
		}
		return out[i].Model < out[j].Model
	})
	return out, nil
}

// WriteSimpleBill 写出模板二的表。
//
// 账单与汇总脱敏日志共用这一个函数：两者的列完全相同（客户已确认），
// 各写一份迟早会随时间走偏——那时同一笔账的两张表会对不上。
//
// 表头写在代码里而不是读模板文件：列是固定的七列，且没有任何一张现成的模板可复用；
// 从零建表比让部署方多维护一个二进制模板文件可靠（漏挂文件的报错很难自解释）。
func WriteSimpleBill(path string, rows []SimpleBillRow, sheetName string) error {
	f := excelize.NewFile()
	defer f.Close()

	if strings.TrimSpace(sheetName) == "" {
		sheetName = "简易账单"
	}
	// 改名后必须用**新名字**操作：GetSheetName(0) 拿到的是改名前的默认名
	// （Sheet1），继续用它会让每一次写入都报 "sheet Sheet1 does not exist"。
	// 这里重取一次而不是复用旧值，就是这么个一行的坑。
	if err := f.SetSheetName(f.GetSheetName(0), sheetName); err != nil {
		return fmt.Errorf("设置工作表名失败: %w", err)
	}
	sheet := f.GetSheetName(0)

	styleHeader, err := f.NewStyle(&excelize.Style{Font: &excelize.Font{Bold: true}})
	if err != nil {
		return err
	}
	styleAccounting, err := f.NewStyle(&excelize.Style{CustomNumFmt: strPtr(AccountingFmt)})
	if err != nil {
		return err
	}
	styleMoney, err := f.NewStyle(&excelize.Style{CustomNumFmt: strPtr(MoneyCNYFmt)})
	if err != nil {
		return err
	}
	styleMoneyBold, err := f.NewStyle(&excelize.Style{
		CustomNumFmt: strPtr(MoneyCNYFmt), Font: &excelize.Font{Bold: true},
	})
	if err != nil {
		return err
	}
	styleAccountingBold, err := f.NewStyle(&excelize.Style{
		CustomNumFmt: strPtr(AccountingFmt), Font: &excelize.Font{Bold: true},
	})
	if err != nil {
		return err
	}

	axis := func(col, r int) string {
		a, _ := excelize.CoordinatesToCellName(col, r)
		return a
	}

	header := make([]interface{}, len(SimpleBillColumns))
	for i, h := range SimpleBillColumns {
		header[i] = h
	}
	if err := f.SetSheetRow(sheet, "A1", &header); err != nil {
		return err
	}
	for i := range SimpleBillColumns {
		if err := f.SetCellStyle(sheet, axis(i+1, 1), axis(i+1, 1), styleHeader); err != nil {
			return err
		}
	}

	// 数据从第 2 行开始：这张表没有模板里那种「第二行写说明」的约定，
	// 多留一行空白只会让客户以为是漏填。
	firstDataRow := 2
	for i, r := range rows {
		row := firstDataRow + i
		values := []interface{}{r.Group, r.Model, r.HitCount, r.TotalPrompt, r.TotalCompletion, r.TotalQuota, r.TotalCostCNY}
		if err := f.SetSheetRow(sheet, axis(1, row), &values); err != nil {
			return err
		}
		for _, col := range []int{4, 5, 6} {
			if err := f.SetCellStyle(sheet, axis(col, row), axis(col, row), styleAccounting); err != nil {
				return err
			}
		}
		if err := f.SetCellStyle(sheet, axis(7, row), axis(7, row), styleMoney); err != nil {
			return err
		}
	}

	// 合计行。金额列写 SUM 公式而不是算好的数值：客户能在 Excel 里点开看它加了哪几行，
	// 与模板一合计行的做法一致。
	if len(rows) > 0 {
		lastDataRow := firstDataRow + len(rows) - 1
		totalRow := lastDataRow + 1
		if err := f.SetCellValue(sheet, axis(1, totalRow), "合计"); err != nil {
			return err
		}
		if err := f.SetCellStyle(sheet, axis(1, totalRow), axis(1, totalRow), styleHeader); err != nil {
			return err
		}
		sumCols := []struct {
			col   int
			style int
		}{
			{3, styleAccountingBold}, // 次数
			{4, styleAccountingBold}, // 输入
			{5, styleAccountingBold}, // 输出
			{6, styleMoneyBold},      // 额度
			{7, styleMoneyBold},      // 金额
		}
		for _, sc := range sumCols {
			letter, _ := excelize.ColumnNumberToName(sc.col)
			formula := fmt.Sprintf("SUM(%s%d:%s%d)", letter, firstDataRow, letter, lastDataRow)
			if err := f.SetCellFormula(sheet, axis(sc.col, totalRow), formula); err != nil {
				return err
			}
			if err := f.SetCellStyle(sheet, axis(sc.col, totalRow), axis(sc.col, totalRow), sc.style); err != nil {
				return err
			}
		}
	}

	widths := map[string]float64{"A": 20, "B": 28, "C": 10, "D": 16, "E": 16, "F": 16, "G": 16}
	for col, w := range widths {
		if err := f.SetColWidth(sheet, col, col, w); err != nil {
			return err
		}
	}

	if err := f.SaveAs(path); err != nil {
		return fmt.Errorf("保存简易账单失败: %w", err)
	}
	return nil
}

// SimpleBillTotals 合计，供调用方组装结果与前端摘要。
type SimpleBillTotals struct {
	HitCount        int
	TotalPrompt     float64
	TotalCompletion float64
	TotalQuota      float64
	TotalCostCNY    float64
}

// SumSimpleBill 把各行加总。金额单独累加各行（而不是用总额度再算一次），
// 这样合计与逐行之和逐位相等，客户手工加总不会差出几分钱。
func SumSimpleBill(rows []SimpleBillRow) SimpleBillTotals {
	var t SimpleBillTotals
	for _, r := range rows {
		t.HitCount += r.HitCount
		t.TotalPrompt += r.TotalPrompt
		t.TotalCompletion += r.TotalCompletion
		t.TotalQuota += r.TotalQuota
		t.TotalCostCNY += r.TotalCostCNY
	}
	return t
}
