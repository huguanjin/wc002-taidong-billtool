package billing

import (
	"fmt"
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

	got, err := AggregateSimpleBill(rows, headers, SimpleBillOptions{})
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

	got, err := AggregateSimpleBill(rows, headers, SimpleBillOptions{})
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

	got, err := AggregateSimpleBill(rows, headers, SimpleBillOptions{})
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

	got, err := AggregateSimpleBill(rows, headers, SimpleBillOptions{})
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

	got, err := AggregateSimpleBill(rows, headers, SimpleBillOptions{})
	require.NoError(t, err)
	require.Len(t, got, 1)

	assert.Equal(t, -800000.0, got[0].TotalQuota)
	assert.InDelta(t, -1.6, got[0].TotalCostCNY, 1e-9, "负额原样输出，不许夹到 0")
}

// TestAggregateSimpleBillMissingColumns 缺关键列时报错，而不是算出一份静默为 0 的表。
func TestAggregateSimpleBillMissingColumns(t *testing.T) {
	// 缺 quota
	_, err := AggregateSimpleBill([][]string{{"m", "g", "1", "2"}},
		[]string{"model_name", "group", "prompt_tokens", "completion_tokens"}, SimpleBillOptions{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "quota")

	// 缺 model_name
	_, err = AggregateSimpleBill([][]string{{"g", "1", "2", "3"}},
		[]string{"group", "prompt_tokens", "completion_tokens", "quota"}, SimpleBillOptions{})
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

	got, err := AggregateSimpleBill(rows, headers, SimpleBillOptions{})
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

// TestFormatSimpleBillSummary 简易账单的可复制文字。
//
// 这段文字是给人粘到聊天/邮件里的，收件人必须能一眼看出金额是什么口径——
// 它与模板一账单上的数是**两个不同的数**（站点实收额度 vs 刊例×折扣），
// 不写清楚会被当成算错了。
func TestFormatSimpleBillSummary(t *testing.T) {
	rows := []SimpleBillRow{
		{Group: "Codex", Model: "gpt-5.5", HitCount: 120, TotalPrompt: 1_000_000, TotalCompletion: 100_000, TotalQuota: 900_000, TotalCostCNY: 1.8},
		{Group: "Codex", Model: "gpt-5.6", HitCount: 30, TotalPrompt: 0, TotalCompletion: 0, TotalQuota: 200_000, TotalCostCNY: 0.4},
		{Group: "Claude", Model: "claude-opus-4-6", HitCount: 5, TotalPrompt: 0, TotalCompletion: 0, TotalQuota: 100_000, TotalCostCNY: 0.2},
	}
	totals := SumSimpleBill(rows)
	header := []string{"客户：钛动", "时段：2026-09-01 00:00:00 ~ 2026-09-30 23:59:59"}

	got := FormatSimpleBillSummary(rows, totals, 2026, 9, header)

	want := `客户：钛动
时段：2026-09-01 00:00:00 ~ 2026-09-30 23:59:59
账期：2026-09
账单金额：¥2.4
请求次数：155 次；汇总行：3 行
分组小计：Codex ¥2.2；Claude ¥0.2
注：金额为站点实际扣费额度 ÷ 500000，已扣除任务退款`
	assert.Equal(t, want, got)
}

// TestFormatSimpleBillSummarySingleGroup 只有一个分组时不写「分组小计」——
// 那行就是总额的复述，没有信息量。
func TestFormatSimpleBillSummarySingleGroup(t *testing.T) {
	rows := []SimpleBillRow{
		{Group: "Codex", Model: "m1", HitCount: 2, TotalQuota: 100_000, TotalCostCNY: 0.2},
		{Group: "Codex", Model: "m2", HitCount: 1, TotalQuota: 50_000, TotalCostCNY: 0.1},
	}
	got := FormatSimpleBillSummary(rows, SumSimpleBill(rows), 2026, 9, nil)

	assert.Contains(t, got, "分组：Codex")
	assert.NotContains(t, got, "分组小计")
}

// TestFormatSimpleBillSummaryNoPeriod 账期推断不出来时整行省略，
// 而不是写「账期：0-0」这种看着像出错了的东西。
func TestFormatSimpleBillSummaryNoPeriod(t *testing.T) {
	rows := []SimpleBillRow{{Group: "G", Model: "m", HitCount: 1, TotalQuota: 500_000, TotalCostCNY: 1}}
	got := FormatSimpleBillSummary(rows, SumSimpleBill(rows), 0, 0, nil)

	assert.NotContains(t, got, "账期")
	assert.Contains(t, got, "账单金额：¥1")
}

// TestSimpleBillHasCopyableSummary 出账结果里必须带可复制文字。
//
// 这是个真实回归：模板二不产成本利润表，而那段可复制文字原先挂在
// 「成本表生成成功」这个条件上，于是简易账单执行完，页面上什么可复制的都没有。
func TestSimpleBillHasCopyableSummary(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "日志查询_2026-09-01_2026-09-30_ab12cd.xlsx")
	outDir := filepath.Join(dir, "out")
	require.NoError(t, os.MkdirAll(outDir, 0o755))

	f := excelize.NewFile()
	sheet := f.GetSheetName(0)
	require.NoError(t, f.SetSheetRow(sheet, "A1", &[]interface{}{"model_name", "group", "prompt_tokens", "completion_tokens", "quota", "created_at"}))
	// created_at 用 2026-09-15 的 Unix 秒，验证「文件名认不出来时退回日志内容」这一层。
	require.NoError(t, f.SetSheetRow(sheet, "A2", &[]interface{}{"gpt-5.5", "Codex", 1000, 100, 900000, 1789430400}))
	require.NoError(t, f.SaveAs(logPath))
	require.NoError(t, f.Close())

	result, err := GenerateBill(logPath, "", "", "", outDir, Params{
		BillTemplate:  BillTemplateSimple,
		CustomerName:  "钛动",
		SummaryHeader: []string{"客户：钛动"},
	})
	require.NoError(t, err)

	require.NotEmpty(t, result.BillSummary, "简易账单必须有可复制的账单摘要")
	assert.Contains(t, result.BillSummary, "账单金额：¥1.8")
	assert.Contains(t, result.BillSummary, "客户：钛动", "定位行要带进来")
	assert.Contains(t, result.BillSummary, "账期：2026-09",
		"文件名是日期式（日志查询_2026-09-01_...），认不出「N月」，必须退回日志的 created_at")
	assert.Empty(t, result.CostSummary, "简易账单不产成本利润摘要")

	// 账期也要填进 Summary，否则结果区会显示 0-00。
	assert.Equal(t, 2026, result.Summary.Year)
	assert.Equal(t, 9, result.Summary.Month)
}

// ---- 成本三列（官方刊例 / 上游成本 / 利润）----

// simpleCostLogHeaders 带 channel_id 的日志表头，成本口径的测试都用它。
//
// 与 simpleLogHeaders 分开：成本列要查渠道倍率，而渠道号来自 channel_id 列或
// other.admin_info.use_channel——两条来源各有各的用例，所以这里把 channel_id
// 显式列出来，让「这次走的是哪条来源」在用例里一眼可见。
func simpleCostLogHeaders() []string {
	return []string{"model_name", "group", "prompt_tokens", "completion_tokens", "quota", "other", "type", "channel_id"}
}

// TestSimpleBillCostReverseDerivation 反推恒等式：官方刊例 = Σ(quota ÷ group_ratio) ÷ 500000。
//
// 用样例日志里 anti 组的真实形状：15 行按次计费、每行 quota 108000、group_ratio 1.8，
// 反推刊例应正好 1.8000 USD——即 15 × 0.12 的按次刊例（文档 1.1 的基线）。
//
// 这条是整个成本列的地基：站内的 quota 就是按「刊例 × 分组倍率 × 500000」记的，
// 除回去能否复原告刊例，决定了成本列到底可信不可信。
func TestSimpleBillCostReverseDerivation(t *testing.T) {
	headers := simpleCostLogHeaders()
	var rows [][]string
	for i := 0; i < 15; i++ {
		rows = append(rows, []string{
			"gpt-image-2-all", "anti", "0", "0", "108000",
			`{"model_price":0.12,"group_ratio":1.8}`, "2", "900",
		})
	}

	got, err := AggregateSimpleBill(rows, headers, SimpleBillOptions{
		CostColumns:    true,
		UpstreamRatios: map[int]float64{900: 0.4},
	})
	require.NoError(t, err)
	require.Len(t, got, 1)

	r := got[0]
	require.NotNil(t, r.OfficialListUSD, "group_ratio 与渠道倍率都有，成本必须算得出来")
	require.NotNil(t, r.UpstreamCostCNY)
	require.NotNil(t, r.ProfitCNY)
	assert.InDelta(t, 1.8, *r.OfficialListUSD, 1e-9, "15 × 0.12 的按次刊例")
	assert.Equal(t, 15, r.HitCount)
	assert.Equal(t, 15, r.CostRows)
	assert.Equal(t, 15, r.TotalRows)
	assert.False(t, r.CostPartial, "全部行都算了，不是部分覆盖")

	// 金额 = 15 × 108000 / 500000 = 3.24
	assert.InDelta(t, 3.24, r.TotalCostCNY, 1e-9)

	// 成本公式与模板一同源（文档 1.1）：刊例USD × 汇率 × (上游倍率 ÷ 7)。
	// 断言写成这个形式而不是「刊例USD × 上游倍率」：两者在数值上恰好等价，
	// 但文档那条才是两套产出共同的口径，以它为准才不会各自漂移。
	wantCost := 1.8 * DefaultExchangeRate * (0.4 / DiscountBaseFactor)
	assert.InDelta(t, wantCost, *r.UpstreamCostCNY, 1e-4)
	assert.InDelta(t, 3.24-wantCost, *r.ProfitCNY, 1e-4, "利润 = 金额 − 成本")
}

// TestSimpleBillCostCrossChecksWithTemplate1 模板一与模板二的成本交叉核对。
//
// 两条路径的推导方向相反：模板一从价表正算出刊例，模板二的刊例从 quota 反推。
// 同一份日志、同一套上游倍率下两个成本必须相等——能对上才说明「quota 里带着
// 分组倍率」这条前提是真的；对不上就是其中一条路径的公式错了，
// 而不是「两个口径本来就不同」。
func TestSimpleBillCostCrossChecksWithTemplate1(t *testing.T) {
	const rate = 7.0
	const groupRatio = 1.8
	const upstream = 0.4
	// 用文档 1.1 那个已核对过的值：gpt-5-mini/AZ 的 OfficialUSD = 24.233498。
	const listUSD = 24.233498

	// quota = 刊例USD × group_ratio × 500000，站内的记账恒等式。
	quota := listUSD * groupRatio * QuotaPerCNY

	headers := simpleCostLogHeaders()
	rows := [][]string{
		{"gpt-5-mini", "AZ", "1000", "100", formatFloat(quota), `{"group_ratio":1.8}`, "2", "1108"},
	}

	got, err := AggregateSimpleBill(rows, headers, SimpleBillOptions{
		CostColumns:    true,
		UpstreamRatios: map[int]float64{1108: upstream},
		ExchangeRate:   rate,
	})
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.NotNil(t, got[0].OfficialListUSD)
	assert.InDelta(t, listUSD, *got[0].OfficialListUSD, 1e-4, "反推刊例要与已知值一致")

	// 模板一那条路径的成本（与 cost_test.go 里那组用例同一公式）。
	template1Cost := listUSD * rate * (upstream / DiscountBaseFactor)
	require.NotNil(t, got[0].UpstreamCostCNY)
	assert.InDelta(t, template1Cost, *got[0].UpstreamCostCNY, 1e-4,
		"模板二反推的成本必须等于模板一正算的成本——两条路径同源")
}

// TestSimpleBillCostMissingGroupRatio 缺 group_ratio 的行留空，不按 0 算。
//
// 按 0 算会让刊例变成无穷大；按 quota 算（等于假定倍率 1）会得到一个看似合理
// 但凭空的数。两种都比留空坏——留空至少看得出来没算。
func TestSimpleBillCostMissingGroupRatio(t *testing.T) {
	headers := simpleCostLogHeaders()
	rows := [][]string{
		{"m1", "Codex", "1", "1", "45000", `{"group_ratio":0.4}`, "2", "101"},
		// 这一行没有 group_ratio：整个汇总行都不写成本。
		{"m1", "Codex", "1", "1", "45000", `{}`, "2", "101"},
	}

	got, err := AggregateSimpleBill(rows, headers, SimpleBillOptions{
		CostColumns:    true,
		UpstreamRatios: map[int]float64{101: 0.4},
	})
	require.NoError(t, err)
	require.Len(t, got, 1)

	r := got[0]
	// 两行里有一行缺 group_ratio：**照样给成本**，但标出只覆盖了一半。
	// 从前这里整行留空，导致 504100 行里 392 行算不出来时整张表没有成本——
	// 那是这个 bug 的一半，另一半是预检看不见这些行（见 TestRowCostReason*）。
	require.NotNil(t, r.OfficialListUSD, "有行能算就要给数，而不是整行留空")
	require.NotNil(t, r.UpstreamCostCNY)
	assert.True(t, r.CostPartial, "只覆盖了一部分行")
	assert.Equal(t, 1, r.CostRows, "只有一行参与了反推")
	assert.Equal(t, 2, r.TotalRows)
	assert.Equal(t, 1, r.SkipReasons[string(SkipNoGroupRatio)])
	assert.InDelta(t, 45000, r.SkippedQuota, 1e-9, "漏掉的那行净额度要报出来")
	// 金额照写：它不需要 group_ratio。
	assert.InDelta(t, 0.18, r.TotalCostCNY, 1e-9)
}

// TestSimpleBillCostMissingUpstreamRatio 渠道没维护倍率时成本留空，
// 而不是按 1 算（会得到「成本 = 刊例」，看着像上游零利润）或按 0 算（上游免费）。
func TestSimpleBillCostMissingUpstreamRatio(t *testing.T) {
	headers := simpleCostLogHeaders()
	rows := [][]string{
		{"m1", "Codex", "1", "1", "45000", `{"group_ratio":0.4}`, "2", "101"},
	}

	got, err := AggregateSimpleBill(rows, headers, SimpleBillOptions{
		CostColumns:    true,
		UpstreamRatios: map[int]float64{}, // 一条倍率都没维护
	})
	require.NoError(t, err)
	require.Len(t, got, 1)

	r := got[0]
	// 一行都没算出来：**一个数都不给**。这时报 0 会被读成"上游免费"，
	// 利润虚高——那是成本核算最不能出的错（与"部分覆盖"是两种状态）。
	assert.Nil(t, r.OfficialListUSD)
	assert.Nil(t, r.UpstreamCostCNY)
	assert.Nil(t, r.ProfitCNY)
	assert.Equal(t, 0, r.CostRows)
	assert.Equal(t, 1, r.SkipReasons[string(SkipNoUpstreamRatio)])
	assert.False(t, r.CostPartial, "一行都没算出来时不算 partial，是整体缺")
}

// TestSimpleBillCostSpansChannels 一个 (分组, 模型) 横跨多个渠道时，成本逐行加权。
//
// 实测 AZ/gpt-5.4 走了 4 个渠道。若拿汇总刊例去乘某一个渠道的倍率，成本会整体偏掉，
// 而表面上完全看不出来——这是本方案里最容易写错、也最难发现的一处。
func TestSimpleBillCostSpansChannels(t *testing.T) {
	headers := simpleCostLogHeaders()
	// 两个渠道、上游倍率不同（0.4 与 1.8）：
	//	行1 刊例 45000/0.4/5e5   = 0.225 USD → 成本 0.225 × 7 × 0.4/7 = 0.09
	//	行2 刊例 202500/1.8/5e5 = 0.225 USD → 成本 0.225 × 7 × 1.8/7 = 0.405
	rows := [][]string{
		{"gpt-5.4", "AZ", "1", "1", "45000", `{"group_ratio":0.4}`, "2", "849"},
		{"gpt-5.4", "AZ", "1", "1", "202500", `{"group_ratio":1.8}`, "2", "1108"},
	}

	got, err := AggregateSimpleBill(rows, headers, SimpleBillOptions{
		CostColumns:    true,
		UpstreamRatios: map[int]float64{849: 0.4, 1108: 1.8},
		ExchangeRate:   7,
	})
	require.NoError(t, err)
	require.Len(t, got, 1)

	r := got[0]
	require.NotNil(t, r.UpstreamCostCNY)
	assert.InDelta(t, 0.495, *r.UpstreamCostCNY, 1e-4,
		"两行刊例相同、上游倍率不同，成本必须逐行加权（0.09 + 0.405）")
	assert.InDelta(t, 0.45, *r.OfficialListUSD, 1e-9, "刊例 = 0.225 + 0.225")

	// 若误用「汇总刊例 × 某一行的倍率」，会得到 0.18 或 0.81 这类数，都不是 0.495。
	// 上面那条断言就是这个错误的哨兵。
}

// TestSimpleBillCostRefundAlsoOffsetsCost 退款行要按同一方向冲抵成本。
//
// 只冲金额不冲成本的话，同一条退款会让利润凭空变高——而退款恰恰发生在
// 「这个任务白跑了」的时候，那时的利润本来就该更低。
func TestSimpleBillCostRefundAlsoOffsetsCost(t *testing.T) {
	headers := simpleCostLogHeaders()
	rows := [][]string{
		{"m1", "Codex", "1000", "100", "450000", `{"group_ratio":0.4}`, "2", "101"},
		{"m1", "Codex", "0", "0", "45000", `{"task_id":7,"group_ratio":0.4}`, "6", "101"},
	}

	got, err := AggregateSimpleBill(rows, headers, SimpleBillOptions{
		CostColumns:    true,
		UpstreamRatios: map[int]float64{101: 0.4},
		ExchangeRate:   7,
	})
	require.NoError(t, err)
	require.Len(t, got, 1)

	r := got[0]
	assert.InDelta(t, 0.81, r.TotalCostCNY, 1e-9, "金额已冲抵：405000/500000")
	require.NotNil(t, r.OfficialListUSD)
	// 净额 405000 ÷ 0.4 ÷ 5e5 = 2.025 USD
	assert.InDelta(t, 2.025, *r.OfficialListUSD, 1e-9,
		"刊例按净额反推：退款冲抵后是 405000，不是 450000")
}

// TestSimpleBillCostDisabledLeavesColumnsEmpty 关掉成本核算时三列为空，
// 但金额、次数、token 一切照旧。
func TestSimpleBillCostDisabledLeavesColumnsEmpty(t *testing.T) {
	headers := simpleCostLogHeaders()
	rows := [][]string{
		{"m1", "Codex", "1", "1", "45000", `{"group_ratio":0.4}`, "2", "101"},
	}

	got, err := AggregateSimpleBill(rows, headers, SimpleBillOptions{
		// 倍率齐全但开关关着——成本列必须仍是空的，否则这个开关就没意义了。
		CostColumns:    false,
		UpstreamRatios: map[int]float64{101: 0.4},
	})
	require.NoError(t, err)
	require.Len(t, got, 1)

	assert.Nil(t, got[0].OfficialListUSD)
	assert.Nil(t, got[0].UpstreamCostCNY)
	assert.Nil(t, got[0].ProfitCNY)
	assert.False(t, got[0].CostPartial, "没开成本核算时不算「部分覆盖」")
	assert.Zero(t, got[0].CostRows)
	assert.InDelta(t, 0.09, got[0].TotalCostCNY, 1e-9)
}

// TestSimpleBillCostFromOtherField 日志没有 channel_id 列时回退解析
// other.admin_info.use_channel——手工用 SQL 导出的日志只有这一个来源。
func TestSimpleBillCostFromOtherField(t *testing.T) {
	headers := []string{"model_name", "group", "prompt_tokens", "completion_tokens", "quota", "other", "type"}
	rows := [][]string{
		{"m1", "Codex", "1", "1", "45000",
			`{"group_ratio":0.4,"admin_info":{"use_channel":["1108"]}}`, "2"},
	}

	got, err := AggregateSimpleBill(rows, headers, SimpleBillOptions{
		CostColumns:    true,
		UpstreamRatios: map[int]float64{1108: 0.4},
	})
	require.NoError(t, err)
	require.Len(t, got, 1)

	assert.NotNil(t, got[0].OfficialListUSD, "没有 channel_id 列也要能从 other 回退取到渠道号")
	require.NotNil(t, got[0].UpstreamCostCNY)
	assert.InDelta(t, 0.09, *got[0].UpstreamCostCNY, 1e-4)
}

// TestSimpleBillCostMultiChannelRowSkipped 一行经多个渠道时成本留空。
//
// 日志没说清额度怎么分摊到各渠道上，任何分摊方式都站得住——那种「看似精确」的成本
// 比留空更坏。宁可报「这一行没算」，也不要报一个编出来的数。
func TestSimpleBillCostMultiChannelRowSkipped(t *testing.T) {
	headers := simpleCostLogHeaders()
	rows := [][]string{
		{"m1", "Codex", "1", "1", "45000", `{"group_ratio":0.4}`, "2", "101,102"},
	}

	got, err := AggregateSimpleBill(rows, headers, SimpleBillOptions{
		CostColumns:    true,
		UpstreamRatios: map[int]float64{101: 0.4, 102: 0.4},
	})
	require.NoError(t, err)
	require.Len(t, got, 1)

	assert.Nil(t, got[0].UpstreamCostCNY, "多渠道路由的行不猜分摊，一行都没算出来就没有成本")
	assert.Equal(t, 1, got[0].SkipReasons[string(SkipMultiChannel)])
}

// TestSumSimpleBillCostPartialGivesTotals 部分行有成本时**照样给合计**，并把覆盖率报出来。
//
// 这条改的是「全有或全无」的老取舍：从前只要有一行算不出成本，合计就是 nil，
// 于是 504100 行里 392 行算不出来时整张表没有成本。现在给数 + 说明覆盖范围——
// 拿 99.9% 的行算出来的成本远比一片空白有用，只要不说成整体毛利就行。
func TestSumSimpleBillCostPartialGivesTotals(t *testing.T) {
	official, cost, profit := 1.8, 0.72, 1.08
	rows := []SimpleBillRow{
		{Group: "A", Model: "m1", TotalCostCNY: 3.24,
			OfficialListUSD: &official, UpstreamCostCNY: &cost, ProfitCNY: &profit,
			CostRows: 15, TotalRows: 15},
		// 这一行没有成本（例如渠道没维护倍率）
		{Group: "A", Model: "m2", TotalCostCNY: 1.0,
			CostRows: 0, TotalRows: 2, SkippedQuota: 500000,
			SkipReasons: map[string]int{string(SkipNoUpstreamRatio): 2}},
	}

	totals := SumSimpleBill(rows)
	require.NotNil(t, totals.OfficialListUSD, "部分覆盖也要给合计")
	assert.InDelta(t, 0.72, *totals.UpstreamCostCNY, 1e-9)
	// 但要如实报出覆盖情况，让调用方能说清「利润只覆盖了 15/17 行」。
	assert.Equal(t, 15, totals.Cost.Rows)
	assert.Equal(t, 17, totals.Cost.TotalRows)
	assert.Equal(t, 2, totals.Cost.SkipReasons[string(SkipNoUpstreamRatio)])
	assert.Equal(t, 2, totals.Cost.SkippedRows())
	assert.False(t, totals.Cost.Complete(), "覆盖不全，页面必须说明")
}

// TestSumSimpleBillCostComplete 全部行都有成本时合计给全，且利润口径是
// 「参与核算的金额 − 成本」，不是「全部金额 − 成本」。
func TestSumSimpleBillCostComplete(t *testing.T) {
	official, cost, profit := 1.8, 0.72, 1.08
	rows := []SimpleBillRow{
		{Group: "A", Model: "m1", TotalCostCNY: 3.24,
			OfficialListUSD: &official, UpstreamCostCNY: &cost, ProfitCNY: &profit,
			CostRows: 15, TotalRows: 15},
	}

	totals := SumSimpleBill(rows)
	require.NotNil(t, totals.UpstreamCostCNY)
	assert.InDelta(t, 0.72, *totals.UpstreamCostCNY, 1e-9)
	assert.InDelta(t, 3.24, totals.AmountCoveredCNY, 1e-9)
	assert.InDelta(t, 1.08, *totals.ProfitCNY, 1e-9)
	assert.True(t, totals.Cost.Complete())
}

// TestSimpleBillCostNoRowsNoTotal 空表不该报出一组 0 成本。
func TestSimpleBillCostNoRowsNoTotal(t *testing.T) {
	totals := SumSimpleBill(nil)
	assert.Nil(t, totals.UpstreamCostCNY, "没有行就没有成本口径，而不是「成本 0」")
	assert.Zero(t, totals.Cost.TotalRows)
}

// TestWriteSimpleBillCostColumns 成本三列写出来，合计行覆盖到利润列。
func TestWriteSimpleBillCostColumns(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "带成本.xlsx")

	official, cost, profit := 1.8, 0.72, 1.08
	rows := []SimpleBillRow{
		{Group: "Codex", Model: "gpt-5.5", HitCount: 3, TotalQuota: 900000, TotalCostCNY: 1.8,
			OfficialListUSD: &official, UpstreamCostCNY: &cost, ProfitCNY: &profit,
			CostRows: 3, TotalRows: 3},
	}
	require.NoError(t, WriteSimpleBill(path, rows, "简易账单"))

	f, err := excelize.OpenFile(path)
	require.NoError(t, err)
	defer f.Close()
	sheet := f.GetSheetName(0)

	assert.Equal(t, 10, len(SimpleBillColumns), "列数固定十列")
	assert.Equal(t, "官方刊例（美金）", SimpleBillColumns[7])
	assert.Equal(t, "上游成本（人民币）", SimpleBillColumns[8])
	assert.Equal(t, "利润（人民币）", SimpleBillColumns[9])

	assert.InDelta(t, 1.8, ToFloat(simpleCell(t, f, sheet, 8, 2)), 1e-9)
	assert.InDelta(t, 0.72, ToFloat(simpleCell(t, f, sheet, 9, 2)), 1e-9)
	assert.InDelta(t, 1.08, ToFloat(simpleCell(t, f, sheet, 10, 2)), 1e-9)

	// 合计行（第 3 行）对成本三列也写 SUM 公式。
	for _, col := range []int{8, 9, 10} {
		letter, _ := excelize.ColumnNumberToName(col)
		got, err := f.GetCellFormula(sheet, letter+"3")
		require.NoError(t, err)
		assert.Equal(t, "SUM("+letter+"2:"+letter+"2)", got, "第 %d 列合计写公式", col)
	}
}

// TestWriteSimpleBillCostPartialNoSum 有行没算出成本时，合计行不写成本列的 SUM，
// 并在表末写一行说明。
//
// SUM 会跳过空单元格，于是合计看起来是个正常数字、实际只加了有成本的那部分——
// 那比留空更糟：留空至少看得出来「没算」，一个偏小的合计看不出来。
func TestWriteSimpleBillCostPartialNoSum(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "成本不全.xlsx")

	official, cost, profit := 1.8, 0.72, 1.08
	rows := []SimpleBillRow{
		{Group: "Codex", Model: "m1", TotalQuota: 900000, TotalCostCNY: 1.8,
			OfficialListUSD: &official, UpstreamCostCNY: &cost, ProfitCNY: &profit,
			CostRows: 3, TotalRows: 3},
		{Group: "Codex", Model: "m2", TotalQuota: 100000, TotalCostCNY: 0.2,
			CostPartial: true, CostRows: 0, TotalRows: 2, SkippedQuota: 100000,
			SkipReasons: map[string]int{string(SkipNoUpstreamRatio): 2}},
	}
	require.NoError(t, WriteSimpleBill(path, rows, "简易账单"))

	f, err := excelize.OpenFile(path)
	require.NoError(t, err)
	defer f.Close()
	sheet := f.GetSheetName(0)

	// 合计行是第 4 行：金额列照写公式，成本三列不写。
	formula, err := f.GetCellFormula(sheet, "G4")
	require.NoError(t, err)
	assert.Equal(t, "SUM(G2:G3)", formula)
	for _, col := range []string{"H4", "I4", "J4"} {
		got, err := f.GetCellFormula(sheet, col)
		require.NoError(t, err)
		assert.Empty(t, got, "%s 不该写 SUM——它会跳过空值，得到一个偏小的合计", col)
	}

	// 表末说明要写清原因与去处，以及漏掉了多少额度。
	note, err := f.GetCellValue(sheet, "A5")
	require.NoError(t, err)
	assert.Contains(t, note, "2 行渠道未维护上游倍率")
	assert.Contains(t, note, "补录")
	assert.Contains(t, note, "0.2", "要写出涉及多少净额度，用户才知道这点缺口要不要紧")
}

// TestWriteSimpleBillCostNoteDistinguishesReason 表末说明按原因分开写，
// 因为「缺渠道倍率」与「缺分组倍率」要去的地方不一样。
func TestWriteSimpleBillCostNoteDistinguishesReason(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "两种原因.xlsx")

	rows := []SimpleBillRow{
		{Group: "A", Model: "m1", TotalQuota: 100, TotalCostCNY: 0.1,
			CostPartial: true, CostRows: 0, TotalRows: 1,
			SkipReasons: map[string]int{string(SkipNoUpstreamRatio): 1}},
		{Group: "A", Model: "m2", TotalQuota: 100, TotalCostCNY: 0.1,
			CostPartial: true, CostRows: 0, TotalRows: 1,
			SkipReasons: map[string]int{string(SkipNoGroupRatio): 1}},
	}
	require.NoError(t, WriteSimpleBill(path, rows, "简易账单"))

	f, err := excelize.OpenFile(path)
	require.NoError(t, err)
	defer f.Close()

	// 合计行在第 4 行（两条数据行之后），说明写在第 5 行。
	note, err := f.GetCellValue(f.GetSheetName(0), "A5")
	require.NoError(t, err)
	assert.Contains(t, note, "1 行渠道未维护上游倍率")
	assert.Contains(t, note, "1 行缺分组倍率")
}

// simpleCell 读指定格的值。excelize 对「从未写过的单元格」会报错，
// 而用例里要断言的正是「写进去了」，所以读失败就让测试失败。
func simpleCell(t *testing.T, f *excelize.File, sheet string, col, row int) string {
	t.Helper()
	axis, err := excelize.CoordinatesToCellName(col, row)
	require.NoError(t, err)
	v, err := f.GetCellValue(sheet, axis)
	require.NoError(t, err)
	return v
}

// TestSimpleBillCostSummaryLines 摘要里带上成本三项，以及覆盖率说明。
//
// 覆盖率那两行是关键：毛利率的分母只是「有成本的那部分金额」，
// 不写清楚会被当成整张账单的毛利率——那是这个功能最容易误导人的地方。
func TestSimpleBillCostSummaryLines(t *testing.T) {
	official, cost, profit := 1.8, 0.72, 1.08
	rows := []SimpleBillRow{
		{Group: "Codex", Model: "m1", HitCount: 5, TotalQuota: 900000, TotalCostCNY: 1.8,
			OfficialListUSD: &official, UpstreamCostCNY: &cost, ProfitCNY: &profit,
			CostRows: 5, TotalRows: 5},
	}
	got := FormatSimpleBillSummary(rows, SumSimpleBill(rows), 2026, 9, nil)
	assert.Contains(t, got, "官方刊例：$1.8")
	assert.Contains(t, got, "上游成本：¥0.72")
	// 毛利率 = 1.08 / 1.8 = 60%
	assert.Contains(t, got, "利润：¥1.08，毛利率 60.00%")
	assert.NotContains(t, got, "成本覆盖", "全算出来了就不用写覆盖率")

	// 部分覆盖：**照样写成本数字**，但必须跟一行覆盖率 + 原因说明。
	partial := append(rows, SimpleBillRow{
		Group: "Codex", Model: "m2", TotalCostCNY: 0.2, CostPartial: true,
		CostRows: 0, TotalRows: 392, SkippedQuota: 500000,
		SkipReasons: map[string]int{string(SkipNoChannel): 392},
	})
	got = FormatSimpleBillSummary(partial, SumSimpleBill(partial), 2026, 9, nil)
	assert.Contains(t, got, "上游成本", "有行能算就要给数，不能因为 392 行算不出来就整段省略")
	assert.Contains(t, got, "成本覆盖：5/397 行")
	assert.Contains(t, got, "未计入成本的原因：392 行日志里取不到渠道号")
}

// TestFormatSimpleBillSummaryNoCostAtAll 一行都没算出来时不给数，
// 但要说清「没算」以及为什么——静默省略会让人以为这张表本来就不含成本。
func TestFormatSimpleBillSummaryNoCostAtAll(t *testing.T) {
	rows := []SimpleBillRow{
		{Group: "Codex", Model: "m1", HitCount: 5, TotalCostCNY: 1.8,
			CostRows: 0, TotalRows: 5,
			SkipReasons: map[string]int{string(SkipNoUpstreamRatio): 5}},
	}
	got := FormatSimpleBillSummary(rows, SumSimpleBill(rows), 2026, 9, nil)
	assert.NotContains(t, got, "上游成本", "一行都没算出来时报 0 会被读成上游免费")
	assert.Contains(t, got, "成本：未能核算（5 行渠道未维护上游倍率）")
}

// TestSimpleBillPartialCostEndToEnd 端到端复现用户报的那个故障。
//
// 现场：504100 次请求、5 个汇总行，账单上却写着「成本：未能核算（392 行缺少渠道倍率
// 或分组倍率）」。两个原因叠在一起：
//
//  1. 预检只遍历「有渠道号的行」，这 392 行压根没有渠道号，于是预检放行、
//     出账时它们才被判为缺失——所以重跑再也弹不出补录界面；
//  2. 出账是「全有或全无」：392 行算不出来，整张表的成本列就全空，
//     哪怕另外 504000 行都算得出来。
//
// 这个用例把两个都钉住：成本必须给出数来，覆盖率与原因必须写在摘要里。
func TestSimpleBillPartialCostEndToEnd(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "日志查询_2026-09-01_2026-09-30_ab12cd.xlsx")
	outDir := filepath.Join(dir, "out")
	require.NoError(t, os.MkdirAll(outDir, 0o755))

	headers := []interface{}{"model_name", "group", "prompt_tokens", "completion_tokens",
		"quota", "other", "type", "channel_id", "created_at"}

	f := excelize.NewFile()
	sheet := f.GetSheetName(0)
	require.NoError(t, f.SetSheetRow(sheet, "A1", &headers))

	row := 2
	add := func(model, group string, quota int, other, ch string) {
		require.NoError(t, f.SetSheetRow(sheet, fmt.Sprintf("A%d", row), &[]interface{}{
			model, group, 1000, 100, quota, other, 2, ch, 1789430400,
		}))
		row++
	}
	// 5 个汇总行，渠道都维护了倍率 → 这些行算得出成本。
	add("opus5", "AWSB opus5", 9000000, `{"group_ratio":0.4}`, "1108")
	add("m2", "AWSB opus5", 8000000, `{"group_ratio":0.4}`, "1108")
	add("m3", "Codex", 7000000, `{"group_ratio":1.8}`, "849")
	add("m4", "Codex", 6000000, `{"group_ratio":1.8}`, "849")
	add("m5", "anti", 5000000, `{"group_ratio":1.8}`, "900")
	// 392 行没有渠道号（channel_id 为 0，other 里也没有 use_channel）——
	// 预检从前看不见它们，出账侧却把它们算作缺失。
	for i := 0; i < 392; i++ {
		add("orphan", "AWSB opus5", 1000, `{"group_ratio":0.4}`, "0")
	}
	require.NoError(t, f.SaveAs(logPath))
	require.NoError(t, f.Close())

	result, err := GenerateBill(logPath, "", "", "", outDir, Params{
		BillTemplate:  BillTemplateSimple,
		CustomerName:  "网宿科技wangsukeji",
		SummaryHeader: []string{"客户：网宿科技wangsukeji"},
		CheckCost:     true,
		ChannelUpstreamRatios: map[int]float64{
			1108: 0.4, 849: 0.4, 900: 0.4,
		},
		ChannelKnownIDs: map[int]bool{1108: true, 849: true, 900: true},
	})
	require.NoError(t, err)

	// 这一条是这个 bug 的核心：成本必须出现，而不是「未能核算」。
	assert.Contains(t, result.BillSummary, "上游成本",
		"有行算得出成本时不能因为少数行缺失就整段省略")
	assert.NotContains(t, result.BillSummary, "成本：未能核算")
	// 覆盖率与原因必须写清楚，否则读者会把利润当成整体毛利。
	assert.Contains(t, result.BillSummary, "成本覆盖：5/397 行")
	assert.Contains(t, result.BillSummary, "392 行日志里取不到渠道号")
	// 金额不受影响：它是额度本身的折算，与渠道无关。
	assert.Contains(t, result.BillSummary, "账单金额：¥")

	// 汇总行数：5 个正常 + 1 个 orphan（392 行同模型同分组合成一行）。
	assert.Contains(t, result.BillSummary, "汇总行：6 行")
}

// TestPreCheckAndCostAgreeOnSkipReason 预检与出账必须按同一套判据分类。
//
// 这是修上面那个故障的关键：两边各写一套判据，就会出现
// 「预检说都维护好了、账单说 392 行缺倍率」这种自相矛盾。
func TestPreCheckAndCostAgreeOnSkipReason(t *testing.T) {
	headers := simpleCostLogHeaders()
	rows := [][]string{
		{"m1", "Codex", "1", "1", "45000", `{"group_ratio":0.4}`, "2", "101"},
		// 没有渠道号
		{"m2", "Codex", "1", "1", "45000", `{"group_ratio":0.4}`, "2", "0"},
		// 渠道清单里没有的渠道
		{"m3", "Codex", "1", "1", "45000", `{"group_ratio":0.4}`, "2", "999"},
		// 清单里有、但没维护倍率
		{"m4", "Codex", "1", "1", "45000", `{"group_ratio":0.4}`, "2", "202"},
	}
	ratios := map[int]float64{101: 0.4}
	known := map[int]bool{101: true, 202: true}

	counts, missing, _ := CountRowCostReasons(headers, rows, ratios, known)
	assert.Equal(t, 1, counts[SkipNoChannel])
	assert.Equal(t, 1, counts[SkipUnknownChannel])
	assert.Equal(t, 1, counts[SkipNoUpstreamRatio])
	assert.Equal(t, 1, counts[SkipNone], "渠道 101 那行是能算的")
	// 只有「清单里有、没填倍率」的才值得提示用户去补。
	assert.Equal(t, []int{202}, missing, "渠道 999 补不了，不该出现在待补录清单里")

	// 出账侧对同一批行给出同样的分类。
	got, err := AggregateSimpleBill(rows, headers, SimpleBillOptions{
		CostColumns: true, UpstreamRatios: ratios, KnownChannels: known,
	})
	require.NoError(t, err)
	require.Len(t, got, 4)
	byModel := map[string]SimpleBillRow{}
	for _, r := range got {
		byModel[r.Model] = r
	}
	assert.Equal(t, 1, byModel["m2"].SkipReasons[string(SkipNoChannel)],
		"出账侧与预检侧必须给出同一个原因")
	assert.Equal(t, 1, byModel["m3"].SkipReasons[string(SkipUnknownChannel)])
	assert.Equal(t, 1, byModel["m4"].SkipReasons[string(SkipNoUpstreamRatio)])
	assert.Nil(t, byModel["m1"].SkipReasons, "算得出来的行不该有缺失原因")
}

// TestRowCostReasonZeroDeltaNotCounted 额度为 0 的行不算「缺成本」。
//
// 任务占位行常记 task_id + 0 额度。把它们计进缺失数会让用户看到几百行"缺失"，
// 跑去补一堆本来不影响成本的倍率。
func TestRowCostReasonZeroDeltaNotCounted(t *testing.T) {
	headers := simpleCostLogHeaders()
	rows := [][]string{
		// 有 task_id（说明是任务调整行）但没有渠道号、额度也是 0。
		{"m1", "Codex", "0", "0", "0", `{"task_id":7,"group_ratio":0.4}`, "6", "0"},
	}
	counts, _, _ := CountRowCostReasons(headers, rows, map[int]float64{}, map[int]bool{})
	assert.Equal(t, 1, counts[SkipZeroDelta])
	assert.Zero(t, counts[SkipNoChannel], "零额度行不该报成缺渠道号")
}

// TestRowCostReasonFirstUseNoChannelTable 首次使用、渠道清单还是空的时候，
// 缺倍率要报成「没维护」而不是「渠道不存在」。
//
// 报成后者的后果很实际：页面会告诉用户这个渠道补不了，而其实只要填个倍率就行。
func TestRowCostReasonFirstUseNoChannelTable(t *testing.T) {
	headers := simpleCostLogHeaders()
	rows := [][]string{
		{"m1", "Codex", "1", "1", "45000", `{"group_ratio":0.4}`, "2", "101"},
	}
	// known 为 nil = 还没拉过渠道清单。
	counts, missing, _ := CountRowCostReasons(headers, rows, map[int]float64{}, nil)
	assert.Equal(t, 1, counts[SkipNoUpstreamRatio])
	assert.Zero(t, counts[SkipUnknownChannel])
	assert.Equal(t, []int{101}, missing, "要能被提示去补录")
}

// TestDescribeSkipReasonsOrderStable 原因文案的顺序固定。
//
// map 遍历顺序随机，直接拼会让同一份结果显示成好几种样子——
// 这段文字是给人复制到聊天里的，来回变会让人以为数变了。
func TestDescribeSkipReasonsOrderStable(t *testing.T) {
	reasons := map[string]int{
		string(SkipNoGroupRatio):    1,
		string(SkipNoUpstreamRatio): 2,
		string(SkipNoChannel):       3,
		string(SkipMultiChannel):    4,
		string(SkipUnknownChannel):  5,
	}
	want := "2 行渠道未维护上游倍率、5 行渠道不在本地清单里、3 行日志里取不到渠道号、" +
		"4 行一行经多个渠道无法分摊、1 行缺分组倍率（group_ratio）"
	for i := 0; i < 10; i++ {
		assert.Equal(t, want, DescribeSkipReasons(reasons), "多跑几次确保不受 map 顺序影响")
	}
}

// TestSimpleBillCostSummaryMarginUsesCoveredAmount 利润口径用「参与核算的金额」
// 而不是全部金额：两者在有行没算成本时不相等，用总金额会算出偏小的毛利率。
func TestSimpleBillCostSummaryMarginUsesCoveredAmount(t *testing.T) {
	// 构造一个口径分歧：总金额 10.0，但参与核算的只有 2.0。
	official, cost, profit := 1.0, 1.0, 1.0
	key := func(v float64) *float64 { return &v }
	rows := []SimpleBillRow{
		{Group: "A", Model: "m1", TotalCostCNY: 2.0,
			OfficialListUSD: key(official), UpstreamCostCNY: key(cost), ProfitCNY: key(profit),
			CostRows: 1, TotalRows: 1},
	}
	totals := SumSimpleBill(rows)
	assert.InDelta(t, 2.0, totals.AmountCoveredCNY, 1e-9,
		"只有这一行有成本，覆盖金额就是它自己的金额")
	got := FormatSimpleBillSummary(rows, totals, 0, 0, nil)
	// 1.0 / 2.0 = 50%，而不是 1.0 / 2.0 之外的任何分母。
	assert.Contains(t, got, "毛利率 50.00%")
}
