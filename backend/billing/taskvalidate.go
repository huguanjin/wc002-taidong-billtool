package billing

import (
	"fmt"
	"time"
)

// 批量执行前的整体校验。
//
// 为什么要在动手之前一次性校验：批量执行是同步的，一条计划可能跑几十秒
// （导日志 + 聚合上百万行）。若跑到第 4 条才发现「客户没配账号」，
// 前 3 条已经真地导了日志、出了账，用户还得自己判断哪些跑过哪些没跑。
// 先把问题一次列全，用户改完再执行，比边跑边撞墙好。

// TaskValidation 单条计划的校验结果。
type TaskValidation struct {
	TaskID   int64  `json:"taskId"`
	TaskName string `json:"taskName"`
	// Error 为空表示这条计划可以执行。
	Error string `json:"error"`
}

// OK 该条是否通过校验。
func (v TaskValidation) OK() bool { return v.Error == "" }

// ValidateTasksForRun 校验一批计划能否执行，返回与输入等长的结果（顺序一致）。
//
// customers 由调用方一次查好传入（客户数很少，避免每条计划各查一次库）；
// channelRatios 是已维护的渠道倍率表，仅在计划勾了成本利润表时用到。
func ValidateTasksForRun(tasks []BillTask, customers map[int64]Customer,
	channelRatios map[int]float64) []TaskValidation {

	out := make([]TaskValidation, 0, len(tasks))
	for _, t := range tasks {
		out = append(out, TaskValidation{
			TaskID:   t.ID,
			TaskName: t.DisplayName(),
			Error:    validateOneTask(t, customers, channelRatios),
		})
	}
	return out
}

// validateOneTask 返回空串表示通过，否则是给用户看的原因。
func validateOneTask(t BillTask, customers map[int64]Customer, channelRatios map[int]float64) string {
	customer, ok := customers[t.CustomerID]
	if !ok {
		// 客户被删掉了（任务表有 ON DELETE CASCADE，正常不会剩下这种行；
		// 但若有并发删除，这里能给出明确原因而不是让执行阶段报个泛化错误）。
		return fmt.Sprintf("客户不存在（ID %d），可能已被删除", t.CustomerID)
	}
	if len(customer.UsernameList()) == 0 {
		return fmt.Sprintf("客户「%s」还没有配置业务库账号，请先到客户信息页填写", customer.Name)
	}

	if t.StartTime == nil || t.EndTime == nil {
		return "还没有设置时段，请先编辑填写起止日期"
	}
	start, end := *t.StartTime, *t.EndTime
	if end.Before(start) {
		return "结束时间早于开始时间"
	}
	if span := end.Sub(start); span > time.Duration(MaxExportDays)*24*time.Hour {
		return fmt.Sprintf("时段跨度 %.1f 天超过上限 %d 天，请拆成多个计划",
			span.Hours()/24, MaxExportDays)
	}

	// 勾了成本利润表但没有维护任何渠道倍率：执行下去会被整表拦下，
	// 白导一次日志。这里提前说清楚——但只在**一个都没维护**时才拦，
	// 部分维护的情况照常执行（未维护的行会在成本表里留空并标注）。
	if t.GenerateCost && len(channelRatios) == 0 {
		return "勾选了生成成本利润表，但还没有维护任何渠道上游倍率；" +
			"请先到「渠道成本倍率」页拉取渠道并维护倍率，或取消该勾选"
	}

	return ""
}
