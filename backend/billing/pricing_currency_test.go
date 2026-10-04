package billing

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/xuri/excelize/v2"
)

// TestLoadPriceTableDiscounts 只读报价表折扣 sheet（第二个 sheet），不加载模型单价。
func TestLoadPriceTableDiscounts(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "price_table.xlsx")
	f := excelize.NewFile()
	sheet1 := f.GetSheetName(0)
	for r := 1; r <= 7; r++ {
		require.NoError(t, f.SetSheetRow(sheet1, cellRef(r), &[]interface{}{"", "占位"}))
	}
	sheet2, err := f.NewSheet("折扣")
	require.NoError(t, err)
	f.SetActiveSheet(sheet2)
	for r := 1; r <= 7; r++ {
		require.NoError(t, f.SetSheetRow("折扣", cellRef(r), &[]interface{}{"", "", "", "", "表头占位", "", "待定"}))
	}
	require.NoError(t, f.SetSheetRow("折扣", "A8", &[]interface{}{"", "", "", "", "DeepSeek", "第三方部署", "6折"}))
	require.NoError(t, f.SaveAs(path))

	discounts, err := LoadPriceTableDiscounts(path)
	require.NoError(t, err)
	assert.Equal(t, 0.6, discounts["DeepSeek"])

	// 不存在的文件：不报错，返回空 map（db 价格源模式下价表是可选的）。
	empty, err := LoadPriceTableDiscounts(filepath.Join(dir, "not-exist.xlsx"))
	require.NoError(t, err)
	assert.Empty(t, empty)
}

// TestAggregateFromRowsScalesDomesticVendorExprToCNY 国产供应商家族（VendorFamily 能识别的
// DeepSeek/GLM/Minimax/可灵/Kimi/Qwen）模型，billing_expr 系数本来就是人民币，不是美元；
// 聚合时必须把结果除回 exchangeRate，否则下游 OfficialListCNY 再乘一次汇率会把刊例放大。
// 非国产供应商家族模型不受影响，仍按系数本身就是美元处理。
func TestAggregateFromRowsScalesDomesticVendorExprToCNY(t *testing.T) {
	headers := []string{"model_name", "group", "prompt_tokens", "completion_tokens", "quota", "other", "created_at"}
	const exchangeRate = 7.0
	const expr = `p * 1 + c * 4` // 1M prompt + 1M completion => 1*1 + 4*1 = 5（系数单位是 $/MTok 或 ¥/MTok）

	t.Run("国产供应商家族按人民币处理", func(t *testing.T) {
		rows := [][]string{{"deepseek-test", "国产模型", "1000000", "1000000", "100", "", "1755000000"}}
		setting := &BillingExprSetting{Exprs: map[string]string{"deepseek-test": expr}}
		result, err := AggregateFromRows(rows, headers, nil, exchangeRate, false, setting, false, nil)
		require.NoError(t, err)
		require.Len(t, result.Rows, 1)
		assert.InDelta(t, 5.0/exchangeRate, result.Rows[0].OfficialUSD, 1e-9,
			"国产供应商家族的表达式结果应先除回汇率，避免换回人民币时被多乘一次")
	})

	t.Run("非国产供应商家族仍按美元处理", func(t *testing.T) {
		rows := [][]string{{"gpt-test", "海外", "1000000", "1000000", "100", "", "1755000000"}}
		setting := &BillingExprSetting{Exprs: map[string]string{"gpt-test": expr}}
		result, err := AggregateFromRows(rows, headers, nil, exchangeRate, false, setting, false, nil)
		require.NoError(t, err)
		require.Len(t, result.Rows, 1)
		assert.InDelta(t, 5.0, result.Rows[0].OfficialUSD, 1e-9)
	})
}

// TestLoadDBPriceCacheMarksDomesticVendorCurrency LoadDBPriceCache 对国产供应商家族模型
// （ModelRatio/CompletionRatio 换算出来的价格本来就是人民币）要标 Currency: "CNY"，
// 否则 ResolvePrice 不会做汇率折算，刊例会被当成美元直接使用。
func TestLoadDBPriceCacheMarksDomesticVendorCurrency(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "db_price_cache.json")
	content := `{
		"fetchedAt": "2026-09-16T00:00:00Z",
		"prices": {
			"kimi-k3": {"inputPerM": 20, "outputPerM": 100},
			"claude-sonnet-5": {"inputPerM": 2, "outputPerM": 10}
		}
	}`
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	book, _, _, err := LoadDBPriceCache(path)
	require.NoError(t, err)
	assert.Equal(t, "CNY", book.ByModel["kimi-k3"].Currency, "Kimi 属于国产供应商家族，应标人民币")
	assert.Equal(t, "USD", book.ByModel["claude-sonnet-5"].Currency, "非国产供应商家族模型保持美元")
}

// TestGenerateBillMergesPriceTableDiscountsUnderDBSource 覆盖本次修复的核心场景：
// PriceSource=db 时，price_table.xlsx 的厂商家族折扣 sheet 也必须生效，
// 分组不应整组掉进「Σ结算/Σ总金额」反推（对走 billing_expr 的国产模型反推并不可靠）。
func TestGenerateBillMergesPriceTableDiscountsUnderDBSource(t *testing.T) {
	dir := t.TempDir()
	templatePath := filepath.Join(dir, "bill_template.xlsx")
	priceTablePath := filepath.Join(dir, "price_table.xlsx")
	dbCachePath := filepath.Join(dir, "db_price_cache.json")
	logPath := filepath.Join(dir, "9月日志.xlsx")
	outDir := filepath.Join(dir, "out")
	require.NoError(t, os.MkdirAll(outDir, 0o755))

	buildFixtureTemplate(t, templatePath)

	// 只给折扣 sheet，不给第一个价格 sheet（模拟真实 price_table.xlsx 里「海外模型价格」
	// 与「国产模型」折扣分属两个 sheet 的结构）。
	f := excelize.NewFile()
	sheet1 := f.GetSheetName(0)
	for r := 1; r <= 7; r++ {
		require.NoError(t, f.SetSheetRow(sheet1, cellRef(r), &[]interface{}{"", "占位"}))
	}
	sheet2, err := f.NewSheet("国产模型")
	require.NoError(t, err)
	f.SetActiveSheet(sheet2)
	for r := 1; r <= 7; r++ {
		require.NoError(t, f.SetSheetRow("国产模型", cellRef(r), &[]interface{}{"", "", "", "", "表头占位", "", "待定"}))
	}
	require.NoError(t, f.SetSheetRow("国产模型", "A8", &[]interface{}{"", "", "", "", "DeepSeek", "第三方部署", "6折"}))
	require.NoError(t, f.SaveAs(priceTablePath))

	dbCacheContent := `{
		"fetchedAt": "2026-09-16T00:00:00Z",
		"prices": {},
		"billingMode": {"deepseek-v4.1-flash": "tiered_expr"},
		"billingExpr": {"deepseek-v4.1-flash": "p * 1 + c * 4 + cr * 0.02"}
	}`
	require.NoError(t, os.WriteFile(dbCachePath, []byte(dbCacheContent), 0o644))

	xf := excelize.NewFile()
	sheet := xf.GetSheetName(0)
	headers := []interface{}{"model_name", "group", "prompt_tokens", "completion_tokens", "quota", "other", "created_at"}
	require.NoError(t, xf.SetSheetRow(sheet, "A1", &headers))
	require.NoError(t, xf.SetSheetRow(sheet, "A2", &[]interface{}{"deepseek-v4.1-flash", "国产模型", 1_000_000, 200_000, 50_000, "", 1_755_000_000}))
	require.NoError(t, xf.SaveAs(logPath))

	result, err := GenerateBill(logPath, templatePath, priceTablePath, dbCachePath, outDir, Params{
		ExchangeRate: 7,
		PriceSource:  PriceSourceDB,
		SanitizedLog: false,
	})
	require.NoError(t, err)
	require.Len(t, result.Summary.Rows, 1)
	assert.Equal(t, 0.6, result.Summary.Rows[0].Discount,
		"db 价格源下也应采用价表里的 DeepSeek=6折，而不是反推值")
}

// TestIsOpenAIOrGeminiModelRecognizesDomesticVendors 国产供应商家族
// （DeepSeek/GLM/Minimax/Qwen，Kimi 已覆盖）走的都是 OpenAI 兼容接口，
// prompt_tokens 含缓存部分，必须按 openai 语义扣减，不能落到 anthropic 兜底分支
// （兜底会把整段 prompt 都当未命中，相当于把缓存部分重复计了一次价）。
func TestIsOpenAIOrGeminiModelRecognizesDomesticVendors(t *testing.T) {
	for _, model := range []string{"deepseek-v4.1-flash", "glm-5", "chatglm-pro", "minimax-m2.5", "qwen3.8-max", "kimi-k3", "gpt-5.4", "gemini-2.5-pro"} {
		assert.True(t, IsOpenAIOrGeminiModel(model), "%s 应识别为 openai 语义", model)
		assert.Equal(t, "openai", InferUsageSemantic(model, ""), "%s 应推断为 openai 语义", model)
	}
	assert.False(t, IsOpenAIOrGeminiModel("claude-sonnet-5"))
}

// TestAggregateFromRowsSubtractsCacheForDomesticVendorModel 覆盖本次修复：国产供应商家族
// 模型（如 deepseek-v4.1-flash）的 uncached 必须扣减 cache_tokens，不能按 anthropic 语义
// 直接取整段 prompt_tokens——否则缓存命中的 token 会被同时计入"未命中"与"缓存读取"两档，
// 把官方刊例严重高估（本次真实客户日志里曾因此导致刊例虚高约 5 倍）。
func TestAggregateFromRowsSubtractsCacheForDomesticVendorModel(t *testing.T) {
	headers := []string{"model_name", "group", "prompt_tokens", "completion_tokens", "quota", "other", "created_at"}
	other := `{"model_ratio":1,"completion_ratio":4,"cache_ratio":0.1,"cache_tokens":57216,"group_ratio":0.75}`
	rows := [][]string{{"deepseek-v4.1-flash", "国产模型", "57945", "914", "7580", other, "1755000000"}}

	result, err := AggregateFromRows(rows, headers, nil, 7.0, false, nil, false, nil)
	require.NoError(t, err)
	require.Len(t, result.Rows, 1)
	agg := result.Rows[0]
	assert.Equal(t, 729.0, agg.Uncached, "uncached 应为 prompt_tokens 扣减 cache_tokens 后的值")
	assert.Equal(t, 57216.0, agg.CacheRead)
	// 官方刊例（未计入 group_ratio）应与真实 quota 换算的口径同数量级：
	// 真实结算 quota/500000/group_ratio ≈ 0.75/0.75=0.0202（CNY，未打折前）。
	assert.InDelta(t, 0.0202, OfficialListCNY(agg, 7.0), 0.001)
}

