package billing

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
	BillingMode       string // token | per_call
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

// Params 一次出账请求的参数，对应 log_to_bill.py 的命令行参数。
type Params struct {
	Month             int     // 0 表示未指定，从日志推断
	Year              int     // 0 表示未指定，从日志推断
	Discount          *float64 // nil 表示不强制，按分组自动反推
	ExchangeRate      float64
	KeepLog           bool
	SanitizedLog      bool
	PreferPriceTable  bool
	Sheet             string
	Encoding          string
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
