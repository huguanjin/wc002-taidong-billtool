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
