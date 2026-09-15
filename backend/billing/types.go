package billing

import "time"

// ModelPrice 单模型单价（已换算为 USD/MTok）。
type ModelPrice struct {
	InputPerM  float64
	OutputPerM float64
	Currency   string // USD | CNY
	Source     string
	Category   string
	Channel    string
}

// AggRow 按 (模型, 分组) 汇总的一行。
type AggRow struct {
	Model        string
	Group        string
	Uncached     float64
	CacheRead    float64
	Output       float64
	CacheWrite5m float64
	CacheWrite1h float64
	Quota        float64
	Rows         int
	// 按请求累计的官方美金刊例（含阶梯价与 web_search）
	OfficialUSD float64
	WebSearchCalls float64
	// 图片按次计费的调用次数；按量图片模型此字段为 0
	ImagePerCallCount float64
	BillingMode       string // token | per_call | tiered_expr
	// BillingExpr 该模型的阶梯计费表达式（来自日志 other.expr_b64 或 options 表）；
	// 非阶梯模型为空。
	BillingExpr string
	// ExprTiers 本账期内表达式命中的档位名集合（按首次命中顺序），
	// 用于在账单里说明这行的刊例由哪几档混合而成。
	ExprTiers []string
	// LastAt 本行最后一次请求发生的时刻（日志 created_at），为零值表示日志没有该列。
	// 折算表达式单价时必须用它，而不是出账时刻——deepseek-v4.1-flash 这类
	// 表达式带 hour("Asia/Shanghai") 峰谷倍率，用 now() 会得到与账期无关的单价。
	LastAt time.Time
}

// SiteCNY 站点人民币 = quota / 500000。
func (a *AggRow) SiteCNY() float64 {
	return a.Quota / QuotaPerCNY
}

// PriceBook 报价表加载结果。
type PriceBook struct {
	ByModel   map[string]ModelPrice
	Discounts map[string]float64
}

func NewPriceBook() *PriceBook {
	return &PriceBook{ByModel: map[string]ModelPrice{}, Discounts: map[string]float64{}}
}

// PriceSource 模型单价来源：内置官方价 / 人工维护报价表(xlsx) / 业务数据库实时。
type PriceSource string

const (
	PriceSourceOfficial   PriceSource = "official"
	PriceSourcePriceTable PriceSource = "price_table"
	PriceSourceDB         PriceSource = "db"
)

// ManualPriceInput 用户在「检查模型价格覆盖」环节手动补全的单模型单价（USD/MTok）。
type ManualPriceInput struct {
	InputPerM  float64 `json:"inputPerM"`
	OutputPerM float64 `json:"outputPerM"`
}

// Params 一次出账请求的参数，对应 log_to_bill.py 的命令行参数。
type Params struct {
	Month             int     // 0 表示未指定，从日志推断
	Year              int     // 0 表示未指定，从日志推断
	Discount          *float64 // nil 表示不强制，按分组自动反推
	ExchangeRate      float64
	KeepLog           bool
	SanitizedLog      bool
	SanitizedFormat   string // "xlsx"（默认，为空时等同）| "csv" | "tsv"
	PriceSource       PriceSource // 空值等同 PriceSourceOfficial
	Sheet             string
	Encoding          string
	// ManualPrices 缺失定价模型的手动补全价格，键为日志里的原始模型名，
	// 生成账单时会合并进 PriceBook.ByModel（Source: "manual_override"）。
	ManualPrices map[string]ManualPriceInput
}

// Summary 返回给前端展示的结果摘要。
type Summary struct {
	Year               int              `json:"year"`
	Month              int              `json:"month"`
	Rows               []RowSummary     `json:"rows"`
	SettleCNYTotal     float64          `json:"settleCnyTotal"`
	ListCNYTotal       float64          `json:"listCnyTotal"`
	OverallDiscount    float64          `json:"overallDiscount"`
	MissingPriceModels []string         `json:"missingPriceModels"`
	RowCount           int              `json:"rowCount"`
	CacheHitRows       int              `json:"cacheHitRows"`
	WebSearchRows      int              `json:"webSearchRows"`
}

// RowSummary 单个 (模型, 分组) 汇总行，供前端表格展示。
type RowSummary struct {
	Model        string  `json:"model"`
	Group        string  `json:"group"`
	Uncached     float64 `json:"uncached"`
	CacheRead    float64 `json:"cacheRead"`
	Output       float64 `json:"output"`
	CacheWrite5m float64 `json:"cacheWrite5m"`
	CacheWrite1h float64 `json:"cacheWrite1h"`
	Quota        float64 `json:"quota"`
	SettleCNY    float64 `json:"settleCny"`
	ListCNY      float64 `json:"listCny"`
	Discount     float64 `json:"discount"`
	Rows         int     `json:"rows"`
	HasPrice     bool    `json:"hasPrice"`
}
