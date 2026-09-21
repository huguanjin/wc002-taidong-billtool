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

var SanitizedDropColumns = map[string]bool{
	"other":                    true,
	"cache_tokens":             true,
	"cache_creation_tokens":    true,
	"cache_creation_tokens_5m": true,
	"cache_creation_tokens_1h": true,
}

const (
	AccountingFmt = `_ * #,##0.00_ ;_ * \-#,##0.00_ ;_ * "-"??_ ;_ @_ `
	MoneyCNYFmt   = "0.0000"
	DiscountFmt   = "0.000"
	MonthFmt      = "YYYY-MM"
)
