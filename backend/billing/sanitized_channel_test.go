package billing

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/xuri/excelize/v2"
)

// 脱敏日志不得暴露渠道号（channel_id）。
//
// 渠道号指向我们的上游渠道：客户拿到它就能看出请求走了哪几条上游、各占多少，
// 进而倒推出供应商与成本结构。它只该留在站内用的原始导出日志里。
//
// 渠道号有两个藏身处：独立的 channel_id 列，以及 other.admin_info 里
// （use_channel / channel_affinity.channel_id）。后者随 other 整列一起被丢弃，
// 所以这里要钉住的是：两个出口都堵死，而且**值**不能出现在产物的任何地方——
// 不只是表头里没有这一列。

// 渠道号取两个互不相干、不会与别的字段碰巧重合的 5 位数（不出现在 id、额度、
// token、时间戳里）：下面按子串查找，数太短会误伤别的列。
const (
	leakChannelA = "73917"
	leakChannelB = "58201"
)

// writeChannelLeakLog 写一份与「导出日志明细」同形的原始日志：渠道号既在 channel_id 列里，
// 也藏在 other.admin_info 里——真实日志就是这样。
func writeChannelLeakLog(t *testing.T, path string) {
	t.Helper()
	f := excelize.NewFile()
	sheet := f.GetSheetName(0)
	headers := []interface{}{
		"id", "username", "type", "created_at", "token_id", "token_name", "model_name",
		"group", "prompt_tokens", "completion_tokens", "quota", "use_time", "is_stream",
		"request_id", "other", "channel_id",
	}
	require.NoError(t, f.SetSheetRow(sheet, "A1", &headers))

	otherFor := func(ch string) string {
		return `{"group_ratio":1.8,"cache_tokens":10000,"cache_creation_tokens":5000,` +
			`"admin_info":{"use_channel":["` + ch + `"],"channel_affinity":{"channel_id":` + ch + `}}}`
	}
	rows := [][]interface{}{
		{1001, "u@example.com", 2, 1789430400, 1, "tk", "claude-sonnet-5", "default",
			1000000, 200000, 50000, 1200, 1, "req-1", otherFor(leakChannelA), leakChannelA},
		{1002, "u@example.com", 2, 1789430400, 1, "tk", "gpt-5.4", "vip",
			300000, 50000, 20000, 900, 1, "req-2", otherFor(leakChannelB), leakChannelB},
	}
	for i, row := range rows {
		axis, _ := excelize.CoordinatesToCellName(1, i+2)
		r := row
		require.NoError(t, f.SetSheetRow(sheet, axis, &r))
	}
	require.NoError(t, f.SaveAs(path))
	require.NoError(t, f.Close())
}

// fileContains 产物里任何位置出现 needle 就返回 true。
//
// xlsx 摊平读全部工作表的全部单元格；csv/tsv 直接查整份原文——后者刻意不走解析器：
// 解析器会吃掉引号与分隔符，而原文才是最终交到客户手里的东西。
func fileContains(t *testing.T, path, needle string) bool {
	t.Helper()
	if strings.EqualFold(filepath.Ext(path), ".xlsx") {
		f, err := excelize.OpenFile(path)
		require.NoError(t, err)
		defer f.Close()
		for _, sheet := range f.GetSheetList() {
			rows, err := f.GetRows(sheet)
			require.NoError(t, err)
			for _, row := range rows {
				for _, cell := range row {
					if strings.Contains(cell, needle) {
						return true
					}
				}
			}
		}
		return false
	}
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	return bytes.Contains(b, []byte(needle))
}

// TestSanitizedLogNeverExposesChannelID 账单一、账单二的脱敏日志，在三种输出格式下，
// 在开不开「附带计费参数列」两种情况下，都不得出现渠道号——列与值都不行。
func TestSanitizedLogNeverExposesChannelID(t *testing.T) {
	templates := []struct {
		name     string
		template string
	}{
		{"账单一（标准明细）", ""},
		{"账单二（简易汇总）", BillTemplateSimple},
	}

	for _, tpl := range templates {
		for _, format := range []string{"xlsx", "csv", "tsv"} {
			for _, includeBilling := range []bool{false, true} {
				name := tpl.name + "/" + format
				if includeBilling {
					name += "/附带计费参数"
				}
				t.Run(name, func(t *testing.T) {
					dir := t.TempDir()
					templatePath := filepath.Join(dir, "bill_template.xlsx")
					priceTablePath := filepath.Join(dir, "price_table.xlsx")
					logPath := filepath.Join(dir, "日志查询_2026-09-01_2026-09-30_ab12cd.xlsx")
					outDir := filepath.Join(dir, "out")
					require.NoError(t, os.MkdirAll(outDir, 0o755))
					buildFixtureTemplate(t, templatePath)
					buildFixturePriceTable(t, priceTablePath)
					writeChannelLeakLog(t, logPath)

					// 正向对照：原始日志里确实带着渠道号，下面的「查不到」才有意义。
					// 查找函数若本身有毛病（比如读错工作表），断言会在没有泄露的情况下
					// 也一直通过——这一步证明它在有值的时候是能查到的。
					require.True(t, fileContains(t, logPath, leakChannelA), "对照失败：原始日志里应能查到渠道号")
					require.True(t, fileContains(t, logPath, leakChannelB), "对照失败：原始日志里应能查到渠道号")

					res, err := GenerateBill(logPath, templatePath, priceTablePath, "", outDir, Params{
						BillTemplate:         tpl.template,
						ExchangeRate:         7,
						SanitizedLog:         true,
						SanitizedFormat:      format,
						IncludeBillingParams: includeBilling,
					})
					require.NoError(t, err)
					require.NotEmpty(t, res.SanitizedPath)

					headers, rows, err := LoadLogRows(res.SanitizedPath, "", "")
					require.NoError(t, err)

					// 列：渠道号列与 other 都不在。
					assert.NotContains(t, headers, "channel_id", "脱敏日志不得带渠道号列")
					assert.NotContains(t, headers, "other", "other 里也藏着渠道号，必须整列丢弃")

					// 值：渠道号不得出现在产物的任何位置（含 other 里提出来的明细列）。
					assert.False(t, fileContains(t, res.SanitizedPath, leakChannelA), "渠道号 %s 不得出现在脱敏日志里", leakChannelA)
					assert.False(t, fileContains(t, res.SanitizedPath, leakChannelB), "渠道号 %s 不得出现在脱敏日志里", leakChannelB)

					// 不是空转：明细行都在，且丢掉 channel_id 之后其余列没有错位。
					// channel_id 在原始日志里夹在 other 之后，丢它时若表头与取值各丢各的，
					// 后面的列会整体错一格——这里拿它前后的列都对一遍。
					require.Len(t, rows, 2, "两条消费明细都应保留")
					col := map[string]int{}
					for i, h := range headers {
						col[h] = i
					}
					assert.Equal(t, "claude-sonnet-5", rows[0][col["model_name"]])
					assert.Equal(t, "req-1", rows[0][col["request_id"]])
					assert.Equal(t, "50000", rows[0][col["quota"]])
					assert.Equal(t, "gpt-5.4", rows[1][col["model_name"]])
					assert.Equal(t, "req-2", rows[1][col["request_id"]])
					assert.Equal(t, "20000", rows[1][col["quota"]])
				})
			}
		}
	}
}
