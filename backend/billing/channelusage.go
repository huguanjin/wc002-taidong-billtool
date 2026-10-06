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

// ChannelIssue 一个待补录上游倍率的渠道，以及它在本次日志里出现在哪些分组。
type ChannelIssue struct {
	ChannelID int    `json:"channelId"`
	Name      string `json:"name"`
	// ChannelGroup 业务库 channels.group（该渠道能服务的分组候选集合）。
	// **仅作参考展示**，不参与归类——归类一律用 Groups。
	ChannelGroup string `json:"channelGroup"`
	// Groups 该渠道在本次日志里实际出现过的分组名，页面据此分节展示。
	Groups []string `json:"groups"`
}

// ChannelCheckResult 一次「上游倍率维护情况」检查的结果。
type ChannelCheckResult struct {
	// UsedChannels 本次日志用到的全部渠道（含已维护的），供页面展示完整清单。
	UsedChannels []UsedChannel `json:"usedChannels"`
	// Missing 未维护倍率、且渠道表里查得到的——可以就地补录。
	Missing []ChannelIssue `json:"missing"`
	// UnknownChannelIDs 渠道表里查不到的（多半已在业务库被硬删除）——补不了，
	// 只能如实报出，让用户知道这些渠道的成本算不全。
	UnknownChannelIDs []int `json:"unknownChannelIds"`
	// MissingGroupRatioRows group_ratio 缺失、无法反推官方刊例的行数。
	// 大于 0 时这些行的成本同样算不出来（见 AggregateSimpleBill）。
	MissingGroupRatioRows int `json:"missingGroupRatioRows"`
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
	// Known 该渠道号在本地渠道清单里是否存在。不存在时无法补录（业务库已删）。
	Known bool `json:"known"`
}

// CheckChannelRatios 检查这批渠道的倍率维护情况。
//
// ratios 只含已维护的渠道（见 ChannelRatioMap）；channels 是本地渠道清单。
// 判定复用 CheckUpstreamRatios，这里只负责把结果映射成带分组的展示结构。
func CheckChannelRatios(usage map[int]*ChannelUsage,
	ratios map[int]float64, channels map[int]ChannelInfo) ChannelCheckResult {

	ids := make([]int, 0, len(usage))
	for id := range usage {
		ids = append(ids, id)
	}
	sort.Ints(ids)

	status := CheckUpstreamRatios(ids, ratios, channels)
	result := ChannelCheckResult{
		UsedChannels:      make([]UsedChannel, 0, len(ids)),
		Missing:           []ChannelIssue{},
		UnknownChannelIDs: status.UnknownChannelIDs,
	}

	for _, id := range ids {
		u := usage[id]
		info, known := channels[id]
		name := info.Name
		if !known {
			name = fmt.Sprintf("渠道 %d（渠道清单里没有）", id)
		}
		var ratio *float64
		if v, ok := ratios[id]; ok {
			r := v
			ratio = &r
		}
		result.UsedChannels = append(result.UsedChannels, UsedChannel{
			ChannelID: id, Name: name, ChannelGroup: info.ChannelGroup,
			Groups: u.Groups, UpstreamRatio: ratio, Known: known,
		})
	}

	// Missing 沿用 CheckUpstreamRatios 的语义（渠道表里有、但没倍率），
	// 只把 Groups 补上——那是页面分节展示的依据。
	for _, info := range status.Missing {
		issue := ChannelIssue{
			ChannelID: info.ChannelID, Name: info.Name, ChannelGroup: info.ChannelGroup,
		}
		if u, ok := usage[info.ChannelID]; ok {
			issue.Groups = u.Groups
		}
		result.Missing = append(result.Missing, issue)
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
