package billing

// 与 log_to_bill.py 保持一致的计费常量与官方刊例价表。
const (
	CacheReadMult       = 0.1
	CacheWrite5mMult    = 1.25
	CacheWrite1hMult    = 2.0
	DefaultExchangeRate = 7.0
	QuotaPerCNY         = 500_000.0
	MoneyDecimals       = 4
	DiscountDecimals    = 3
	// DiscountBaseFactor billtool 自有的"分组倍率→折扣"换算基数，与 new-api 本身的倍率算法无关：
	// 分组倍率 1 对应折扣 1/DiscountBaseFactor，详见 group_ratio_source.md。
	DiscountBaseFactor = 7.0
	// ExcelMaxRowsPerSheet 是 xlsx 格式规定的单个 sheet 最大行数（含表头），超出需拆分到多个 sheet。
	ExcelMaxRowsPerSheet = 1_048_576
)

// CacheReadAbsUSD 缓存读取绝对价（$/MTok）；未列出的模型用 input×CacheReadMult。
var CacheReadAbsUSD = map[string]float64{
	"gpt-4o": 1.25,
}

// TierPrice 阶梯计费配置：按单条请求 prompt_tokens 长度选择低/高档单价。
type TierPrice struct {
	Threshold float64
	Op        string     // "lt" | "le"
	Low       [3]float64 // input, output, cache_read $/MTok
	High      [3]float64
}

// TieredModelPrices 是「表达式跑不通时」的兜底价表，只用于 aggregate.go 的降级路径
// 与 excel_write.go 的展示兜底。判档一律以表达式为准，这里不要把阈值当成权威口径：
// 上游各模型的比较符并不统一（gpt-5.4/gpt-5.5/gpt-6-astra 是 <=，gemini-3.1-pro-preview
// 是 <=，gpt-5.6-sol 是 <），照表达式抄即可，不要自己改写成统一写法。
var TieredModelPrices = map[string]TierPrice{
	"gpt-5.4": {
		Threshold: 272_000, Op: "lt",
		Low: [3]float64{2.5, 15.0, 0.25}, High: [3]float64{5.0, 22.5, 0.5},
	},
	"gpt-5.5": {
		Threshold: 272_000, Op: "lt",
		Low: [3]float64{5.0, 30.0, 0.5}, High: [3]float64{10.0, 45.0, 1.0},
	},
	"gemini-3.1-pro-preview": {
		Threshold: 200_000, Op: "le",
		Low: [3]float64{2.0, 12.0, 0.2}, High: [3]float64{4.0, 18.0, 0.4},
	},
	// gpt-6-astra：len <= 272000 ? tier("base", p*10 + c*50 + cr*1 + cc*12.5)
	//                          : tier("tier_2", p*20 + c*75 + cr*2 + cc*25)
	"gpt-6-astra": {
		Threshold: 272_000, Op: "le",
		Low: [3]float64{10.0, 50.0, 1.0}, High: [3]float64{20.0, 75.0, 2.0},
	},
	// gpt-5.6-sol：len < 272000 ? tier("tier_1", p*4 + c*20 + cr*0.4 + cc*5)
	//                         : tier("tier_2", p*8 + c*30 + cr*0.8 + cc*10)
	"gpt-5.6-sol": {
		Threshold: 272_000, Op: "lt",
		Low: [3]float64{4.0, 20.0, 0.4}, High: [3]float64{8.0, 30.0, 0.8},
	},
}

// PriceUSD 官方 (input, output) $/MTok 报价。
type PriceUSD struct{ Input, Output float64 }

var OfficialGeminiTextPrices = map[string]PriceUSD{
	"gemini-3.5-flash":        {1.5, 9.0},
	"gemini-3.6-flash":        {1.5, 7.5},
	"gemini-2.5-flash":        {0.3, 2.5},
	"gemini-2.5-pro":          {1.25, 10.0},
	"gemini-3-flash-preview":  {0.5, 3.0},
	"gemini-3.1-pro-preview":  {2.0, 12.0},
}

var OfficialImageTokenPrices = map[string]PriceUSD{
	"gemini-3-pro-image-preview":           {2.0, 120.0},
	"gemini-3-pro-image-preview-token":     {2.0, 120.0},
	"gemini-3-pro-image":                   {2.0, 120.0},
	"gemini-3.1-flash-image-preview":       {0.5, 60.0},
	"gemini-3.1-flash-image-preview-token": {0.5, 60.0},
	"gemini-3.1-flash-image":               {0.5, 60.0},
	"gpt-image-2":                          {5.0, 30.0},
}

var OfficialKimiPrices = map[string]PriceUSD{
	"kimi-k3": {3.0, 15.0},
}

var OfficialAnthropicPrices = map[string]PriceUSD{
	"claude-fable-5":            {10.0, 50.0},
	"claude-mythos-5":           {10.0, 50.0},
	"claude-opus-5":             {5.0, 25.0},
	"claude-opus-4-8":           {5.0, 25.0},
	"claude-opus-4-7":           {5.0, 25.0},
	"claude-opus-4-6":           {5.0, 25.0},
	"claude-opus-4-5":           {5.0, 25.0},
	"claude-opus-4-5-20251101":  {5.0, 25.0},
	"claude-sonnet-5":           {2.0, 10.0},
	"claude-sonnet-4-6":         {3.0, 15.0},
	"claude-sonnet-4-5":         {3.0, 15.0},
	"claude-sonnet-4-5-20250929": {3.0, 15.0},
	"claude-haiku-4-5":          {1.0, 5.0},
	"claude-haiku-4-5-20251001": {1.0, 5.0},
}

const (
	CacheCreationColumnName = "cache_creation_tokens"
	CacheTokensColumnName   = "cache_tokens"
)

// SanitizedCacheColumns 脱敏日志中展开的缓存列（删除 other 后写出）。
var SanitizedCacheColumns = []string{
	"cache_tokens",
	"cache_creation_tokens",
	"cache_creation_tokens_5m",
	"cache_creation_tokens_1h",
}

// SanitizedDetailColumns 脱敏日志在缓存列之后追加的明细列（顺序即输出顺序），
// 全部从 other 里提出来，不恢复 other 本身。
var SanitizedDetailColumns = []string{
	"uncached_input_tokens",
	"input_tokens_total",
	"usage_semantic",
	"cache_write_tokens",
	"text_input_tokens",
	"text_output_tokens",
	"audio_input_tokens",
	"audio_output_tokens",
	"image_output_tokens",
	"reasoning_tokens",
	"web_search_calls",
	"tool_surcharges",
}

// SanitizedBillingColumns 可选输出的站点内部计费参数列（需 Params.IncludeBillingParams
// 开启才会追加；默认不输出，这些字段暴露站点内部定价倍率，是否对客户可见属于商务决定）。
var SanitizedBillingColumns = []string{
	"model_ratio",
	"completion_ratio",
	"group_ratio",
	"user_group_ratio",
	"cache_ratio",
	"cache_creation_ratio",
	"cache_creation_ratio_5m",
	"cache_creation_ratio_1h",
	"model_price",
	"billing_mode",
	"matched_tier",
	"pre_consumed_quota",
	"actual_quota",
}

// SanitizedColumns 返回脱敏日志在原始列（已去除 other 与 SanitizedDropColumns）之后
// 追加的列集合：缓存列 + 明细列，includeBilling 为真时再追加计费参数列。
func SanitizedColumns(includeBilling bool) []string {
	cols := make([]string, 0, len(SanitizedCacheColumns)+len(SanitizedDetailColumns)+len(SanitizedBillingColumns))
	cols = append(cols, SanitizedCacheColumns...)
	cols = append(cols, SanitizedDetailColumns...)
	if includeBilling {
		cols = append(cols, SanitizedBillingColumns...)
	}
	return cols
}

// SanitizedDropColumns 脱敏日志整体丢弃的原始列。SanitizedDetailColumns /
// SanitizedBillingColumns 是从 other 里提出来的新列，other 本身仍整体丢弃，
// 不因新增列而改变脱敏策略。
var SanitizedDropColumns = map[string]bool{
	"other":                    true,
	"cache_tokens":             true,
	"cache_creation_tokens":    true,
	"cache_creation_tokens_5m": true,
	"cache_creation_tokens_1h": true,
}

// detailCells 按 SanitizedDetailColumns 顺序返回明细列的值：float64 表示数字、
// string 表示文本、nil 表示该字段缺失（数字留空、文本也留空，不写成 0 或空串占位）。
func detailCells(d RowDetails) []interface{} {
	return []interface{}{
		d.UncachedInputTokens,
		ptrCell(d.InputTokensTotal),
		strCell(d.UsageSemantic),
		ptrCell(d.CacheWriteTokens),
		ptrCell(d.TextInput),
		ptrCell(d.TextOutput),
		ptrCell(d.AudioInput),
		ptrCell(d.AudioOutput),
		ptrCell(d.ImageOutput),
		ptrCell(d.ReasoningTokens),
		d.WebSearchCalls,
		strCell(d.ToolSurcharges),
	}
}

// billingCells 按 SanitizedBillingColumns 顺序返回计费参数列的值，语义同 detailCells。
func billingCells(b BillingDetails) []interface{} {
	return []interface{}{
		ptrCell(b.ModelRatio),
		ptrCell(b.CompletionRatio),
		ptrCell(b.GroupRatio),
		ptrCell(b.UserGroupRatio),
		ptrCell(b.CacheRatio),
		ptrCell(b.CacheCreationRatio),
		ptrCell(b.CacheCreationRatio5m),
		ptrCell(b.CacheCreationRatio1h),
		ptrCell(b.ModelPrice),
		strCell(b.BillingMode),
		strCell(b.MatchedTier),
		ptrCell(b.PreConsumedQuota),
		ptrCell(b.ActualQuota),
	}
}

func ptrCell(v *float64) interface{} {
	if v == nil {
		return nil
	}
	return *v
}

func strCell(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

const (
	AccountingFmt = `_ * #,##0.00_ ;_ * \-#,##0.00_ ;_ * "-"??_ ;_ @_ `
	MoneyCNYFmt   = "0.0000"
	DiscountFmt   = "0.000"
	MonthFmt      = "YYYY-MM"
)
