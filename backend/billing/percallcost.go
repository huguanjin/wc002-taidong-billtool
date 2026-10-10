package billing

import (
	"math"
	"sort"
	"strings"
)

// 严格区分按次计费的上游成本估算。
//
// 默认的成本口径是「站内刊例 × 上游折扣」，它假定上游对这个模型按量计费。但有的渠道里
// 同一个模型既有按量、又有按次的，按次的那部分用倍率去估会偏得很离谱。
// 打开「严格区分按次计费」后：
//
//  1. 站内按次计费的行（日志 other.model_price > 0，与 priceRow 同一个判据）单独拿出来；
//  2. 这类行所在的 (渠道, 模型) 必须在 channel_model_billing 里维护过上游计费方式，
//     没维护的会像缺倍率一样拦下让用户就地补录——静默按倍率估只会得出一个看似正常的错数；
//  3. 维护成「按量」的走原来的倍率路径；维护成「按次」的，成本 = 次数 × 单次费用（人民币）。
//
// 判据只看站内：上游怎么收费日志里没有，只能由人来维护；而站内是否按次是日志自带的证据。

// 上游计费方式。
const (
	UpstreamModePerCall  = "per_call"  // 按次：成本 = 次数 × 单次费用
	UpstreamModePerToken = "per_token" // 按量：沿用上游倍率估算
)

// ChannelModelKey 上游计费方式的维护粒度：同一个模型在不同渠道上游的收费方式可以不同。
type ChannelModelKey struct {
	ChannelID int
	Model     string
}

// UpstreamBilling 一个 (渠道, 模型) 的上游计费方式。
type UpstreamBilling struct {
	Mode string
	// PerCallCNY 按次时的单次调用费用（人民币/次）。直接用人民币而不是倍率或美金：
	// 它就是实际花掉的钱，不需要再经汇率与折扣换算，也就没有换算出错的余地。
	PerCallCNY float64
}

// StrictPerCall 严格区分按次计费时算成本所需的一切。nil 表示开关关闭，
// 所有函数对 nil 都退化成原来的行为——关闭时产物必须与加这个功能之前逐位一致。
type StrictPerCall struct {
	Config map[ChannelModelKey]UpstreamBilling
	// SitePrice 模型 → 站内单次价（日志 model_price），取自本批日志的消费行。
	//
	// 任务的退款/补扣行 other 里常常没有 model_price，但它们属于同一个按次模型，
	// 判断「这是不是按次行」只能靠同批日志里这个模型的消费行。
	SitePrice map[string]float64
}

// NewStrictPerCall 建立严格模式的判定上下文；enabled 为假时返回 nil。
func NewStrictPerCall(enabled bool, cfg map[ChannelModelKey]UpstreamBilling,
	headers []string, rows [][]string) *StrictPerCall {

	if !enabled {
		return nil
	}
	s := &StrictPerCall{Config: cfg, SitePrice: map[string]float64{}}
	if s.Config == nil {
		s.Config = map[ChannelModelKey]UpstreamBilling{}
	}
	idxModel, okModel := columnIndex(headers, "model_name")
	idxOther, okOther := columnIndex(headers, "other")
	if !okModel || !okOther {
		return s
	}
	for _, row := range rows {
		other := cellAt(row, idxOther)
		if IsTaskQuotaAdjustment(other) {
			continue
		}
		if p := ParseModelPrice(other); p > 0 {
			s.SitePrice[strings.TrimSpace(cellAt(row, idxModel))] = p
		}
	}
	return s
}

// perCallState 一行在严格模式下的归类。
type perCallState int

const (
	// pcNotApplicable 站内不是按次计费（或没开严格模式）：走原来的倍率路径。
	pcNotApplicable perCallState = iota
	// pcPerCall 站内按次，且上游维护成按次：成本 = 次数 × 单次费用。
	pcPerCall
	// pcPending 站内按次，但这个 (渠道, 模型) 还没维护上游计费方式。
	pcPending
	// pcPerToken 站内按次，上游维护成按量：沿用倍率路径。
	pcPerToken
)

// classify 判定一行在严格模式下属于哪一类。adjustment 为真表示任务退款/补扣行。
func (s *StrictPerCall) classify(channelID int, model, other string, adjustment bool) (perCallState, UpstreamBilling, float64) {
	if s == nil {
		return pcNotApplicable, UpstreamBilling{}, 0
	}
	model = strings.TrimSpace(model)
	price := ParseModelPrice(other)
	if price <= 0 && adjustment {
		price = s.SitePrice[model]
	}
	if price <= 0 {
		return pcNotApplicable, UpstreamBilling{}, 0
	}
	cfg, ok := s.Config[ChannelModelKey{ChannelID: channelID, Model: model}]
	switch {
	case !ok:
		return pcPending, UpstreamBilling{}, price
	case cfg.Mode == UpstreamModePerCall:
		return pcPerCall, cfg, price
	default:
		return pcPerToken, cfg, price
	}
}

// PerCallUnits 按次行的调用次数（可带符号）。
//
// 站内计费式是 quota = model_price × QuotaPerCNY × group_ratio × n，反解 n。
// 与 priceRow 里同一套判据：消费行反推不出接近整数的正数时**不猜**，退回 1 次并报告 certain=false，
// 由调用方把这个不确定如实带出去。退款/补扣行（adjustment）不要求整数——
// 它冲抵的可能只是一部分张数，按比例折算才对。
func PerCallUnits(delta, sitePrice, groupRatio float64, adjustment bool) (units float64, certain bool) {
	if sitePrice <= 0 || groupRatio <= 0 {
		return 1, false
	}
	est := delta / (sitePrice * QuotaPerCNY * groupRatio)
	if adjustment {
		return est, true
	}
	if est >= 1 && math.Abs(est-math.Round(est)) < 1e-6 {
		return math.Round(est), true
	}
	return 1, false
}

// PlanRowCost 在 RowCostReason 之上叠加严格区分按次计费的判定。
//
// 出账与预检共用它（理由同 CostSkipReason：两边各写一套判据，就会出现
// 「预检放行、账单上却写着 N 行算不出」）。strict 为 nil 时与 RowCostReason 完全一致。
//
// 维护成按次的行**不需要渠道倍率**——它的成本不经倍率换算，要求渠道倍率只会
// 逼用户去填一个根本用不上的数。
type RowCostPlan struct {
	Reason CostSkipReason
	IDs    []int
	// PerCall 为真表示本行成本 = 次数 × FeeCNY。
	PerCall bool
	FeeCNY  float64
	// SitePrice 站内单次价（仅按次行有值）。
	SitePrice float64
	// Pending 为真时 Reason 一定是 SkipNoPerCallConfig。
	Pending bool
}

func PlanRowCost(row []string, idxChannel int, hasChannelCol bool, idxOther int, hasOtherCol bool,
	idxModel int, hasModel bool, ratios map[int]float64, delta float64, adjustment bool,
	strict *StrictPerCall) RowCostPlan {

	reason, ids := RowCostReason(row, idxChannel, hasChannelCol, idxOther, hasOtherCol, ratios, delta)
	plan := RowCostPlan{Reason: reason, IDs: ids}
	// 取不到唯一渠道的行谈不上按渠道维护上游计费方式，交回原来的判定。
	if strict == nil || !hasModel || len(ids) != 1 {
		return plan
	}
	// 没有渠道号的行 RowCostReason 已经判过了；有渠道号的才可能是按次。
	other := ""
	if hasOtherCol {
		other = cellAt(row, idxOther)
	}
	state, cfg, price := strict.classify(ids[0], cellAt(row, idxModel), other, adjustment)
	switch state {
	case pcPending:
		plan.Reason = SkipNoPerCallConfig
		plan.Pending = true
		plan.SitePrice = price
	case pcPerCall:
		plan.Reason = SkipNone
		plan.PerCall = true
		plan.FeeCNY = cfg.PerCallCNY
		plan.SitePrice = price
	}
	return plan
}

// PerCallIssue 一个待维护上游计费方式的 (渠道, 模型)：站内按次，上游怎么收费还没告诉我们。
type PerCallIssue struct {
	ChannelID   int    `json:"channelId"`
	ChannelName string `json:"channelName"`
	Model       string `json:"model"`
	// Rows 本次日志里该组合的行数；Units 其中的调用次数（站内按次口径），供估算工作量。
	Rows  int     `json:"rows"`
	Units float64 `json:"units"`
	// SitePrice 站内单次价（model_price，倍率前，人民币口径），填单次费用时的参考：
	// 上游单次费用通常低于它，高于它说明可能填错。
	SitePrice float64  `json:"sitePrice"`
	Groups    []string `json:"groups"`
	// AmountCNY 该组合在本次日志里的站内金额（净额度 ÷ QuotaPerCNY），按金额降序排，先补影响大的。
	AmountCNY float64 `json:"amountCny"`
}

// CollectPerCallIssues 扫日志，列出严格模式下尚未维护上游计费方式的 (渠道, 模型)。
//
// 单独扫一遍而不是塞进 CountRowCostReasons：那个函数的返回值被很多处依赖，
// 而这里需要的是另一种聚合（按 (渠道, 模型) 而非按渠道）。
func CollectPerCallIssues(headers []string, rows [][]string, strict *StrictPerCall) []PerCallIssue {
	if strict == nil {
		return nil
	}
	idxChannel, hasChannelCol := columnIndex(headers, "channel_id")
	idxOther, hasOtherCol := columnIndex(headers, "other")
	idxModel, hasModel := columnIndex(headers, "model_name")
	idxQuota, hasQuota := columnIndex(headers, "quota")
	idxType, hasType := columnIndex(headers, "type")
	idxGroup, hasGroup := columnIndex(headers, "group")
	if !hasModel {
		return nil
	}

	type acc struct {
		issue  *PerCallIssue
		groups map[string]bool
	}
	byKey := map[ChannelModelKey]*acc{}

	for _, row := range rows {
		if len(row) == 0 {
			continue
		}
		other := ""
		if hasOtherCol {
			other = cellAt(row, idxOther)
		}
		delta := 0.0
		if hasQuota {
			delta = ToFloat(cellAt(row, idxQuota))
		}
		adjustment := IsTaskQuotaAdjustment(other)
		if adjustment {
			logType := ""
			if hasType {
				logType = cellAt(row, idxType)
			}
			d, ok := QuotaAdjustmentDelta(logType, delta)
			if !ok {
				continue
			}
			delta = -d
		}
		plan := PlanRowCost(row, idxChannel, hasChannelCol, idxOther, hasOtherCol,
			idxModel, hasModel, nil, delta, adjustment, strict)
		if !plan.Pending {
			continue
		}
		model := strings.TrimSpace(cellAt(row, idxModel))
		key := ChannelModelKey{ChannelID: plan.IDs[0], Model: model}
		a, ok := byKey[key]
		if !ok {
			a = &acc{
				issue:  &PerCallIssue{ChannelID: key.ChannelID, Model: model, SitePrice: plan.SitePrice},
				groups: map[string]bool{},
			}
			byKey[key] = a
		}
		a.issue.AmountCNY += delta / QuotaPerCNY
		if adjustment {
			continue
		}
		a.issue.Rows++
		gr, _ := GroupRatioFromOther(other)
		units, _ := PerCallUnits(delta, plan.SitePrice, gr, false)
		a.issue.Units += units
		if hasGroup {
			if g := strings.TrimSpace(cellAt(row, idxGroup)); g != "" {
				a.groups[g] = true
			}
		}
	}

	out := make([]PerCallIssue, 0, len(byKey))
	for _, a := range byKey {
		for g := range a.groups {
			a.issue.Groups = append(a.issue.Groups, g)
		}
		sort.Strings(a.issue.Groups)
		a.issue.AmountCNY = round(a.issue.AmountCNY, MoneyDecimals)
		out = append(out, *a.issue)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].AmountCNY != out[j].AmountCNY {
			return out[i].AmountCNY > out[j].AmountCNY
		}
		if out[i].ChannelID != out[j].ChannelID {
			return out[i].ChannelID < out[j].ChannelID
		}
		return out[i].Model < out[j].Model
	})
	return out
}

// ExtractRatioChannelIDs 取需要上游倍率的渠道号：与 ExtractChannelIDs 一样，
// 但严格模式下，只出现按次行（且已维护成按次）的渠道不需要倍率，不列入。
func ExtractRatioChannelIDs(headers []string, rows [][]string, strict *StrictPerCall) ([]int, error) {
	if strict == nil {
		return ExtractChannelIDs(headers, rows)
	}
	all, err := ExtractChannelIDs(headers, rows)
	if err != nil {
		return nil, err
	}
	idxChannel, hasChannelCol := columnIndex(headers, "channel_id")
	idxOther, hasOtherCol := columnIndex(headers, "other")
	idxModel, hasModel := columnIndex(headers, "model_name")
	need := map[int]bool{}
	for _, row := range rows {
		if len(row) == 0 {
			continue
		}
		other := ""
		if hasOtherCol {
			other = cellAt(row, idxOther)
		}
		adjustment := IsTaskQuotaAdjustment(other)
		plan := PlanRowCost(row, idxChannel, hasChannelCol, idxOther, hasOtherCol,
			idxModel, hasModel, nil, 1, adjustment, strict)
		if plan.PerCall {
			continue
		}
		for _, id := range plan.IDs {
			need[id] = true
		}
	}
	out := make([]int, 0, len(all))
	for _, id := range all {
		if need[id] {
			out = append(out, id)
		}
	}
	return out, nil
}
