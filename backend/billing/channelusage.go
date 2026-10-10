package billing

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// 本文件解决「检查这份日志用到的渠道有没有维护上游倍率」这件事，
// 并把它按**日志里实际观测到的分组**归类，供页面展示。
//
// 为什么需要单独一层：分组与渠道是两个正交维度（分组决定卖给客户多少钱，
// 渠道决定上游花多少钱），倍率维护锚在 channel_id 上，而用户是按分组认知的。
// 日志每行同时给了两者，所以这个对应关系是观测到的，不是猜的——
// 不能拿 channels.channel_group 去猜（那是「该渠道能服务哪些分组」的候选集合）。

// ChannelUsage 一个渠道在本次日志里的使用情况。
type ChannelUsage struct {
	ChannelID int
	// Groups 该渠道在**本次日志**里出现过的分组名（如 AZ / Codex），升序去重。
	//
	// 取日志的 group 列原值，不用 ChannelInfo.ChannelGroup：后者是渠道能服务的
	// 候选分组集合，与实际这次落在哪个分组不是一回事。
	Groups []string
}

// ExtractChannelUsage 扫一遍日志，得出「渠道 → 出现在哪些分组」。
//
// 渠道号有两个来源，都要支持：
//  1. 日志有 channel_id 列——工具「导出日志明细」导出的日志必有（logexport.go 的必选列）；
//  2. 没有该列时回退解析 other.admin_info.use_channel——手工用 SQL 导出的日志就只有这个。
//
// 第二个返回值为 false 表示两处都取不到渠道号，即「这份日志做不了渠道检查」。
// 这与「没有渠道、无需检查」必须区分开：后者会让用户以为已经检查通过。
func ExtractChannelUsage(headers []string, rows [][]string) (map[int]*ChannelUsage, bool) {
	col := map[string]int{}
	for i, h := range headers {
		if h != "" {
			col[h] = i
		}
	}
	idxChannel, hasChannelCol := col["channel_id"]
	idxGroup, hasGroupCol := col["group"]
	idxOther, hasOtherCol := col["other"]

	usage := map[int]*ChannelUsage{}
	seen := map[[2]interface{}]bool{} // (channel, group) 去重，避免同一组合反复 append

	for _, row := range rows {
		if len(row) == 0 {
			continue
		}
		group := ""
		if hasGroupCol {
			group = strings.TrimSpace(cellAt(row, idxGroup))
		}

		ids := rowChannelIDs(row, idxChannel, hasChannelCol, idxOther, hasOtherCol)
		if len(ids) == 0 {
			continue
		}

		for _, id := range ids {
			if id <= 0 {
				continue
			}
			u, ok := usage[id]
			if !ok {
				u = &ChannelUsage{ChannelID: id}
				usage[id] = u
			}
			if group == "" {
				continue
			}
			key := [2]interface{}{id, group}
			if seen[key] {
				continue
			}
			seen[key] = true
			u.Groups = append(u.Groups, group)
		}
	}

	for _, u := range usage {
		sort.Strings(u.Groups)
	}
	return usage, len(usage) > 0
}

// rowChannelIDs 取一行日志的渠道号，两个来源按优先序回退。
//
// 抽成函数是因为**两处都要用**：渠道使用情况统计（本文件）与模板二的成本列
// （simplebill.go）。两处各写一份回退逻辑，迟早会有一处忘了回退、或者两处的
// 优先级反了——那时账单上的成本与检查界面报的渠道对不上，很难查。
//
// 顺序不能反：channel_id 列是工具「导出日志明细」写出的权威值；other 里那份
// 是站点自己记的路由过程，手工 SQL 导出的日志只有它。
func rowChannelIDs(row []string, idxChannel int, hasChannelCol bool,
	idxOther int, hasOtherCol bool) []int {

	var ids []int
	if hasChannelCol {
		// 渠道可能有多个（一个请求经多个渠道），日志用逗号分隔。
		ids = parseChannelList(cellAt(row, idxChannel))
	}
	if len(ids) == 0 && hasOtherCol {
		ids = ParseChannelIDsFromOther(cellAt(row, idxOther))
	}
	return ids
}

// ParseChannelIDsFromOther 从日志 other 里取渠道号，回退路径用。
//
// 取值路径是 other.admin_info.use_channel。[849] 与 ["849"] 两种写法都见过，
// 所以统一按「数组元素，数字或数字字符串」处理。
func ParseChannelIDsFromOther(other string) []int {
	text := strings.TrimSpace(other)
	if text == "" {
		return nil
	}
	var data map[string]interface{}
	if err := json.Unmarshal([]byte(text), &data); err != nil || data == nil {
		return nil
	}
	admin, ok := data["admin_info"].(map[string]interface{})
	if !ok {
		return nil
	}
	arr, ok := admin["use_channel"].([]interface{})
	if !ok {
		return nil
	}

	out := make([]int, 0, len(arr))
	for _, v := range arr {
		if id := int(jsonNumber(v)); id > 0 {
			out = append(out, id)
		}
	}
	return out
}

// parseChannelList 解析日志 channel_id 列里的一条。
//
// 该列是整数，但同一行可能记多个渠道（逗号分隔）；非数字片段直接跳过——
// 宁可认为这一行没有渠道号，也不要猜出一个错误的渠道号去查倍率。
func parseChannelList(v string) []int {
	s := strings.TrimSpace(v)
	if s == "" || s == "0" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if id := int(ToFloat(p)); id > 0 {
			out = append(out, id)
		}
	}
	return out
}

// CostSkipReason 一行日志没能算出成本的原因。
//
// 枚举而不是几个计数器，是因为**检查与出账必须按同一套判据**：
// 之前检查侧只遍历「有渠道号的行」，出账侧却遍历全部行，于是
// 「检查通过、成本全空」这种自相矛盾的结果就出现了——用户看到预检放行，
// 却在账单上读到「392 行缺少渠道倍率」。
type CostSkipReason string

const (
	// SkipNone 这一行能算出成本。
	SkipNone CostSkipReason = ""
	// SkipNoGroupRatio 日志缺 group_ratio，反推不出官方刊例。
	SkipNoGroupRatio CostSkipReason = "no_group_ratio"
	// SkipNoChannel 取不到渠道号（channel_id 列与 other.use_channel 都没有）。
	SkipNoChannel CostSkipReason = "no_channel"
	// SkipMultiChannel 一行经多个渠道，额度怎么分摊不明，不猜。
	SkipMultiChannel CostSkipReason = "multi_channel"
	// SkipNoUpstreamRatio 该渠道还没维护上游倍率——**唯一能靠补录解决的一种**。
	//
	// 渠道在不在本地清单里**不单列一类**：倍率表以 channel_id 为主键，与清单无关，
	// 所以两者的处置完全相同（填一个倍率）。当初拆成两类是个错误设计，
	// 直接后果是「清单里没有」的渠道被排除在待补录清单之外（CountRowCostReasons
	// 只为 SkipNoUpstreamRatio 收集渠道号），预检因此永不拦下、页面永不提示。
	// 渠道名缺失与否由 ChannelIssue.Known 表达，与"要不要补录"无关。
	SkipNoUpstreamRatio CostSkipReason = "no_upstream_ratio"
	// SkipNoPerCallConfig 严格区分按次计费时：站内按次的 (渠道, 模型) 还没维护上游计费方式。
	//
	// 和缺倍率一样靠补录解决，所以预检同样会拦；但补的是另一张表，单列一类以便页面分开提示。
	SkipNoPerCallConfig CostSkipReason = "no_percall_config"
	// SkipZeroDelta 这一行不改动额度（补扣/退款但金额为 0），对成本没有影响。
	//
	// 单列一类而不是并进上面几类：它不该被算作"缺成本"。任务行常常记
	// task_id + 0 额度当占位，把它们计进缺失数会让用户看到一个夸张的行数，
	// 去补一堆本来不影响成本的倍率。
	SkipZeroDelta CostSkipReason = "zero_delta"
)

// RowCostReason 这一行为什么算不出成本，以及它用到的渠道号。
//
// 出账与检查共用它的判据（见 CostSkipReason 的说明）。
func RowCostReason(row []string, idxChannel int, hasChannelCol bool,
	idxOther int, hasOtherCol bool, ratios map[int]float64,
	delta float64) (CostSkipReason, []int) {

	ids := rowChannelIDs(row, idxChannel, hasChannelCol, idxOther, hasOtherCol)
	switch {
	case len(ids) == 0:
		// 额度为 0 的行没有成本可言，先于「没渠道号」判定：
		// 否则任务占位行会被报成需要补录的缺失行。
		if delta == 0 {
			return SkipZeroDelta, nil
		}
		return SkipNoChannel, ids
	case len(ids) > 1:
		return SkipMultiChannel, ids
	}
	if r, ok := ratios[ids[0]]; ok && r >= 0 {
		return SkipNone, ids
	}
	// 有渠道号但没倍率——就这一种，去补倍率即可。
	//
	// **不看渠道清单**：清单里有没有这个渠道，都不影响「能不能填倍率」，
	// 因为倍率表的主键就是 channel_id（见 UpsertChannelRatios，它不校验清单）。
	// 清单只决定页面上显示不显示得出渠道名。
	return SkipNoUpstreamRatio, ids
}

// CountRowCostReasons 扫一遍日志，按原因统计算不出成本的行数，并收集缺失的渠道号。
//
// **按行统计而不是按渠道**：一个渠道可能只在大批行里出现在少数几行上，
// 只报渠道会让用户以为补了倍率就万事大吉，实际还有别的行因别的原因算不出来。
func CountRowCostReasons(headers []string, rows [][]string, ratios map[int]float64) (
	counts map[CostSkipReason]int, missingChannels []int, rowsPerChannel map[int]int) {
	return CountRowCostReasonsStrict(headers, rows, ratios, nil)
}

// CountRowCostReasonsStrict 同 CountRowCostReasons，另按「严格区分按次计费」判定：
// strict 为 nil 时两者完全一致。维护成按次的行不需要渠道倍率，不计入缺倍率。
func CountRowCostReasonsStrict(headers []string, rows [][]string, ratios map[int]float64, strict *StrictPerCall) (
	counts map[CostSkipReason]int, missingChannels []int, rowsPerChannel map[int]int) {

	col := map[string]int{}
	for i, h := range headers {
		if h != "" {
			col[h] = i
		}
	}
	idxChannel, hasChannelCol := col["channel_id"]
	idxOther, hasOtherCol := col["other"]
	idxQuota, hasQuota := col["quota"]
	idxType, hasType := col["type"]
	idxModel, hasModel := col["model_name"]

	counts = map[CostSkipReason]int{}
	rowsPerChannel = map[int]int{}
	// 缺失渠道按**渠道**去重计数，而不是按行：一个渠道可能出现在几万行里，
	// 按行报会得到「12000 行渠道未维护倍率」这种数字——用户要补的其实只有 1 个渠道，
	// 报行数只会让人以为工作量很大。
	//
	// 但每个渠道**各影响多少行**要单独留着（rowsPerChannel）：
	// 用户补录时按行数排优先级，先补影响面最大的那个。
	missingByChannel := map[int]bool{}

	for _, row := range rows {
		if len(row) == 0 {
			continue
		}
		other := ""
		if hasOtherCol {
			other = cellAt(row, idxOther)
		}

		// delta 的口径必须与 AggregateSimpleBill 完全一致（退款为负、补扣为正），
		// 否则「额度为 0 的行」在这边被算成缺失、在那边被忽略，两处又对不上。
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
			idxModel, hasModel, ratios, delta, adjustment, strict)
		reason, ids := plan.Reason, plan.IDs
		if reason == SkipNoUpstreamRatio && len(ids) == 1 {
			rowsPerChannel[ids[0]]++
			if !missingByChannel[ids[0]] {
				missingByChannel[ids[0]] = true
				counts[reason]++
				missingChannels = append(missingChannels, ids[0])
			}
			continue
		}
		counts[reason]++
	}
	sort.Ints(missingChannels)
	return counts, missingChannels, rowsPerChannel
}

// UsedChannelIDs 取 usage 里的渠道号，去重升序。
//
// 排序是为了让展示稳定：map 的遍历顺序是随机的，直接输出会让同一份日志
// 每次刷新都换一个渠道顺序，用户没法核对「上次是不是就这几条」。
func UsedChannelIDs(usage map[int]*ChannelUsage) []int {
	ids := make([]int, 0, len(usage))
	for id := range usage {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	return ids
}

// GroupChannelMap 分组名 → 该分组下用到的渠道号（升序、去重）。
//
// 页面按**分组**分节展示待补录渠道时用它：倍率锚在渠道上，用户按分组认知，
// 这张表就是两者的桥。一个渠道可能出现在多个分组下（实测 AZ 组下有
// 849/1108/721/791/774），所以是「分组 → 渠道列表」而不是反过来的映射。
func GroupChannelMap(usage map[int]*ChannelUsage) map[string][]int {
	out := map[string][]int{}
	for _, id := range UsedChannelIDs(usage) {
		for _, g := range usage[id].Groups {
			out[g] = append(out[g], id)
		}
	}
	return out
}

// LogGroupKeys 取日志里出现过的原始分组名（group 列原值），去重升序。
//
// 与 AggRow.KeyGroup 同一个取值口径——手工折扣就是按 KeyGroup 维护的，
// 所以出账前能拿它来判断「这个客户的哪些分组还没填线下折扣」。
func LogGroupKeys(headers []string, rows [][]string) []string {
	idx, ok := columnIndex(headers, "group")
	if !ok {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, row := range rows {
		if len(row) == 0 {
			continue
		}
		g := strings.TrimSpace(cellAt(row, idx))
		if g == "" || seen[g] {
			continue
		}
		seen[g] = true
		out = append(out, g)
	}
	sort.Strings(out)
	return out
}

// MissingGroupDiscounts 挑出日志里出现、但该客户还没维护线下折扣的分组。
//
// 只在「使用自定义折扣」勾选时才有意义：不勾的话手工折扣根本不参与出账，
// 缺不缺都无所谓（报了反而让人以为有问题）。
//
// 与待补录渠道同一套哲学：缺了就拦下让用户就地填，而不是一半按线下折扣、
// 一半按反推——同一张账单里两种折扣口径混着，客户核对时一定会问。
func MissingGroupDiscounts(headers []string, rows [][]string, manual map[string]float64) []string {
	var out []string
	for _, g := range LogGroupKeys(headers, rows) {
		if _, ok := manual[g]; !ok {
			out = append(out, g)
		}
	}
	return out
}

// ChannelIssue 一个待补录上游倍率的渠道，以及它在本次日志里出现在哪些分组。
type ChannelIssue struct {
	ChannelID int    `json:"channelId"`
	Name      string `json:"name"`
	// ChannelGroup 业务库 channels.group（该渠道能服务的分组候选集合）。
	// **仅作参考展示**，不参与归类——归类一律用 Groups。
	ChannelGroup string `json:"channelGroup"`
	// Groups 该渠道在本次日志里实际出现过的分组名，页面据此分节展示。
	Groups []string `json:"groups"`
	// Known 该渠道号在本地渠道清单里是否存在。
	//
	// **它不决定「能不能补录」**：倍率表以 channel_id 为主键，与渠道清单无关
	// （见 UpsertChannelRatios，它不校验清单）。从前清单里查不到的渠道不给输入框，
	// 结果是一批新上线、清单快照还没拉到的渠道永远算不出成本，而且预检不拦——
	// 用户只看到账单上一句「392 行未计入成本」，界面上却无处可填。
	Known bool `json:"known"`
	// RowCount 该渠道在本次日志里未计入成本的行数，供页面按工作量排序/提示。
	RowCount int `json:"rowCount"`

	// ---- 国模标识相关：填倍率的同时要能顺手标国模，所以待补录清单也带上这几项 ----

	// IsDomestic 该渠道当前是否已标为国模渠道。倍率还没填、但先标过国模的渠道也会是 true。
	IsDomestic bool `json:"isDomestic"`
	// Models 该渠道在本次日志里跑过的模型，按行数降序。用户靠它判断这是不是国模渠道。
	Models []string `json:"models,omitempty"`
	// AllDomesticModels / AnyDomesticModels 这些模型里按模型名能识别为国产厂商的情况，
	// 页面据此提示「看起来该标国模」。只是提示，从不自动勾选——标识直接决定成本差 7 倍，
	// 该由人来拍板。
	AllDomesticModels bool `json:"allDomesticModels"`
	AnyDomesticModels bool `json:"anyDomesticModels"`
}

// ChannelCheckResult 一次「上游倍率维护情况」检查的结果。
type ChannelCheckResult struct {
	// NeedsUpstreamReview 为真表示本次执行停在「核对上游倍率与国模标识」这一步
	// （计划勾了 ReviewUpstream），此时 ReviewChannels 是核对清单。
	//
	// 与 Missing 非空的「被拦下」是两回事：核对是用户主动要求的人工确认，
	// 哪怕所有渠道都维护好了也会停；被拦下是因为缺倍率、不补就算不了成本。
	NeedsUpstreamReview bool `json:"needsUpstreamReview,omitempty"`
	// ReviewChannels 核对清单：本次日志用到的**全部**渠道（含已维护的），
	// 按日志额度折算的金额降序——先看对成本影响最大的。
	ReviewChannels []ChannelReviewItem `json:"reviewChannels,omitempty"`
	// UsedChannels 本次日志用到的全部渠道（含已维护的），供页面展示完整清单。
	UsedChannels []UsedChannel `json:"usedChannels"`
	// Maintained 已维护倍率的渠道。
	Maintained []ChannelInfo `json:"maintained"`
	// Missing 未维护倍率、需要补录的渠道（**含渠道清单里查不到的**）。
	//
	// 从前这里只放「清单里查得到」的，清单里查不到的那批被塞进 UnknownChannelIDs
	// 并当成「补不了」。那个判断是错的：倍率表不依赖渠道清单，清单里没有的渠道
	// 照样能填倍率。改成一律进 Missing（Known 字段标明是不是清单之外），
	// 页面就能对它们一视同仁地给输入框。
	Missing []ChannelIssue `json:"missing"`
	// UnknownChannelIDs 渠道表里查不到的（多半已在业务库被硬删除）——补不了，
	// 只能如实报出，让用户知道这些渠道的成本算不全。
	UnknownChannelIDs []int `json:"unknownChannelIds"`
	// MissingPerCall 严格区分按次计费时，站内按次但尚未维护上游计费方式的 (渠道, 模型)。
	// 非空表示本次被拦下，补录后重跑即可。
	MissingPerCall []PerCallIssue `json:"missingPerCall,omitempty"`
	// MissingGroupRatioRows group_ratio 缺失、无法反推官方刊例的行数。
	// 大于 0 时这些行的成本同样算不出来（见 AggregateSimpleBill）。
	MissingGroupRatioRows int `json:"missingGroupRatioRows"`
	// UncostableRows 算不出成本的行数，按原因分类。
	//
	// 它是**唯一权威的口径**：预检拦不拦、账面上成本留不留空，都以它为准。
	// 从前预检只遍历「有渠道号的行」，而成本侧遍历全部行，于是会出现
	// 「预检放行、账单上却写着 392 行缺倍率」——两个数都自称是缺失行数。
	UncostableRows map[string]int `json:"uncostableRows,omitempty"`
	// UncostableTotal 上面那张表的总和（不含 zero_delta）。
	UncostableTotal int `json:"uncostableTotal"`
	// MissingDiscountGroups 日志里出现、但该客户还没维护线下折扣的分组。
	// 只在勾了「使用自定义折扣」时才有值；非空表示本次被拦下。
	MissingDiscountGroups []string `json:"missingDiscountGroups,omitempty"`
	// TotalRows 日志数据行数，供页面显示「检查了多少行」。
	TotalRows int `json:"totalRows"`
	// NoChannelInfo 这份日志里一个渠道号都没有——渠道检查做不了。
	// 与「没有未维护的渠道」是两回事：后者是检查通过，这个是根本没能检查。
	NoChannelInfo bool `json:"noChannelInfo"`
}

// UsedChannel 日志里用到的一个渠道 + 它的维护状态。
type UsedChannel struct {
	ChannelID     int      `json:"channelId"`
	Name          string   `json:"name"`
	ChannelGroup  string   `json:"channelGroup"`
	Groups        []string `json:"groups"`
	UpstreamRatio *float64 `json:"upstreamRatio"` // nil = 未维护
	// IsDomestic 是否国模渠道（见 UpstreamDiscountFor）。
	IsDomestic bool `json:"isDomestic"`
	// Known 该渠道号在本地渠道清单里是否存在。不存在时无法补录（业务库已删）。
	Known bool `json:"known"`
}

// ChannelObservation 一个渠道在本次日志里**被观测到的事实**，与「配置了什么」无关。
//
// 核对上游倍率时人要判断的是「这个渠道到底在跑什么」：光看渠道号与名称，
// 分不清它承接的是国产模型还是海外模型——而这两类的成本折扣差 7 倍。
// 所以把日志里能观测到的（行数、金额、分组、模型）一并给出来，让判断有依据。
type ChannelObservation struct {
	ChannelID int
	// Rows / QuotaNet 只统计「这一行只经过这一个渠道」的消费行与净额度。
	// 一行经多个渠道时额度怎么分摊日志没说（见 SkipMultiChannel），归给谁都是猜，所以不归。
	// 退款/补扣行不计入 Rows（它不是一次请求），但计入 QuotaNet（与账单的净额口径一致）。
	Rows     int
	QuotaNet float64
	// Groups / Models 该渠道出现过的分组与模型。这两项含多渠道行：
	// 「出现过」不需要分摊，只要这一行经过了它。Models 按行数降序，同数按名称升序。
	Groups []string
	Models []string
	// AllDomesticModels / AnyDomesticModels：Models 里按模型名能识别为国产厂商的情况，
	// 判据与账单里换算币种用的是同一个（VendorFamily），不另起一套。
	// Models 为空时两者都是 false。
	AllDomesticModels bool
	AnyDomesticModels bool
}

// ObserveChannels 扫一遍日志，得出每个渠道的观测事实。只读日志，不涉及任何配置。
//
// 渠道号的取法与出账、预检共用 rowChannelIDs（channel_id 列优先，回退 other.use_channel），
// 额度的符号口径与 CountRowCostReasons / AggregateSimpleBill 完全一致——
// 核对界面上看到的金额必须与之后出的账同一把尺子，否则「核对时看着对、出账又不对」。
func ObserveChannels(headers []string, rows [][]string) map[int]*ChannelObservation {
	col := map[string]int{}
	for i, h := range headers {
		if h != "" {
			col[h] = i
		}
	}
	idxChannel, hasChannelCol := col["channel_id"]
	idxOther, hasOtherCol := col["other"]
	idxQuota, hasQuota := col["quota"]
	idxType, hasType := col["type"]
	idxGroup, hasGroup := col["group"]
	idxModel, hasModel := col["model_name"]

	type acc struct {
		obs        *ChannelObservation
		groups     map[string]bool
		modelsRows map[string]int
	}
	byID := map[int]*acc{}
	get := func(id int) *acc {
		a, ok := byID[id]
		if !ok {
			a = &acc{
				obs:        &ChannelObservation{ChannelID: id},
				groups:     map[string]bool{},
				modelsRows: map[string]int{},
			}
			byID[id] = a
		}
		return a
	}

	for _, row := range rows {
		if len(row) == 0 {
			continue
		}
		ids := rowChannelIDs(row, idxChannel, hasChannelCol, idxOther, hasOtherCol)
		if len(ids) == 0 {
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
		isAdjustment := IsTaskQuotaAdjustment(other)
		if isAdjustment {
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

		group, model := "", ""
		if hasGroup {
			group = strings.TrimSpace(cellAt(row, idxGroup))
		}
		if hasModel {
			model = strings.TrimSpace(cellAt(row, idxModel))
		}

		for _, id := range ids {
			if id <= 0 {
				continue
			}
			a := get(id)
			if group != "" {
				a.groups[group] = true
			}
			// 模型按「消费行」计数：退款行带的模型名不代表这个渠道又服务了一次请求。
			if model != "" && !isAdjustment {
				a.modelsRows[model]++
			}
		}

		// 额度与行数只在「单渠道行」上归属，原因见 ChannelObservation。
		if len(ids) == 1 && ids[0] > 0 {
			a := get(ids[0])
			a.obs.QuotaNet += delta
			if !isAdjustment {
				a.obs.Rows++
			}
		}
	}

	out := make(map[int]*ChannelObservation, len(byID))
	for id, a := range byID {
		o := a.obs
		for g := range a.groups {
			o.Groups = append(o.Groups, g)
		}
		sort.Strings(o.Groups)

		for m := range a.modelsRows {
			o.Models = append(o.Models, m)
		}
		sort.Slice(o.Models, func(i, j int) bool {
			ri, rj := a.modelsRows[o.Models[i]], a.modelsRows[o.Models[j]]
			if ri != rj {
				return ri > rj
			}
			return o.Models[i] < o.Models[j]
		})

		if len(o.Models) > 0 {
			o.AllDomesticModels = true
			for _, m := range o.Models {
				if VendorFamily(m) != "" {
					o.AnyDomesticModels = true
				} else {
					o.AllDomesticModels = false
				}
			}
		}
		out[id] = o
	}
	return out
}

// ChannelReviewItem 核对弹窗里的一行：一个渠道的当前配置 + 它在本次日志里的观测事实。
type ChannelReviewItem struct {
	ChannelID int    `json:"channelId"`
	Name      string `json:"name"`
	Known     bool   `json:"known"`

	// UpstreamRatio / IsDomestic 是**当前存着的配置**，弹窗里让用户确认或修改。
	// UpstreamRatio 为 nil 表示还没维护。
	UpstreamRatio *float64 `json:"upstreamRatio"`
	IsDomestic    bool     `json:"isDomestic"`

	// 以下是本次日志里的观测事实，只读展示。
	Groups []string `json:"groups"`
	Models []string `json:"models"`
	Rows   int      `json:"rows"`
	// AmountCNY 该渠道在本次日志里的金额（净额度 ÷ QuotaPerCNY）。
	// 是**日志额度折算**，不是账单上的结算额（模板一的结算额是刊例 × 折扣），
	// 这里只用来让用户判断「这个渠道值不值得认真核对」。
	AmountCNY         float64 `json:"amountCny"`
	AllDomesticModels bool    `json:"allDomesticModels"`
	AnyDomesticModels bool    `json:"anyDomesticModels"`
}

// BuildChannelReview 把观测事实与当前配置拼成核对清单，按金额降序（同额按渠道号升序）。
//
// 按金额排序是因为人的注意力有限：一个月 3 元的渠道标错了无关痛痒，
// 占了九成金额的渠道标错了成本就整个失真，后者必须排在最前面。
func BuildChannelReview(obs map[int]*ChannelObservation, ratios map[int]float64,
	domestic map[int]bool, channels map[int]ChannelInfo) []ChannelReviewItem {

	out := make([]ChannelReviewItem, 0, len(obs))
	for id, o := range obs {
		info, known := channels[id]
		name := info.Name
		if !known {
			name = fmt.Sprintf("渠道 %d（不在渠道清单里）", id)
		}
		item := ChannelReviewItem{
			ChannelID: id, Name: name, Known: known,
			IsDomestic: domestic[id],
			Groups:     o.Groups, Models: o.Models, Rows: o.Rows,
			AmountCNY:         round(o.QuotaNet/QuotaPerCNY, MoneyDecimals),
			AllDomesticModels: o.AllDomesticModels,
			AnyDomesticModels: o.AnyDomesticModels,
		}
		if v, ok := ratios[id]; ok {
			r := v
			item.UpstreamRatio = &r
		}
		out = append(out, item)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].AmountCNY != out[j].AmountCNY {
			return out[i].AmountCNY > out[j].AmountCNY
		}
		return out[i].ChannelID < out[j].ChannelID
	})
	return out
}

// EnrichIssues 把本次日志里观测到的模型信息补到待补录清单上（国模标识本身已由
// CheckChannelRatios 带上）。
//
// 单独成函数而不是让 CheckChannelRatios 自己去扫日志：后者只吃 ChannelUsage，
// 而模型信息只在任务页的补录面板里用得上，就地补一遍比把每个调用方都拖去多扫一遍更稳。
func EnrichIssues(issues []ChannelIssue, obs map[int]*ChannelObservation) {
	for i := range issues {
		if o, ok := obs[issues[i].ChannelID]; ok {
			issues[i].Models = o.Models
			issues[i].AllDomesticModels = o.AllDomesticModels
			issues[i].AnyDomesticModels = o.AnyDomesticModels
		}
	}
}

// CheckChannelRatios 检查这批渠道的倍率维护情况。
//
// ratios 只含已维护的渠道（见 ChannelUpstreamConfig）；domestic 是国模标识（只含标了的，可为 nil）；
// channels 是本地渠道清单。判定复用 CheckUpstreamRatios，这里只负责把结果映射成带分组的展示结构。
func CheckChannelRatios(usage map[int]*ChannelUsage, ratios map[int]float64, domestic map[int]bool,
	channels map[int]ChannelInfo, rowCounts map[int]int) ChannelCheckResult {

	ids := make([]int, 0, len(usage))
	for id := range usage {
		ids = append(ids, id)
	}
	sort.Ints(ids)

	status := CheckUpstreamRatios(ids, ratios, channels)
	result := ChannelCheckResult{
		UsedChannels: make([]UsedChannel, 0, len(ids)),
		// Missing 从**倍率表**直接推，不再只认 CheckUpstreamRatios 的结论：
		// 那个函数把清单之外的渠道归进 UnknownChannelIDs 并当作「补不了」，
		// 而倍率表根本不需要渠道清单（主键就是 channel_id）。
		// 结果是：清单快照没拉到的渠道再也算不出成本，页面上还没有输入框可以填。
		Missing:           []ChannelIssue{},
		Maintained:        status.Maintained,
		UnknownChannelIDs: status.UnknownChannelIDs,
	}

	for _, id := range ids {
		u := usage[id]
		info, known := channels[id]
		name := info.Name
		if !known {
			name = fmt.Sprintf("渠道 %d（不在渠道清单里）", id)
		}
		var ratio *float64
		if v, ok := ratios[id]; ok {
			r := v
			ratio = &r
		}
		result.UsedChannels = append(result.UsedChannels, UsedChannel{
			ChannelID: id, Name: name, ChannelGroup: info.ChannelGroup,
			Groups: u.Groups, UpstreamRatio: ratio, Known: known,
			IsDomestic: domestic[id],
		})
		if ratio == nil {
			result.Missing = append(result.Missing, ChannelIssue{
				ChannelID: id, Name: name, ChannelGroup: info.ChannelGroup,
				Groups: u.Groups, Known: known, RowCount: rowCounts[id],
				IsDomestic: domestic[id],
			})
		}
	}
	return result
}

// CountMissingGroupRatioRows 数出日志里 group_ratio 缺失的行数。
//
// 这些行无法反推官方刊例（模板二的成本列要留空），数量要如实报给用户，
// 而不是悄悄按 0 算——按 0 算会让刊例虚高到无穷、成本跟着失真。
func CountMissingGroupRatioRows(headers []string, rows [][]string) int {
	col := map[string]int{}
	for i, h := range headers {
		if h != "" {
			col[h] = i
		}
	}
	idxOther, ok := col["other"]
	if !ok {
		return len(rows)
	}

	n := 0
	for _, row := range rows {
		if len(row) == 0 {
			continue
		}
		if ratio, ok := GroupRatioFromOther(cellAt(row, idxOther)); !ok || ratio <= 0 {
			n++
		}
	}
	return n
}
