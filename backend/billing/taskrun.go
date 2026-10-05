package billing

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// 一键出账任务的编排：按计划的时段导出客户日志 → 出账 →（可选）成本利润表。
//
// 这里只做编排，不碰全局状态（不登记 job、不删文件）——那些属于 HTTP 层的职责。
// 保持纯编排的好处是这条链路能用测试直接跑，不必起 HTTP 服务。

// DisplayName 计划的展示名。用户没起名时用「客户 + 时段」兜底，
// 让列表里每条都读得懂，而不是一排空白。
func (t BillTask) DisplayName() string {
	if t.Name != "" {
		return t.Name
	}
	if t.StartTime != nil && t.EndTime != nil {
		return fmt.Sprintf("%s %s~%s", t.CustomerName,
			t.StartTime.Format("01-02"), t.EndTime.Format("01-02"))
	}
	return t.CustomerName
}

// TaskRunDeps 一次任务执行需要的外部依赖。
//
// 全部由调用方（handler）注入，而不是在函数里去读全局变量：
// 这样测试可以传临时目录与假配置，也避免 billing 包反向依赖 main 包的全局状态。
type TaskRunDeps struct {
	DB               DBConfig // 业务库（只读），用来导出日志
	PG               PGConfig // 本地库，读计划、客户、默认参数与渠道倍率
	DataDir          string   // 导出日志落这里（与「已导出文件」列表同目录）
	JobDir           string   // 出账产物落这里（6 小时后由 cleanupOldJobs 清理）
	TemplatePath     string   // data/bill_template.xlsx
	PriceTablePath   string   // data/price_table.xlsx
	DBPriceCachePath string   // data/db_price_cache.json

	// TaskID 要执行的**计划** ID。客户、时段、勾选全部来自这条计划，
	// 不再由调用方拼参数——那样「先建好计划、之后批量执行」就失去意义了。
	TaskID int64
}

// TaskRunResult 一次任务执行的结果。
type TaskRunResult struct {
	TaskID              int64
	Customer            Customer
	Task                BillTask // 结果字段已填好，调用方负责 SaveBillTaskResult
	BillPath            string
	SanitizedPath       string // 为空表示未生成
	CostPath            string // 为空表示未生成或被拦下
	CostBlocked         bool
	MissingChannelInfos []ChannelInfo
	UnknownChannelIDs   []int
	CostSummary         string // 可复制的成本利润摘要，为空表示无成本数据
	Summary             Summary
	LogPath             string // 导出的源日志
	LogRowCount         int64
}

// RunBillExportTask 执行一条计划。
//
// 失败一律返回 error 且**不落库**——调用方只在成功时 SaveBillTaskResult。
// 这个约定很重要：执行结果是覆盖写的，一次失败如果也写库，
// 会把上一次跑出来的好数字覆盖成空值，页面上看起来「跑过了」但数已经没了。
// 批量执行时这条同样成立：单个任务失败不能影响其他任务已成功的记录。
func RunBillExportTask(deps TaskRunDeps) (*TaskRunResult, error) {
	// ---- 1. 读计划并校验时段 ----
	task, err := GetBillTask(deps.PG, deps.TaskID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("任务不存在（可能已被删除）")
		}
		return nil, fmt.Errorf("读取任务失败: %w", err)
	}
	if task.StartTime == nil || task.EndTime == nil {
		return nil, fmt.Errorf("任务「%s」还没有设置时段，请先编辑填写起止日期", task.DisplayName())
	}
	start, end := *task.StartTime, *task.EndTime

	// 计划保存时已校验过，这里再挡一道：库里的行可能被直接改过，
	// 而这条上限守的是业务库的索引约束（logs 表在 (username, created_at) 上没组合索引）。
	if span := end.Sub(start); span > time.Duration(MaxExportDays)*24*time.Hour {
		return nil, fmt.Errorf("时段跨度 %.1f 天超过上限 %d 天，请拆成多个计划",
			span.Hours()/24, MaxExportDays)
	}

	// ---- 2. 客户与账号 ----
	customer, err := GetCustomer(deps.PG, task.CustomerID)
	if err != nil {
		return nil, fmt.Errorf("读取客户失败: %w", err)
	}
	usernames := customer.UsernameList()
	if len(usernames) == 0 {
		return nil, fmt.Errorf("客户「%s」还没有配置业务库账号，请先在客户信息页填写", customer.Name)
	}

	// ---- 3. 导出客户日志（只读业务库）----
	exported, err := ExportLogsFromDB(deps.DB, deps.DataDir, LogExportParams{
		Usernames: usernames,
		StartTime: start,
		EndTime:   end,
	})
	if err != nil {
		return nil, fmt.Errorf("导出该客户日志失败: %w", err)
	}
	// 0 行不是错误，但也没法出账——直接告诉用户这个时段没有消费，
	// 而不是生成一张全是 0 的账单让他自己发现。
	if exported.RowCount == 0 {
		return nil, fmt.Errorf("客户「%s」在 %s ~ %s 没有消费记录（已按 %d 个账号查询）",
			customer.Name, start.Format("2006-01-02"), end.Format("2006-01-02"), len(usernames))
	}

	// ---- 4. 装载默认出账参数 ----
	settings, err := GetSettings(deps.PG)
	if err != nil {
		return nil, err
	}

	params := Params{
		// 账期用计划上存的**归属账期**，不靠推断：
		// 导出的日志文件名是「日志查询_<起>_<止>_<指纹>.tsv」，不含「N月」字样，
		// GenerateBill 的 monthFromFilename 认不出来，会落到日志内容的月份——
		// 跨月或跨月补录时那个推断就错了。
		Year:                 task.PeriodYear,
		Month:                task.PeriodMonth,
		Discount:             settings.Discount,
		ExchangeRate:         settings.ExchangeRate,
		PriceSource:          PriceSource(settings.PriceSource),
		SanitizedLog:         task.GenerateSanitized,
		SanitizedFormat:      settings.SanitizedFormat,
		IncludeBillingParams: settings.IncludeBillingParams,
		DomesticMarkers:      settings.DomesticMarkerList(),
		GenerateCost:         task.GenerateCost,
	}

	// 成本利润表需要渠道上游倍率；从本地 PG 读好传进去（billing 的算账逻辑不连 PG）。
	if task.GenerateCost {
		ratios, err := ChannelRatioMap(deps.PG)
		if err != nil {
			return nil, err
		}
		channels, err := ListChannels(deps.PG)
		if err != nil {
			return nil, err
		}
		params.ChannelUpstreamRatios = ratios
		params.ChannelNames = make(map[int]string, len(channels))
		params.ChannelInfos = make(map[int]ChannelInfo, len(channels))
		for _, c := range channels {
			params.ChannelNames[c.ChannelID] = c.Name
			params.ChannelInfos[c.ChannelID] = c.ChannelInfo
		}
	}

	// ---- 5. 出账 ----
	gen, err := GenerateBill(exported.Path, deps.TemplatePath, deps.PriceTablePath,
		deps.DBPriceCachePath, deps.JobDir, params)
	if err != nil {
		return nil, err
	}

	// ---- 6. 组装结果（只填结果字段，计划字段原样保留）----
	settle := gen.Summary.SettleCNYTotal
	list := gen.Summary.ListCNYTotal
	discount := gen.Summary.OverallDiscount
	task.SettleCNY = &settle
	task.ListCNY = &list
	task.OverallDiscount = &discount
	task.RowCount = int(exported.RowCount)
	task.LogPath = exported.Path
	task.CustomerName = customer.Name // 刷新冗余快照，客户改过名的话跟上

	// 只有成本利润表真的生成出来了，才有成本口径可落。
	// 被拦下（渠道倍率没维护）时三个成本字段保持 nil，页面据此显示「未核算成本」——
	// 这比写 0 安全：0 会被读成「上游免费」，利润虚高。
	if gen.CostTotals != nil {
		costedSettle := gen.CostTotals.SettleCNY
		cost := gen.CostTotals.CostCNY
		profit := gen.CostTotals.ProfitCNY
		task.CostedSettleCNY = &costedSettle
		task.CostCNY = &cost
		task.ProfitCNY = &profit
		task.CostComplete = gen.CostTotals.PricedRows == gen.CostTotals.TotalRows
		task.PricedRows = gen.CostTotals.PricedRows
		task.TotalCostRows = gen.CostTotals.TotalRows
	}

	return &TaskRunResult{
		TaskID:              task.ID,
		Customer:            customer,
		Task:                task,
		BillPath:            gen.BillPath,
		SanitizedPath:       gen.SanitizedPath,
		CostPath:            gen.CostPath,
		CostBlocked:         gen.CostBlocked,
		MissingChannelInfos: gen.MissingChannelInfos,
		UnknownChannelIDs:   gen.UnknownChannelIDs,
		CostSummary:         gen.CostSummary,
		Summary:             gen.Summary,
		LogPath:             exported.Path,
		LogRowCount:         exported.RowCount,
	}, nil
}
