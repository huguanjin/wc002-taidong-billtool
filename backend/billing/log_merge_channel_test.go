package billing

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 脱敏日志合并时不得带出 channel_id（见 MergeLogs）。
//
// 背景：渠道号不给客户看，新版脱敏日志已经不含 channel_id 列。但此前生成的脱敏日志
// 还带着它，这些旧文件与新文件混着合并时有两种坏结果：
//   - 新版在前当基准：旧版多出一列，mergeColumnPermutation 直接报错，合并失败；
//   - 旧版在前当基准：结果保留 channel_id 列，旧文件的行填着真实渠道号——
//     等于借合并把已经脱掉的渠道号又交了出去。

// 表头里 channel_id 的位置与真实产物一致：夹在 request_id 与 quota 之间，
// 这样丢掉它时，排在后面的列才有机会错位——测试要的就是能发现这件事。
const (
	sanHeadNew = "id\tmodel_name\trequest_id\tquota\tcache_tokens\tuncached_input_tokens\n"
	sanHeadOld = "id\tmodel_name\trequest_id\tchannel_id\tquota\tcache_tokens\tuncached_input_tokens\n"

	sanRowNew  = "1\tm-new\treq-new\t100\t5\t80\n"
	sanRowOld  = "2\tm-old\treq-old\t73917\t200\t7\t90\n"
	sanRowOld2 = "3\tm-old2\treq-old2\t58201\t300\t9\t95\n"
)

// sanExpect 各行合并后每一列应有的值，按 id 索引。
var sanExpect = map[string]map[string]string{
	"1": {"model_name": "m-new", "request_id": "req-new", "quota": "100", "cache_tokens": "5", "uncached_input_tokens": "80"},
	"2": {"model_name": "m-old", "request_id": "req-old", "quota": "200", "cache_tokens": "7", "uncached_input_tokens": "90"},
	"3": {"model_name": "m-old2", "request_id": "req-old2", "quota": "300", "cache_tokens": "9", "uncached_input_tokens": "95"},
}

// mergeFiles 按给定顺序写出输入文件并合并（第一个是基准），读回结果。
func mergeFiles(t *testing.T, contents ...string) (*MergeResult, []string, [][]string) {
	t.Helper()
	dir := t.TempDir()
	var paths []string
	for i, c := range contents {
		p := filepath.Join(dir, "输入"+string(rune('A'+i))+".tsv")
		writeTSVLog(t, p, c)
		paths = append(paths, p)
	}
	res, err := MergeLogs(paths, MergeParams{Format: "tsv", OutDir: dir})
	require.NoError(t, err)
	headers, rows, err := LoadLogRows(res.Path, "", "")
	require.NoError(t, err)
	return res, headers, rows
}

func TestMergeSanitizedLogsDropChannelID(t *testing.T) {
	wantHeaders := []string{"id", "model_name", "request_id", "quota", "cache_tokens", "uncached_input_tokens"}

	cases := []struct {
		name     string
		contents []string
		wantIDs  []string
	}{
		{"新版在前、旧版在后（旧版多出一列）", []string{sanHeadNew + sanRowNew, sanHeadOld + sanRowOld}, []string{"1", "2"}},
		{"旧版在前、新版在后（结果表头会继承 channel_id）", []string{sanHeadOld + sanRowOld, sanHeadNew + sanRowNew}, []string{"2", "1"}},
		{"两个都是旧版", []string{sanHeadOld + sanRowOld, sanHeadOld + sanRowOld2}, []string{"2", "3"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, headers, rows := mergeFiles(t, tc.contents...)

			assert.Equal(t, wantHeaders, headers, "结果不得带 channel_id，其余列顺序不变")
			assert.Equal(t, []string{"channel_id"}, res.DroppedColumns, "要如实报出剔除了哪一列")

			// 值：渠道号不得出现在结果文件的任何位置。
			raw, err := os.ReadFile(res.Path)
			require.NoError(t, err)
			assert.NotContains(t, string(raw), "73917")
			assert.NotContains(t, string(raw), "58201")

			// 不错位：丢掉的列夹在中间，排在它后面的列取值必须仍对得上。
			require.Len(t, rows, len(tc.wantIDs))
			col := map[string]int{}
			for i, h := range headers {
				col[h] = i
			}
			for i, id := range tc.wantIDs {
				assert.Equal(t, id, rows[i][col["id"]])
				for name, want := range sanExpect[id] {
					assert.Equal(t, want, rows[i][col[name]], "id=%s 的 %s 列", id, name)
				}
			}
		})
	}
}

// 输入里本来就没有 channel_id 时，什么也没剔除，就不该声称剔除过。
func TestMergeSanitizedLogsWithoutChannelIDDropsNothing(t *testing.T) {
	res, headers, rows := mergeFiles(t, sanHeadNew+sanRowNew, sanHeadNew+sanRowNew)

	assert.Empty(t, res.DroppedColumns)
	assert.NotContains(t, headers, "channel_id")
	assert.Len(t, rows, 2)
}

// 原始日志的合并不受影响：它合并之后还要拿去估成本，channel_id 必须留着。
// 判据靠 other 列区分——原始导出一定带 other，脱敏日志一定没有。
func TestMergeRawLogsKeepChannelID(t *testing.T) {
	head := "id\tmodel_name\tquota\tother\tchannel_id\n"
	a := head + "1\tm1\t100\t{}\t73917\n"
	b := head + "2\tm2\t200\t{}\t58201\n"

	res, headers, rows := mergeFiles(t, a, b)

	assert.Empty(t, res.DroppedColumns, "原始日志合并不剔除任何列")
	require.Contains(t, headers, "channel_id", "原始日志的 channel_id 是成本估算的输入，不能丢")
	col := map[string]int{}
	for i, h := range headers {
		col[h] = i
	}
	require.Len(t, rows, 2)
	assert.Equal(t, "73917", rows[0][col["channel_id"]])
	assert.Equal(t, "58201", rows[1][col["channel_id"]])
}

// 没有 other 不等于是脱敏日志：手工导出的日志同样没有 other。
// 脱敏日志的判据还要求带 uncached_input_tokens（只有脱敏才会展开出来的明细列），
// 缺了它就按原样合并——误删一个有用的 channel_id，比放过一个非脱敏文件的代价更难察觉。
func TestMergeNonSanitizedLogWithoutOtherKeepsChannelID(t *testing.T) {
	head := "id\tmodel_name\tquota\tchannel_id\n"
	res, headers, rows := mergeFiles(t, head+"1\tm1\t100\t73917\n", head+"2\tm2\t200\t58201\n")

	assert.Empty(t, res.DroppedColumns)
	require.Contains(t, headers, "channel_id")
	assert.Len(t, rows, 2)
}
