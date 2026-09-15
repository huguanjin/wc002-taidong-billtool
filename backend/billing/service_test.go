package billing

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/xuri/excelize/v2"
)

// data/ 目录里的真实模板与报价表不在仓库里，这里构造最小可用的替身文件，
// 只为验证从「读日志→聚合→写模板」整条链路能跑通、且关键计费分支符合预期。
func buildFixtureTemplate(t *testing.T, path string) {
	t.Helper()
	f := excelize.NewFile()
	sheet := f.GetSheetName(0)
	_ = f.SetSheetRow(sheet, "A1", &[]interface{}{"账期", "模型", "分组"})
	_ = f.SetSheetRow(sheet, "A2", &[]interface{}{"", "", ""})
	_ = f.SetSheetRow(sheet, "A3", &[]interface{}{"示例行，应被清空"})
	_ = f.SetSheetRow(sheet, "A4", &[]interface{}{"合计"})
	if err := f.SaveAs(path); err != nil {
		t.Fatalf("保存模板失败: %v", err)
	}
}

func buildFixturePriceTable(t *testing.T, path string) {
	t.Helper()
	f := excelize.NewFile()
	sheet1 := f.GetSheetName(0)
	for r := 1; r <= 7; r++ {
		_ = f.SetSheetRow(sheet1, cellRef(r), &[]interface{}{"", "表头占位", "", "", "", ""})
	}
	_ = f.SetSheetRow(sheet1, "A8", &[]interface{}{"", "第三方模型", "渠道A", "test-model-1", 1.0, 2.0})
	_ = f.SetSheetRow(sheet1, "A9", &[]interface{}{"", "国产模型", "渠道B", "cn-model-1", 7.0, 14.0})

	sheet2, err := f.NewSheet("折扣")
	if err != nil {
		t.Fatalf("创建折扣 sheet 失败: %v", err)
	}
	f.SetActiveSheet(sheet2)
	for r := 1; r <= 7; r++ {
		_ = f.SetSheetRow("折扣", cellRef(r), &[]interface{}{"", "", "", "", "表头占位", "", "待定"})
	}
	_ = f.SetSheetRow("折扣", "A8", &[]interface{}{"", "", "", "", "第三方模型", "", "8折"})

	if err := f.SaveAs(path); err != nil {
		t.Fatalf("保存报价表失败: %v", err)
	}
}

func cellRef(row int) string {
	axis, _ := excelize.CoordinatesToCellName(1, row)
	return axis
}

func buildFixtureLog(t *testing.T, path string) {
	t.Helper()
	f := excelize.NewFile()
	sheet := f.GetSheetName(0)
	headers := []interface{}{"model_name", "group", "prompt_tokens", "completion_tokens", "quota", "other", "created_at"}
	_ = f.SetSheetRow(sheet, "A1", &headers)

	rows := [][]interface{}{
		{"claude-sonnet-5", "default", 1_000_000, 200_000, 50_000, `{"cache_tokens":10000,"cache_creation_tokens":5000}`, 1_755_000_000},
		{"gpt-5.4", "vip", 300_000, 50_000, 20_000, `{"cache_tokens":1000}`, 1_755_000_000},
		{"test-model-1", "default", 100_000, 20_000, 5_000, "", 1_755_000_000},
		{"gemini-3-pro-image", "vip", 0, 0, 1_000, `{"model_price":0.5}`, 1_755_000_000},
	}
	for i, row := range rows {
		axis := cellRef(i + 2)
		r := row
		_ = f.SetSheetRow(sheet, axis, &r)
	}
	if err := f.SaveAs(path); err != nil {
		t.Fatalf("保存日志失败: %v", err)
	}
}

func TestGenerateBillEndToEnd(t *testing.T) {
	dir := t.TempDir()
	templatePath := filepath.Join(dir, "bill_template.xlsx")
	priceTablePath := filepath.Join(dir, "price_table.xlsx")
	logPath := filepath.Join(dir, "8月日志.xlsx")
	outDir := filepath.Join(dir, "out")
	_ = os.MkdirAll(outDir, 0o755)

	buildFixtureTemplate(t, templatePath)
	buildFixturePriceTable(t, priceTablePath)
	buildFixtureLog(t, logPath)

	result, err := GenerateBill(logPath, templatePath, priceTablePath, "", outDir, Params{
		ExchangeRate: 7,
		SanitizedLog: true,
	})
	if err != nil {
		t.Fatalf("GenerateBill 失败: %v", err)
	}

	if _, err := os.Stat(result.BillPath); err != nil {
		t.Fatalf("账单文件未生成: %v", err)
	}
	if result.SanitizedPath == "" {
		t.Fatalf("脱敏日志路径为空")
	}
	if _, err := os.Stat(result.SanitizedPath); err != nil {
		t.Fatalf("脱敏日志文件未生成: %v", err)
	}

	if result.Summary.Month != 8 {
		t.Errorf("期望账期月份从文件名推断为 8，实际 %d", result.Summary.Month)
	}
	if len(result.Summary.Rows) != 4 {
		t.Fatalf("期望汇总出 4 行 (model,group)，实际 %d", len(result.Summary.Rows))
	}
	if result.Summary.RowCount != 4 {
		t.Errorf("期望原始行数 4，实际 %d", result.Summary.RowCount)
	}

	if len(result.Summary.MissingPriceModels) != 0 {
		t.Errorf("期望无缺失定价的模型，实际 %v", result.Summary.MissingPriceModels)
	}

	var claudeRow, gptRow *RowSummary
	for i := range result.Summary.Rows {
		row := &result.Summary.Rows[i]
		switch row.Model {
		case "claude-sonnet-5":
			claudeRow = row
		case "gpt-5.4":
			gptRow = row
		}
	}
	if claudeRow == nil {
		t.Fatal("未找到 claude-sonnet-5 汇总行")
	}
	// claude: prompt_tokens 已是未命中量，不再扣减缓存
	if claudeRow.Uncached != 1_000_000 {
		t.Errorf("claude 未命中 token 期望 1000000，实际 %v", claudeRow.Uncached)
	}
	wantSettle := 50_000.0 / QuotaPerCNY
	if claudeRow.SettleCNY != round(wantSettle, MoneyDecimals) {
		t.Errorf("claude 结算人民币期望 %v，实际 %v", wantSettle, claudeRow.SettleCNY)
	}

	if gptRow == nil {
		t.Fatal("未找到 gpt-5.4 汇总行")
	}
	// gpt-5.4 走内置阶梯价表（官方价表里没有它的条目，由 TieredModelPrices 兜底），
	// 刊例由阶梯低档价算出，因此算「有价」。
	if !gptRow.HasPrice {
		t.Errorf("gpt-5.4 期望 HasPrice=true（阶梯价表兜底）")
	}
	if gptRow.ListCNY <= 0 {
		t.Errorf("gpt-5.4 期望刊例为正，实际 %v", gptRow.ListCNY)
	}
}
