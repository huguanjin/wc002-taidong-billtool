package billing

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	// 结算金额口径 = 总金额 × 结算系数（不是展示折扣）。
	// 反推结算的系数是精确商，展示折扣是它的 3 位取整值，两者有意不同：
	// 用取整值乘会引入纯显示精度造成的偏差，与业务金额无关。
	if claudeRow.ListCNY <= 0 {
		t.Fatalf("claude 刊例期望为正，实际 %v", claudeRow.ListCNY)
	}
	wantClaudeSettle := round(claudeRow.ListCNY*claudeRow.SettleFactor, MoneyDecimals)
	if claudeRow.SettleCNY != wantClaudeSettle {
		t.Errorf("claude 结算人民币期望 %v（刊例 × 结算系数），实际 %v",
			wantClaudeSettle, claudeRow.SettleCNY)
	}
	// 结算系数必须与展示折扣接近（同一笔账的两种精度），但不必相等。
	if diff := math.Abs(claudeRow.SettleFactor - claudeRow.Discount); diff > 0.001 {
		t.Errorf("结算系数 %v 与展示折扣 %v 偏离过大（差 %v）",
			claudeRow.SettleFactor, claudeRow.Discount, diff)
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

// TestOutputNamesCarryCustomerName 三类产物文件名都要能认出客户。
//
// 一次批量执行会同时产出好几个客户的账单，下载到本地 Downloads 后全叫
// 「账单_日志查询_...xlsx」，只能靠打开看才知道是谁的——文件名带上客户名是唯一的解法。
func TestOutputNamesCarryCustomerName(t *testing.T) {
	dir := t.TempDir()
	templatePath := filepath.Join(dir, "bill_template.xlsx")
	priceTablePath := filepath.Join(dir, "price_table.xlsx")
	// 文件名含「日志查询」，以便出账时被 defaultOutputName 改名为「账单」——
	// 与「导出日志明细」产物的真实形状一致（见 ExportLogFileName）。
	logPath := filepath.Join(dir, "日志查询_2026-09-01_2026-09-30_ab12cd.xlsx")
	outDir := filepath.Join(dir, "out")
	_ = os.MkdirAll(outDir, 0o755)

	buildFixtureTemplate(t, templatePath)
	buildFixturePriceTable(t, priceTablePath)
	buildFixtureLog(t, logPath)

	result, err := GenerateBill(logPath, templatePath, priceTablePath, "", outDir, Params{
		ExchangeRate: 7,
		SanitizedLog: true,
		CustomerName: "钛动",
	})
	if err != nil {
		t.Fatalf("GenerateBill 失败: %v", err)
	}

	assert.Equal(t, "账单_2026-09-01_2026-09-30_ab12cd_钛动.xlsx",
		filepath.Base(result.BillPath), "账单名末尾应带客户名")
	assert.Equal(t, "脱敏日志_2026-09-01_2026-09-30_ab12cd_钛动.xlsx",
		filepath.Base(result.SanitizedPath), "脱敏日志名末尾应带客户名")

	// 成本利润表的名字由账单名推出（账单_xxx → 成本利润_xxx），
	// 客户名放末尾才能保证这条推导仍然成立、三张表名字对齐。
	costPath := costOutputPath(result.BillPath)
	assert.Equal(t, "成本利润_2026-09-01_2026-09-30_ab12cd_钛动.xlsx",
		filepath.Base(costPath), "成本利润表名应沿用同一后缀")
}

// TestOutputNamesWithoutCustomer 没传客户名时文件名与改动前逐字节一致。
//
// 「手动上传日志」那条路径没有客户概念，凭空多出一个「_」会让老用户以为出了 bug。
func TestOutputNamesWithoutCustomer(t *testing.T) {
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

	// SanitizedFormat 留空即默认 xlsx（见 sanitizedFormatInfo）。
	assert.Equal(t, "8月账单.xlsx", filepath.Base(result.BillPath))
	assert.Equal(t, "8月脱敏日志.xlsx", filepath.Base(result.SanitizedPath))
}

// TestCustomerNameSanitizedInFileName 客户名是自由文本，必须洗成合法文件名。
//
// 不洗的话 `/` 会让 filepath.Join 把文件写到别的目录（甚至逃出 job 目录），
// Windows 上 `:` `*` `?` 则直接导致创建失败——两种都是执行时才炸，很难查。
func TestCustomerNameSanitizedInFileName(t *testing.T) {
	dir := t.TempDir()
	templatePath := filepath.Join(dir, "bill_template.xlsx")
	priceTablePath := filepath.Join(dir, "price_table.xlsx")
	logPath := filepath.Join(dir, "8月日志.xlsx")
	outDir := filepath.Join(dir, "out")
	_ = os.MkdirAll(outDir, 0o755)

	buildFixtureTemplate(t, templatePath)
	buildFixturePriceTable(t, priceTablePath)
	buildFixtureLog(t, logPath)

	cases := []struct {
		name     string
		customer string
		want     string
	}{
		{"路径分隔符", `ACME/华东\区`, "8月账单_ACME_华东_区.xlsx"},
		{"Windows 非法字符", `A*B?C:D"E<F>G|H`, "8月账单_A_B_C_D_E_F_G_H.xlsx"},
		{"首尾空白", "  钛动  ", "8月账单_钛动.xlsx"},
		{"末尾点号", "ACME Inc.", "8月账单_ACME Inc.xlsx"},
		{"清洗后为空", "///", "8月账单.xlsx"},
		{"超长按字符截断", strings.Repeat("客", 50), "8月账单_" + strings.Repeat("客", 40) + ".xlsx"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := GenerateBill(logPath, templatePath, priceTablePath, "", outDir,
				Params{ExchangeRate: 7, CustomerName: tc.customer})
			require.NoError(t, err)
			assert.Equal(t, tc.want, filepath.Base(result.BillPath))
			// 关键：产物必须仍然落在 job 目录里，不能被客户名里的 `/` 带出去。
			assert.Equal(t, outDir, filepath.Dir(result.BillPath))
			assert.NotContains(t, filepath.Base(result.BillPath), "/")
			assert.NotContains(t, filepath.Base(result.BillPath), "\\")
		})
	}
}
