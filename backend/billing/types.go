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
	// ListOrigin 本行 OfficialUSD 的来源（见 ListOrigin* 常量）。
	// 只有外部对标价才能当折扣反推的分母；站内公式自算出来的数字反推不出商务折扣。
	ListOrigin ListOrigin
	// ExprUnitCurrency 表达式系数的计价币种："CNY" 表示该模型在站上按人民币报价
	// （1元=1美金的充值比例，系数 p*1 就是「每百万 1 元」），"USD" 表示系数本身即美金。
	//
	// 这个标记只影响单价列与 AC 列的显示换算：账单模板的单价列表头写的是
	// 「美金/百万token」，人民币系数原样填进去，客户按美金读会虚高 7 倍，
	// 且 AC 列公式（单价×用量）会连带把总金额放大约汇率倍。
	// 聚合口径（OfficialUSD 已折算成美金、×汇率还原人民币）与它无关。
	ExprUnitCurrency string
}

// ExprUnitDivisor 表达式单价列落成「美金/百万token」要除的数：
// 人民币计价的系数除汇率，美金计价的不除。汇率缺失时按 1 处理（等于不换算），
// 宁可显示原值也不要把行弄成 0 单价。
func (a *AggRow) ExprUnitDivisor(exchangeRate float64) float64 {
	if a.ExprUnitCurrency != "CNY" || exchangeRate <= 0 {
		return 1
	}
	return exchangeRate
}

// ListOrigin 标出官方刊例（AggRow.OfficialUSD）是从哪里来的。
//
// 这个区分是「能不能反推折扣」的唯一依据，比按厂商名猜国产/海外更可靠：
// 反推的分母必须是一份与站点自身定价无关的外部对标价，否则 Σ结算/Σ总金额
// 恢复出来的只是站点自己的 group_ratio，不是商务谈定的折扣。
type ListOrigin string

const (
	// ListOriginExternal 外部对标价：内置官网价、人工维护报价表、业务库实时价、
	// 阶梯绝对价、按次固定价，或日志自带的 ratio 快照。
	// 注意 ratio 快照虽由日志给出，换算基准（ratio=1 → $2/MTok）是官方锚点，
	// 不是站内自定的公式，因此算外部对标价。
	ListOriginExternal ListOrigin = "external"
	// ListOriginExpr 站内 billing_expr 表达式自算的刊例：站点自己的定价公式。
	// 拿它当分母反推，得到的是式子里隐含的 group_ratio，与商务折扣无关。
	ListOriginExpr ListOrigin = "expr"
	// ListOriginMixed 该 (模型,分组) 行内部混用了多种来源，分母口径不统一，不可反推。
	ListOriginMixed ListOrigin = "mixed"
	// ListOriginNone 一点价都取不到。
	ListOriginNone ListOrigin = "none"
)

// mergesListOrigin 把新出现的来源并入桶上已记录的来源：
// 只有整桶来源一致且为外部对标价时，这一桶的分母口径才可信。
func mergesListOrigin(current, incoming ListOrigin) ListOrigin {
	if current == "" {
		return incoming
	}
	if current == incoming {
		return current
	}
	return ListOriginMixed
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
	// IncludeBillingParams 控制脱敏日志是否附带站点内部计费参数列（见 SanitizedBillingColumns）。
	// 默认 false：这些字段暴露内部定价倍率，是否对客户可见属于商务决定。
	IncludeBillingParams bool
	// DomesticMarkers 人工标记的国产/站内定价标识：一条一个，可以是分组标识
	// （精确匹配，如「国产模型」），也可以是模型名前缀（前缀匹配，如「doubao」）。
	// 命中的 (模型,分组) 不参与折扣反推——模型名推厂商家族覆盖不全，识别不到时
	// 会静默按海外处理、折扣悄悄算错，这里给用户一个显式兜底。
	DomesticMarkers []string
}

// RowDetails 脱敏日志需要额外展开的单行明细，全部来自日志 other 字段
// 或聚合循环已算出的计费口径值。指针字段为 nil 表示「该日志没有这个字段」，
// 写出时留空，不要写成 0；UncachedInputTokens / WebSearchCalls 是计费口径值，
// 与现有 4 个缓存列行为一致，始终写数字（含 0）。
type RowDetails struct {
	UncachedInputTokens float64
	UsageSemantic       string
	InputTokensTotal    *float64
	CacheWriteTokens    *float64
	TextInput           *float64
	TextOutput          *float64
	AudioInput          *float64
	AudioOutput         *float64
	ImageOutput         *float64
	ReasoningTokens     *float64
	WebSearchCalls      float64
	ToolSurcharges      string
	// Billing 仅当 Params.IncludeBillingParams 为真时才会被填充。
	Billing BillingDetails
}

// BillingDetails 站点内部计费参数（可选输出，默认关闭，见 SanitizedBillingColumns）。
type BillingDetails struct {
	ModelRatio           *float64
	CompletionRatio      *float64
	GroupRatio           *float64
	UserGroupRatio       *float64
	CacheRatio           *float64
	CacheCreationRatio   *float64
	CacheCreationRatio5m *float64
	CacheCreationRatio1h *float64
	ModelPrice           *float64
	BillingMode          string
	MatchedTier          string
	PreConsumedQuota     *float64
	ActualQuota          *float64
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
