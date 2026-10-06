package billing

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/xuri/excelize/v2"
)

// 简易账单（模板二）的测试。
//
// 这张表的口径是「站点实收额度 ÷ 500000」，所以最要紧的两件事是：
//  1. 退款必须冲抵（否则任务失败已退的钱照收）；
//  2. token 与次数只数消费行（退款行不是一次请求）。

func simpleLogHeaders() []string {
	return []string{"model_name", "group", "prompt_tokens", "completion_tokens", "quota", "other", "type"}
}

// TestAggregateSimpleBillGroupsByGroupAndModel 按 (分组, 模型) 汇总，
// 同分组下多个倍率桶要合并成一行。
//
// 倍率是模板一的聚合维度（同分组多倍率会拆多行），但模板二的用户要的是
// 「这个分组这个模型一共花了多少」，拆行反而对不上他们的 SQL。
func TestAggregateSimpleBillGroupsByGroupAndModel(t *testing.T) {
	headers := simpleLogHeaders()
	rows := [][]string{
		{"gpt-5.5", "Codex", "1000", "100", "45000", `{"group_ratio":0.4}`, "2"},
		{"gpt-5.5", "Codex", "2000", "200", "45000", `{"group_ratio":0.9}`, "2"},
		{"gpt-5.6", "Codex", "500", "50", "10000", `{"group_ratio":0.4}`, "2"},
		{"gpt-5.5", "Claude", "300", "30", "6000", `{"group_ratio":1.8}`, "2"},
	}

	got, err := AggregateSimpleBill(rows, headers)
	require.NoError(t, err)
	require.Len(t, got, 3, "同分组同模型的两条不同倍率行要合并成一行")

	byModel := map[string]SimpleBillRow{}
	for _, r := range got {
		byModel[r.Group+"/"+r.Model] = r
	}

	codex := byModel["Codex/gpt-5.5"]
	assert.Equal(t, 2, codex.HitCount)
	assert.Equal(t, 3000.0, codex.TotalPrompt)
	assert.Equal(t, 300.0, codex.TotalCompletion)
	assert.Equal(t, 90000.0, codex.TotalQuota, "两条不同倍率的额度要加在一起")
	assert.InDelta(t, 0.18, codex.TotalCostCNY, 1e-9, "90000/500000")

	assert.Equal(t, 1, byModel["Codex/gpt-5.6"].HitCount)
	assert.Equal(t, 1, byModel["Claude/gpt-5.5"].HitCount)
}

// TestSimpleBillAmountMatchesSQL 金额口径与手工核对的 SQL 逐位一致。
//
// 用户会拿 SQL 的结果跟这张表对，差一个数就会被当成算错了。
func TestSimpleBillAmountMatchesSQL(t *testing.T) {
	headers := simpleLogHeaders()
	rows := [][]string{
		{"gpt-image-2-all", "Codex", "0", "0", "45000", `{"model_price":0.12,"group_ratio":0.75}`, "2"},
	}

	got, err := AggregateSimpleBill(rows, headers)
	require.NoError(t, err)
	require.Len(t, got, 1)

	// ROUND(45000/500000, 4) = 0.09
	assert.Equal(t, 0.09, got[0].TotalCostCNY)
	assert.Equal(t, 45000.0, got[0].TotalQuota)
}

// TestSimpleBillRefundOffsetsQuotaOnly 退款只冲额度，不碰 token 与次数。
//
// 这是本模板最要紧的一条：金额就是额度本身，退错方向或多算一次都会直接
// 变成给客户多收钱。而 token/次数是被用户拿来核对用量的，退款行不是一次请求。
func TestSimpleBillRefundOffsetsQuotaOnly(t *testing.T) {
	headers := simpleLogHeaders()
	rows := [][]string{
		{"gpt-5.5", "Codex", "1000", "100", "500000", `{"is_task":true,"group_ratio":1.8}`, "2"},
		{"gpt-5.5", "Codex", "0", "0", "200000", `{"task_id":7,"reason":"failed"}`, "6"},
	}

	got, err := AggregateSimpleBill(rows, headers)
	require.NoError(t, err)
	require.Len(t, got, 1)

	r := got[0]
	assert.Equal(t, 1, r.HitCount, "退款行不是一次请求，不该计入次数")
	assert.Equal(t, 1000.0, r.TotalPrompt, "token 只由消费行贡献")
	assert.Equal(t, 100.0, r.TotalCompletion)
	// (500000 − 200000) / 500000 = 0.6
	assert.Equal(t, 300000.0, r.TotalQuota, "额度要减掉退款")
	assert.InDelta(t, 0.6, r.TotalCostCNY, 1e-9)
}

// TestSimpleBillMakeupSettlementAddsBack 补扣结算行（type=2 带 task_id）把额度加回去。
//
// 它和消费行同为 type=2，只有 task_id 能区分；当成消费行会把 token 与次数算重。
func TestSimpleBillMakeupSettlementAddsBack(t *testing.T) {
	headers := simpleLogHeaders()
	rows := [][]string{
		{"gpt-5.5", "Codex", "1000", "100", "500000", `{"is_task":true,"group_ratio":1.8}`, "2"},
		{"gpt-5.5", "Codex", "0", "0", "300000", `{"task_id":5,"pre_consumed_quota":500000}`, "2"},
	}

	got, err := AggregateSimpleBill(rows, headers)
	require.NoError(t, err)
	require.Len(t, got, 1)

	r := got[0]
	assert.Equal(t, 1, r.HitCount, "补扣行不是一次请求")
	assert.Equal(t, 1000.0, r.TotalPrompt, "补扣行 token 为 0，不该影响用量")
	assert.Equal(t, 800000.0, r.TotalQuota, "预扣 500000 + 补扣 300000")
	assert.InDelta(t, 1.6, r.TotalCostCNY, 1e-9)
}

// TestSimpleBillNegativeQuotaNotClamped 某期退款多于消费时净额为负，原样输出。
func TestSimpleBillNegativeQuotaNotClamped(t *testing.T) {
	headers := simpleLogHeaders()
	rows := [][]string{
		{"gpt-5.5", "Codex", "100", "10", "100000", `{"group_ratio":1.8}`, "2"},
		{"gpt-5.5", "Codex", "0", "0", "900000", `{"task_id":9}`, "6"},
	}

	got, err := AggregateSimpleBill(rows, headers)
	require.NoError(t, err)
	require.Len(t, got, 1)

	assert.Equal(t, -800000.0, got[0].TotalQuota)
	assert.InDelta(t, -1.6, got[0].TotalCostCNY, 1e-9, "负额原样输出，不许夹到 0")
}

// TestAggregateSimpleBillMissingColumns 缺关键列时报错，而不是算出一份静默为 0 的表。
func TestAggregateSimpleBillMissingColumns(t *testing.T) {
	// 缺 quota
	_, err := AggregateSimpleBill([][]string{{"m", "g", "1", "2"}},
		[]string{"model_name", "group", "prompt_tokens", "completion_tokens"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "quota")

	// 缺 model_name
	_, err = AggregateSimpleBill([][]string{{"g", "1", "2", "3"}},
		[]string{"group", "prompt_tokens", "completion_tokens", "quota"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "model_name")
}

// TestSimpleBillSortOrder 分组按首次出现顺序，组内按次数降序——与 SQL 的
// ORDER BY `group`, hit_count DESC 一致，用户对账时行序不用重新找。
func TestSimpleBillSortOrder(t *testing.T) {
	headers := simpleLogHeaders()
	rows := [][]string{
		{"b-model", "Bravo", "1", "1", "1000", `{}`, "2"},
		{"a-model", "Alpha", "1", "1", "1000", `{}`, "2"},
		// Bravo 组内：少调用的先出现，但排序后应排到后面
		{"z-model", "Bravo", "1", "1", "1000", `{}`, "2"},
		{"z-model", "Bravo", "1", "1", "1000", `{}`, "2"},
		{"z-model", "Bravo", "1", "1", "1000", `{}`, "2"},
	}

	got, err := AggregateSimpleBill(rows, headers)
	require.NoError(t, err)
	require.Len(t, got, 3)

	// 分组按首次出现顺序：Bravo 先出现，所以它的两行都排在 Alpha 之前。
	// 组内按次数降序：z-model 3 次排在 b-model 1 次之前。
	assert.Equal(t, "Bravo", got[0].Group)
	assert.Equal(t, "z-model", got[0].Model, "组内按次数降序")
	assert.Equal(t, "Bravo", got[1].Group)
	assert.Equal(t, "b-model", got[1].Model)
	assert.Equal(t, "Alpha", got[2].Group, "分组按首次出现顺序")
}

// TestSumSimpleBill 合计等于逐行之和（逐位相等，客户手工加总不会差出几分钱）。
func TestSumSimpleBill(t *testing.T) {
	rows := []SimpleBillRow{
		{Group: "A", Model: "m1", HitCount: 3, TotalPrompt: 100, TotalCompletion: 10, TotalQuota: 45000, TotalCostCNY: 0.09},
		{Group: "A", Model: "m2", HitCount: 2, TotalPrompt: 200, TotalCompletion: 20, TotalQuota: 55000, TotalCostCNY: 0.11},
	}
	got := SumSimpleBill(rows)

	assert.Equal(t, 5, got.HitCount)
	assert.Equal(t, 300.0, got.TotalPrompt)
	assert.Equal(t, 30.0, got.TotalCompletion)
	assert.Equal(t, 100000.0, got.TotalQuota)
	assert.InDelta(t, 0.2, got.TotalCostCNY, 1e-9, "0.09 + 0.11，逐行相加")
}

// TestWriteSimpleBillLayout 写出形态：表头七列、数据行、合计行。
func TestWriteSimpleBillLayout(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "账单二.xlsx")

	rows := []SimpleBillRow{
		{Group: "Codex", Model: "gpt-5.5", HitCount: 3, TotalPrompt: 100, TotalCompletion: 10, TotalQuota: 45000, TotalCostCNY: 0.09},
		{Group: "Codex", Model: "gpt-5.6", HitCount: 2, TotalPrompt: 200, TotalCompletion: 20, TotalQuota: 55000, TotalCostCNY: 0.11},
	}
	require.NoError(t, WriteSimpleBill(path, rows, "简易账单"))

	f, err := excelize.OpenFile(path)
	require.NoError(t, err)
	defer f.Close()

	sheet := f.GetSheetName(0)
	assert.Equal(t, "简易账单", sheet)

	// 表头
	for i, want := range SimpleBillColumns {
		axis, _ := excelize.CoordinatesToCellName(i+1, 1)
		got, err := f.GetCellValue(sheet, axis)
		require.NoError(t, err)
		assert.Equal(t, want, got, "第 %d 列表头", i+1)
	}

	// 数据从第 2 行开始
	v, err := f.GetCellValue(sheet, "A2")
	require.NoError(t, err)
	assert.Equal(t, "Codex", v)
	v, err = f.GetCellValue(sheet, "C2")
	require.NoError(t, err)
	assert.Equal(t, "3", v, "次数是数字")

	// 合计行在第 4 行，金额列写的是 SUM 公式（可追溯）
	v, err = f.GetCellValue(sheet, "A4")
	require.NoError(t, err)
	assert.Equal(t, "合计", v)

	formula, err := f.GetCellFormula(sheet, "G4")
	require.NoError(t, err)
	assert.Equal(t, "SUM(G2:G3)", formula, "合计写公式而不是算好的数值")
	formula, err = f.GetCellFormula(sheet, "C4")
	require.NoError(t, err)
	assert.Equal(t, "SUM(C2:C3)", formula)
}

// TestWriteSimpleBillEmptyNoTotalRow 没有数据行时不写合计行，免得客户以为漏了数据。
func TestWriteSimpleBillEmptyNoTotalRow(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "空账单.xlsx")
	require.NoError(t, WriteSimpleBill(path, nil, "简易账单"))

	f, err := excelize.OpenFile(path)
	require.NoError(t, err)
	defer f.Close()

	sheet := f.GetSheetName(0)
	// 用 GetRows 而不是 GetCellValue：excelize 对「从未写过的单元格」会返回
	// "sheet does not exist" 之类的错，而这里要断言的正是「第 2 行是空的」。
	rows, err := f.GetRows(sheet)
	require.NoError(t, err)
	assert.Len(t, rows, 1, "只有表头一行，不该出现合计行")
	assert.Equal(t, SimpleBillColumns[0], rows[0][0])
}

// TestGenerateSimpleBillNeedsNoPriceTable 模板二不读价表。
//
// 这是它与模板一的关键差异，也是设计上的一个真实好处：没有 price_table.xlsx
// 或 db_price_cache.json 的部署，也能给这类「只要汇总」的客户出账。
// 传一个不存在的路径来证明它确实没被碰过。
func TestGenerateSimpleBillNeedsNoPriceTable(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "日志查询_2026-09-01_2026-09-30_ab12cd.xlsx")
	outDir := filepath.Join(dir, "out")
	require.NoError(t, os.MkdirAll(outDir, 0o755))

	// 造一份含分组/模型/用量/额度的日志。
	f := excelize.NewFile()
	sheet := f.GetSheetName(0)
	headers := []interface{}{"model_name", "group", "prompt_tokens", "completion_tokens", "quota", "other", "type"}
	require.NoError(t, f.SetSheetRow(sheet, "A1", &headers))
	rows := [][]interface{}{
		{"gpt-5.5", "Codex", 1000, 100, 45000, `{"group_ratio":0.4}`, 2},
		{"gpt-5.5", "Codex", 2000, 200, 45000, `{"group_ratio":0.9}`, 2},
		{"gpt-5.6", "Claude", 500, 50, 10000, `{"group_ratio":1.8}`, 2},
	}
	for i, r := range rows {
		axis, _ := excelize.CoordinatesToCellName(1, i+2)
		require.NoError(t, f.SetSheetRow(sheet, axis, &r))
	}
	require.NoError(t, f.SaveAs(logPath))
	require.NoError(t, f.Close())

	result, err := GenerateBill(logPath,
		filepath.Join(dir, "不存在的模板.xlsx"),   // 模板一才需要，这里不该被读
		filepath.Join(dir, "不存在的报价表.xlsx"),  // 同上
		filepath.Join(dir, "不存在的价格缓存.json"), // 同上
		outDir, Params{BillTemplate: BillTemplateSimple, SanitizedLog: true, CustomerName: "钛动"})
	require.NoError(t, err, "模板二不该依赖模板一/价表/价格缓存")

	assert.Contains(t, filepath.Base(result.BillPath), "账单二", "文件名要与模板一区分开")
	assert.Contains(t, filepath.Base(result.BillPath), "钛动", "客户名后缀仍要生效")
	assert.NotEmpty(t, result.SanitizedPath)
	assert.Contains(t, filepath.Base(result.SanitizedPath), "脱敏日志二")
	assert.Empty(t, result.CostPath, "模板二不产出成本利润表")
	assert.False(t, result.CostBlocked)

	// 两个文件都真的写出来了。
	require.FileExists(t, result.BillPath)
	require.FileExists(t, result.SanitizedPath)

	// 摘要：金额 = (45000+45000+10000)/500000 = 0.2；行数 = 汇总后的 2 行。
	assert.InDelta(t, 0.2, result.Summary.SettleCNYTotal, 1e-9)
	assert.Equal(t, 2, result.Summary.RowCount, "同分组同倍率桶合并后只剩 Codex/gpt-5.5 与 Claude/gpt-5.6 两行")
	assert.Empty(t, result.Summary.Rows, "模板二不返回逐行明细")
}

// TestGenerateSimpleBillIgnoresGenerateCost 勾了成本利润表也不该产出它。
//
// 简易账单的金额来自站点额度，与上游成本无关；真去算会要求渠道倍率，
// 把一张本可立即完成的汇总账单卡在完全不相关的依赖上。
func TestGenerateSimpleBillIgnoresGenerateCost(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "日志.xlsx")
	outDir := filepath.Join(dir, "out")
	require.NoError(t, os.MkdirAll(outDir, 0o755))

	f := excelize.NewFile()
	sheet := f.GetSheetName(0)
	require.NoError(t, f.SetSheetRow(sheet, "A1", &[]interface{}{"model_name", "group", "prompt_tokens", "completion_tokens", "quota"}))
	require.NoError(t, f.SetSheetRow(sheet, "A2", &[]interface{}{"m", "g", 1, 1, 500000}))
	require.NoError(t, f.SaveAs(logPath))
	require.NoError(t, f.Close())

	result, err := GenerateBill(logPath, "", "", "", outDir,
		Params{BillTemplate: BillTemplateSimple, GenerateCost: true})
	require.NoError(t, err)
	assert.Empty(t, result.CostPath)
	assert.False(t, result.CostBlocked)
	assert.InDelta(t, 1.0, result.Summary.SettleCNYTotal, 1e-9)
}
