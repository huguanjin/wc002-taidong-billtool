package billing

import (
	"fmt"
	"sort"
	"strings"

	"github.com/xuri/excelize/v2"
)

// 模板二成本表的「渠道明细」「渠道汇总」两张附表。
//
// 成本表原来只到 (分组, 模型) 一层：一个 (分组, 模型) 横跨多个渠道时，各渠道的倍率、
// 计费方式、盈亏都被合成一行，成本看着不对也没法知道是哪个渠道造成的。
// 这里把同一份日志再按渠道拆开，**逐行累加的口径与 AggregateSimpleBill 完全一致**
// （同一个循环里同时累加，不是另算一遍），所以各渠道行加起来必然等于汇总表对应行。

// SimpleBillChannelRow 一个 (分组, 模型, 渠道) 的汇总。渠道号 0 表示取不到渠道号，-1 表示一行经多个渠道。
type SimpleBillChannelRow struct {
	ChannelID   int    `json:"channelId"`
	ChannelName string `json:"channelName"`
	Group       string `json:"group"`
	Model       string `json:"model"`

	HitCount           int     `json:"hitCount"`
	TotalPrompt        float64 `json:"totalPrompt"`
	TotalCompletion    float64 `json:"totalCompletion"`
	TotalCacheRead     float64 `json:"totalCacheRead"`
	TotalCacheCreation float64 `json:"totalCacheCreation"`
	TotalQuota         float64 `json:"totalQuota"`
	TotalCostCNY       float64 `json:"totalCostCny"`

	// UpstreamRatio 该渠道当前维护的上游倍率，nil = 未维护。Domestic 为国模渠道。
	UpstreamRatio *float64 `json:"upstreamRatio"`
	Domestic      bool     `json:"domestic"`
	// BillingMode 这个渠道上该模型的上游计费口径：按量 / 按次 / 混合（两种行都有）；
	// 没有一行算出成本时为空。PerCallFeeCNY 是按次时的单次费用。
	BillingMode    string  `json:"billingMode"`
	PerCallFeeCNY  float64 `json:"perCallFeeCny"`
	PerCallUnits   float64 `json:"perCallUnits"`
	PerCallCostCNY float64 `json:"perCallCostCny"`

	// SalesRatio 平均销售倍率 = 站内额度 ÷ 站内刊例（只看按倍率估算的那些行）。
	// 与 BreakEvenRatio 并排放，一眼看出「上游倍率是否高于销售倍率」——
	// 上游倍率高于盈亏平衡点，这个渠道上的这个模型就是亏的。
	SalesRatio *float64 `json:"salesRatio"`
	// BreakEvenRatio 盈亏平衡的上游倍率：取这个值时该渠道该模型利润恰好为 0（口径同上游倍率，
	// 国模渠道是折扣、其余是每美金成本）。BreakEvenPerCall 是按次时的盈亏平衡单次费用。
	BreakEvenRatio   *float64 `json:"breakEvenRatio"`
	BreakEvenPerCall *float64 `json:"breakEvenPerCall"`

	OfficialListUSD *float64 `json:"officialListUsd"`
	UpstreamCostCNY *float64 `json:"upstreamCostCny"`
	ProfitCNY       *float64 `json:"profitCny"`

	CostRows     int            `json:"costRows"`
	TotalRows    int            `json:"totalRows"`
	SkippedQuota float64        `json:"skippedQuota"`
	SkipReasons  map[string]int `json:"skipReasons,omitempty"`
}

// ChannelLabel 渠道的展示名：特殊渠道号给出人能读的说明。
func channelLabel(id int, name string) string {
	switch {
	case id == 0:
		return "（日志里取不到渠道号）"
	case id < 0:
		return "（一行经多个渠道）"
	case strings.TrimSpace(name) == "":
		return fmt.Sprintf("渠道 %d", id)
	}
	return name
}

// simpleChannelAcc 渠道维度的累加器：基础量与 AggregateSimpleBill 里按 (分组, 模型) 的完全同构。
type simpleChannelAcc struct {
	row SimpleBillChannelRow

	skippedQuota float64
	skipReasons  map[string]int

	rows                     int
	officialQuota, upstreamC float64
	perCallUnits, perCallCNY float64
	perCallUncertain         int
	perCallRows, ratioRows   int
	fee                      float64

	// 按倍率估算的那部分：额度、站内刊例（未做国模换算）、站内刊例（国模已除汇率）。
	ratioDelta, ratioListRaw, ratioListAdj float64
	// 按次的那部分额度。
	perCallDelta float64
}

func (a *simpleChannelAcc) skip(reason CostSkipReason, delta float64) {
	if reason == SkipZeroDelta {
		return
	}
	a.skippedQuota += delta
	if a.skipReasons == nil {
		a.skipReasons = map[string]int{}
	}
	a.skipReasons[string(reason)]++
}

// finalize 收口成输出行。rate 是汇率，用于盈亏平衡倍率的换算。
func (a *simpleChannelAcc) finalize(opts SimpleBillOptions, rate float64) SimpleBillChannelRow {
	r := a.row
	r.TotalCostCNY = round(r.TotalQuota/QuotaPerCNY, MoneyDecimals)
	if r.ChannelID > 0 {
		if v, ok := opts.UpstreamRatios[r.ChannelID]; ok {
			vv := v
			r.UpstreamRatio = &vv
		}
		r.Domestic = opts.DomesticChannels[r.ChannelID]
		r.ChannelName = opts.ChannelNames[r.ChannelID]
	}
	r.ChannelName = channelLabel(r.ChannelID, r.ChannelName)
	r.SkippedQuota = round(a.skippedQuota, MoneyDecimals)
	r.SkipReasons = a.skipReasons
	r.CostRows = a.rows
	if a.rows == 0 {
		return r // 一行都没算出来：成本列留空，不报 0（会被读成上游免费）
	}

	switch {
	case a.perCallRows > 0 && a.ratioRows > 0:
		r.BillingMode = "混合"
	case a.perCallRows > 0:
		r.BillingMode = "按次"
	default:
		r.BillingMode = "按量"
	}
	r.PerCallFeeCNY = a.fee
	r.PerCallUnits = round(a.perCallUnits, 4)
	r.PerCallCostCNY = round(a.perCallCNY, MoneyDecimals)

	official := round(a.officialQuota/QuotaPerCNY, MoneyDecimals)
	cost := round(a.upstreamC, MoneyDecimals)
	profit := round(r.TotalCostCNY-cost, MoneyDecimals)
	r.OfficialListUSD, r.UpstreamCostCNY, r.ProfitCNY = &official, &cost, &profit

	// 盈亏平衡：令利润为 0 反解上游倍率 / 单次费用。只针对各自那部分的行，
	// 混合渠道里按次的行不能拿来反解倍率，反之亦然。
	if a.ratioListRaw > 0 && a.ratioListAdj > 0 && a.ratioDelta > 0 {
		sales := a.ratioDelta / a.ratioListRaw
		r.SalesRatio = &sales
		// 成本 = listAdj/QPC × rate × discount；令成本 = 额度/QPC。
		discount := a.ratioDelta / (a.ratioListAdj * rate)
		be := discount * DiscountBaseFactor
		if r.Domestic {
			be = discount
		}
		r.BreakEvenRatio = &be
	}
	if a.perCallUnits > 0 && a.perCallDelta > 0 {
		be := a.perCallDelta / QuotaPerCNY / a.perCallUnits
		r.BreakEvenPerCall = &be
	}
	return r
}

// SimpleBillChannelTotal 渠道汇总：同一渠道跨分组、跨模型的合计。
type SimpleBillChannelTotal struct {
	ChannelID     int
	ChannelName   string
	UpstreamRatio *float64
	Domestic      bool
	Models        int
	HitCount      int
	TotalQuota    float64
	TotalCostCNY  float64
	// 成本三项：算不全时口径同汇总表（只覆盖算得出来的行），Partial 为真提示读的人别当整体毛利。
	OfficialListUSD, UpstreamCostCNY, ProfitCNY float64
	HasCost                                     bool
	Partial                                     bool
	CostRows, TotalRows                         int
}

// SummarizeByChannel 把渠道明细按渠道合并，按金额降序（同额按渠道号升序）。
func SummarizeByChannel(rows []SimpleBillChannelRow) []SimpleBillChannelTotal {
	byID := map[int]*SimpleBillChannelTotal{}
	models := map[int]map[string]bool{}
	var order []int
	for _, r := range rows {
		t, ok := byID[r.ChannelID]
		if !ok {
			t = &SimpleBillChannelTotal{
				ChannelID: r.ChannelID, ChannelName: r.ChannelName,
				UpstreamRatio: r.UpstreamRatio, Domestic: r.Domestic,
			}
			byID[r.ChannelID] = t
			models[r.ChannelID] = map[string]bool{}
			order = append(order, r.ChannelID)
		}
		models[r.ChannelID][r.Model] = true
		t.HitCount += r.HitCount
		t.TotalQuota += r.TotalQuota
		t.TotalRows += r.TotalRows
		t.CostRows += r.CostRows
		if r.CostRows < r.TotalRows {
			t.Partial = true
		}
		if r.UpstreamCostCNY != nil {
			t.HasCost = true
			t.OfficialListUSD += *r.OfficialListUSD
			t.UpstreamCostCNY += *r.UpstreamCostCNY
			t.ProfitCNY += *r.ProfitCNY
		}
	}
	out := make([]SimpleBillChannelTotal, 0, len(order))
	for _, id := range order {
		t := byID[id]
		t.Models = len(models[id])
		t.TotalCostCNY = round(t.TotalQuota/QuotaPerCNY, MoneyDecimals)
		t.OfficialListUSD = round(t.OfficialListUSD, MoneyDecimals)
		t.UpstreamCostCNY = round(t.UpstreamCostCNY, MoneyDecimals)
		t.ProfitCNY = round(t.ProfitCNY, MoneyDecimals)
		out = append(out, *t)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].TotalCostCNY != out[j].TotalCostCNY {
			return out[i].TotalCostCNY > out[j].TotalCostCNY
		}
		return out[i].ChannelID < out[j].ChannelID
	})
	return out
}

// 渠道明细表的列。成本表专用——这些列含采购倍率与单笔毛利，客户版文件里不会出现。
var SimpleBillChannelColumns = []string{
	"渠道ID", "渠道名称", "分组", "模型", "次数",
	"输入Token", "输出Token", "缓存读Token", "缓存创建Token",
	"额度", "金额（人民币）",
	"上游倍率", "国模渠道", "上游计费", "单次费用", "按次次数",
	"销售倍率", "盈亏平衡上游倍率", "盈亏平衡单次费用",
	"官方刊例（美金）", "上游成本（人民币）", "利润（人民币）", "成本/金额", "备注",
}

// SimpleBillChannelTotalColumns 渠道汇总表的列。
var SimpleBillChannelTotalColumns = []string{
	"渠道ID", "渠道名称", "上游倍率", "国模渠道", "涉及模型数", "次数",
	"额度", "金额（人民币）",
	"官方刊例（美金）", "上游成本（人民币）", "利润（人民币）", "成本/金额", "备注",
}

func costRatioCell(cost, amount *float64) interface{} {
	if cost == nil || amount == nil || *amount == 0 {
		return ""
	}
	return round(*cost / *amount, 4)
}

func boolText(b bool) string {
	if b {
		return "是"
	}
	return ""
}

func channelCell(v *float64) interface{} {
	if v == nil {
		return ""
	}
	return *v
}

// channelNote 备注：没算成本的原因，一眼看出该去补什么。
func channelNote(reasons map[string]int, partial bool, ratioMissing bool) string {
	desc := DescribeSkipReasons(reasons)
	switch {
	case desc != "":
		return "未计入成本：" + desc
	case ratioMissing && partial:
		return "未维护上游倍率"
	}
	return ""
}

// writeChannelSheets 往成本表工作簿里追加「渠道明细」「渠道汇总」。
// 亏损的行（利润为负）整行标红：排查时第一眼就要看到哪个渠道在亏。
func writeChannelSheets(f *excelize.File, rows []SimpleBillChannelRow) error {
	styleHeader, err := f.NewStyle(&excelize.Style{Font: &excelize.Font{Bold: true}})
	if err != nil {
		return err
	}
	money, err := f.NewStyle(&excelize.Style{CustomNumFmt: strPtr(MoneyCNYFmt)})
	if err != nil {
		return err
	}
	moneyLoss, err := f.NewStyle(&excelize.Style{CustomNumFmt: strPtr(MoneyCNYFmt),
		Font: &excelize.Font{Color: "C00000", Bold: true}})
	if err != nil {
		return err
	}
	// 亏损行的标红样式按单元格内容分开：渠道号/名称/模型是文本与整数，倍率要保留原有位数，
	// 都不能套金额的四位小数格式（否则渠道号会显示成 846.0000）。
	lossText, err := f.NewStyle(&excelize.Style{Font: &excelize.Font{Color: "C00000", Bold: true}})
	if err != nil {
		return err
	}
	lossRatio, err := f.NewStyle(&excelize.Style{CustomNumFmt: strPtr("0.####"),
		Font: &excelize.Font{Color: "C00000", Bold: true}})
	if err != nil {
		return err
	}
	ratioStyle, err := f.NewStyle(&excelize.Style{CustomNumFmt: strPtr("0.####")})
	if err != nil {
		return err
	}
	moneyBold, err := f.NewStyle(&excelize.Style{CustomNumFmt: strPtr(MoneyCNYFmt), Font: &excelize.Font{Bold: true}})
	if err != nil {
		return err
	}
	acct, err := f.NewStyle(&excelize.Style{CustomNumFmt: strPtr(AccountingFmt)})
	if err != nil {
		return err
	}
	acctBold, err := f.NewStyle(&excelize.Style{CustomNumFmt: strPtr(AccountingFmt), Font: &excelize.Font{Bold: true}})
	if err != nil {
		return err
	}
	axis := func(c, r int) string { a, _ := excelize.CoordinatesToCellName(c, r); return a }

	// ---- 渠道明细 ----
	const detail = "渠道明细"
	if _, err := f.NewSheet(detail); err != nil {
		return err
	}
	hdr := make([]interface{}, len(SimpleBillChannelColumns))
	for i, h := range SimpleBillChannelColumns {
		hdr[i] = h
	}
	if err := f.SetSheetRow(detail, "A1", &hdr); err != nil {
		return err
	}
	_ = f.SetCellStyle(detail, "A1", axis(len(hdr), 1), styleHeader)

	col := func(name string) int {
		idx, ok := columnIndex(SimpleBillChannelColumns, name)
		if !ok {
			panic("渠道明细：没有列 " + name)
		}
		return idx + 1
	}
	for i, r := range rows {
		rn := 2 + i
		ratioMissing := r.UpstreamRatio == nil && r.ChannelID > 0 && r.BillingMode != "按次"
		vals := []interface{}{
			r.ChannelID, r.ChannelName, r.Group, r.Model, r.HitCount,
			r.TotalPrompt, r.TotalCompletion, r.TotalCacheRead, r.TotalCacheCreation,
			r.TotalQuota, r.TotalCostCNY,
			channelCell(r.UpstreamRatio), boolText(r.Domestic), r.BillingMode, "", "",
			channelCell(r.SalesRatio), channelCell(r.BreakEvenRatio), channelCell(r.BreakEvenPerCall),
			channelCell(r.OfficialListUSD), channelCell(r.UpstreamCostCNY), channelCell(r.ProfitCNY),
			costRatioCell(r.UpstreamCostCNY, &r.TotalCostCNY),
			channelNote(r.SkipReasons, r.CostRows < r.TotalRows, ratioMissing),
		}
		if r.PerCallFeeCNY > 0 {
			vals[col("单次费用")-1] = r.PerCallFeeCNY
			vals[col("按次次数")-1] = r.PerCallUnits
		}
		if err := f.SetSheetRow(detail, axis(1, rn), &vals); err != nil {
			return err
		}
		for _, name := range []string{"输入Token", "输出Token", "缓存读Token", "缓存创建Token"} {
			_ = f.SetCellStyle(detail, axis(col(name), rn), axis(col(name), rn), acct)
		}
		for _, name := range []string{"额度", "金额（人民币）", "官方刊例（美金）", "上游成本（人民币）"} {
			_ = f.SetCellStyle(detail, axis(col(name), rn), axis(col(name), rn), money)
		}
		profitStyle := money
		if r.ProfitCNY != nil && *r.ProfitCNY < 0 {
			profitStyle = moneyLoss
			// 亏损行的渠道名、模型、倍率一并标红，扫一眼就能定位。
			for _, name := range []string{"渠道ID", "渠道名称", "模型"} {
				_ = f.SetCellStyle(detail, axis(col(name), rn), axis(col(name), rn), lossText)
			}
			for _, name := range []string{"上游倍率", "销售倍率", "盈亏平衡上游倍率"} {
				_ = f.SetCellStyle(detail, axis(col(name), rn), axis(col(name), rn), lossRatio)
			}
		} else {
			for _, name := range []string{"上游倍率", "销售倍率", "盈亏平衡上游倍率"} {
				_ = f.SetCellStyle(detail, axis(col(name), rn), axis(col(name), rn), ratioStyle)
			}
		}
		_ = f.SetCellStyle(detail, axis(col("利润（人民币）"), rn), axis(col("利润（人民币）"), rn), profitStyle)
	}
	if len(rows) > 0 {
		last := 1 + len(rows)
		tr := last + 1
		_ = f.SetCellValue(detail, axis(1, tr), "合计")
		_ = f.SetCellStyle(detail, axis(1, tr), axis(1, tr), styleHeader)
		// 成本三列只有每一行都算出来才写合计：SUM 会跳过空格，写出来是个偏小的数（口径同汇总表）。
		allCosted := true
		for _, r := range rows {
			if r.UpstreamCostCNY == nil || r.CostRows < r.TotalRows {
				allCosted = false
				break
			}
		}
		for _, sc := range []struct {
			name  string
			style int
			cost  bool
		}{
			{"次数", acctBold, false}, {"额度", moneyBold, false}, {"金额（人民币）", moneyBold, false},
			{"官方刊例（美金）", moneyBold, true}, {"上游成本（人民币）", moneyBold, true}, {"利润（人民币）", moneyBold, true},
		} {
			if sc.cost && !allCosted {
				continue
			}
			letter, _ := excelize.ColumnNumberToName(col(sc.name))
			_ = f.SetCellFormula(detail, axis(col(sc.name), tr), fmt.Sprintf("SUM(%s2:%s%d)", letter, letter, last))
			_ = f.SetCellStyle(detail, axis(col(sc.name), tr), axis(col(sc.name), tr), sc.style)
		}
		if !allCosted {
			_ = f.SetCellValue(detail, axis(1, tr+1),
				"注：有渠道没算出成本，成本三列不写合计（避免只加了一部分）；见各行备注。")
		}
	}
	_ = f.SetColWidth(detail, "A", "A", 9)
	_ = f.SetColWidth(detail, "B", "B", 30)
	_ = f.SetColWidth(detail, "C", "D", 20)
	_ = f.SetColWidth(detail, "E", "X", 14)
	_ = f.SetPanes(detail, &excelize.Panes{Freeze: true, XSplit: 4, YSplit: 1, TopLeftCell: "E2", ActivePane: "bottomRight"})

	// ---- 渠道汇总 ----
	const sum = "渠道汇总"
	if _, err := f.NewSheet(sum); err != nil {
		return err
	}
	totals := SummarizeByChannel(rows)
	h2 := make([]interface{}, len(SimpleBillChannelTotalColumns))
	for i, h := range SimpleBillChannelTotalColumns {
		h2[i] = h
	}
	if err := f.SetSheetRow(sum, "A1", &h2); err != nil {
		return err
	}
	_ = f.SetCellStyle(sum, "A1", axis(len(h2), 1), styleHeader)
	c2 := func(name string) int {
		idx, ok := columnIndex(SimpleBillChannelTotalColumns, name)
		if !ok {
			panic("渠道汇总：没有列 " + name)
		}
		return idx + 1
	}
	for i, t := range totals {
		rn := 2 + i
		var ratio interface{} = ""
		if t.UpstreamRatio != nil {
			ratio = *t.UpstreamRatio
		}
		var off, cost, profit, cr interface{} = "", "", "", ""
		note := ""
		if t.HasCost {
			off, cost, profit = t.OfficialListUSD, t.UpstreamCostCNY, t.ProfitCNY
			if t.TotalCostCNY != 0 {
				cr = round(t.UpstreamCostCNY/t.TotalCostCNY, 4)
			}
			if t.Partial {
				note = fmt.Sprintf("成本只覆盖 %d/%d 行，利润不是该渠道整体毛利", t.CostRows, t.TotalRows)
			}
		} else if t.ChannelID > 0 {
			note = "未计入成本（见渠道明细备注）"
		}
		vals := []interface{}{
			t.ChannelID, t.ChannelName, ratio, boolText(t.Domestic), t.Models, t.HitCount,
			t.TotalQuota, t.TotalCostCNY, off, cost, profit, cr, note,
		}
		if err := f.SetSheetRow(sum, axis(1, rn), &vals); err != nil {
			return err
		}
		for _, name := range []string{"额度", "金额（人民币）", "官方刊例（美金）", "上游成本（人民币）"} {
			_ = f.SetCellStyle(sum, axis(c2(name), rn), axis(c2(name), rn), money)
		}
		ps := money
		if t.HasCost && t.ProfitCNY < 0 {
			ps = moneyLoss
			_ = f.SetCellStyle(sum, axis(1, rn), axis(2, rn), lossText)
		}
		_ = f.SetCellStyle(sum, axis(c2("利润（人民币）"), rn), axis(c2("利润（人民币）"), rn), ps)
		_ = f.SetCellStyle(sum, axis(c2("上游倍率"), rn), axis(c2("上游倍率"), rn), ratioStyle)
	}
	_ = f.SetColWidth(sum, "A", "A", 9)
	_ = f.SetColWidth(sum, "B", "B", 30)
	_ = f.SetColWidth(sum, "C", "L", 14)
	_ = f.SetColWidth(sum, "M", "M", 40)
	_ = f.SetPanes(sum, &excelize.Panes{Freeze: true, XSplit: 2, YSplit: 1, TopLeftCell: "C2", ActivePane: "bottomRight"})
	return nil
}
