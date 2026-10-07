package billing

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sanitizedTestHeaders 是最小化的原始日志表头：包含 other 与 4 个缓存列（会被脱敏丢弃），
// 以及若干会被保留的普通列。
var sanitizedTestHeaders = []string{"model_name", "group", "prompt_tokens", "completion_tokens", "quota", "other", "cache_tokens", "cache_creation_tokens", "cache_creation_tokens_5m", "cache_creation_tokens_1h", "created_at"}

// wantSanitizedDetailHeaders 对应 3.1 节契约的表头顺序（不含计费参数列）。
var wantSanitizedDetailHeaders = []string{
	"uncached_input_tokens", "input_tokens_total", "usage_semantic", "cache_write_tokens",
	"text_input_tokens", "text_output_tokens", "audio_input_tokens", "audio_output_tokens",
	"image_output_tokens", "reasoning_tokens", "web_search_calls", "tool_surcharges",
}

// wantSanitizedBillingHeaders 对应 3.2 节可选计费参数列顺序。
var wantSanitizedBillingHeaders = []string{
	"model_ratio", "completion_ratio", "group_ratio", "user_group_ratio",
	"cache_ratio", "cache_creation_ratio", "cache_creation_ratio_5m", "cache_creation_ratio_1h",
	"model_price", "billing_mode", "matched_tier", "pre_consumed_quota", "actual_quota",
}

const sanitizedTestOtherFull = `{"usage_semantic":"anthropic","input_tokens_total":1300,"cache_write_tokens":250,` +
	`"text_input":10,"text_output":20,"audio_input":5,"audio_output":6,"image_output":7,` +
	`"completion_tokens_details":{"reasoning_tokens":42},` +
	`"tool_surcharges":[{"name":"web_search","count":2,"price":10}],` +
	`"model_ratio":1.5,"group_ratio":0.8,"billing_mode":"token","matched_tier":"base",` +
	`"pre_consumed_quota":100,"actual_quota":90}`

func sanitizedTestRows() [][]string {
	rowFull := []string{"claude-sonnet-5", "default", "1300", "200", "50000", sanitizedTestOtherFull, "10000", "5000", "200", "50", "1755000000"}
	rowEmpty := []string{"gpt-5.4", "vip", "300000", "50000", "20000", "", "0", "0", "0", "0", "1755000000"}
	return [][]string{rowFull, rowEmpty}
}

// TestSanitizedHeadersContract 断言表头顺序：含/不含计费参数列两种情况，且 other 不在其中。
func TestSanitizedHeadersContract(t *testing.T) {
	wantBase := []string{"model_name", "group", "prompt_tokens", "completion_tokens", "quota", "created_at"}

	withoutBilling := buildSanitizedHeaders(sanitizedTestHeaders, false)
	want := append(append([]string{}, wantBase...), SanitizedCacheColumns...)
	want = append(want, wantSanitizedDetailHeaders...)
	assert.Equal(t, want, withoutBilling)
	assert.NotContains(t, withoutBilling, "other")

	withBilling := buildSanitizedHeaders(sanitizedTestHeaders, true)
	wantWithBilling := append(append([]string{}, want...), wantSanitizedBillingHeaders...)
	assert.Equal(t, wantWithBilling, withBilling)
}

// TestCSVSanitizedWriterDetailColumns 覆盖 tsv 写出：首行有值、次行全空，
// 验证新列取值与 other 原字段一致，且 cache_creation_tokens 仍是 5m+1h 合计。
func TestCSVSanitizedWriterDetailColumns(t *testing.T) {
	for _, includeBilling := range []bool{false, true} {
		dir := t.TempDir()
		path := filepath.Join(dir, "脱敏日志.tsv")
		w, err := NewCSVSanitizedWriter(path, sanitizedTestHeaders, '\t', includeBilling)
		require.NoError(t, err)

		rows := sanitizedTestRows()
		d0 := ParseRowDetails(sanitizedTestOtherFull, includeBilling)
		d0.UncachedInputTokens = 1234
		d0.WebSearchCalls = 2
		require.NoError(t, w.WriteRow(rows[0], 10000, 200, 50, d0))

		d1 := ParseRowDetails("", includeBilling)
		d1.UncachedInputTokens = 0
		d1.WebSearchCalls = 0
		require.NoError(t, w.WriteRow(rows[1], 0, 0, 0, d1))
		require.NoError(t, w.Close())

		headers, outRows, err := LoadLogRows(path, "", "")
		require.NoError(t, err)
		require.NotContains(t, headers, "other")

		col := map[string]int{}
		for i, h := range headers {
			col[h] = i
		}

		require.Len(t, outRows, 2)
		full, empty := outRows[0], outRows[1]

		assert.Equal(t, "10000", full[col["cache_tokens"]])
		assert.Equal(t, "250", full[col["cache_creation_tokens"]], "5m+1h 合计")
		assert.Equal(t, "200", full[col["cache_creation_tokens_5m"]])
		assert.Equal(t, "50", full[col["cache_creation_tokens_1h"]])

		assert.Equal(t, "1234", full[col["uncached_input_tokens"]])
		assert.Equal(t, "1300", full[col["input_tokens_total"]])
		assert.Equal(t, "anthropic", full[col["usage_semantic"]])
		assert.Equal(t, "250", full[col["cache_write_tokens"]])
		assert.Equal(t, "10", full[col["text_input_tokens"]])
		assert.Equal(t, "20", full[col["text_output_tokens"]])
		assert.Equal(t, "5", full[col["audio_input_tokens"]])
		assert.Equal(t, "6", full[col["audio_output_tokens"]])
		assert.Equal(t, "7", full[col["image_output_tokens"]])
		assert.Equal(t, "42", full[col["reasoning_tokens"]])
		assert.Equal(t, "2", full[col["web_search_calls"]])
		assert.Equal(t, "web_search×2@10", full[col["tool_surcharges"]])

		// 全空行：计费口径字段写 0，其余缺字段留空（csv 空串）。
		assert.Equal(t, "0", empty[col["cache_tokens"]])
		assert.Equal(t, "0", empty[col["uncached_input_tokens"]])
		assert.Equal(t, "0", empty[col["web_search_calls"]])
		assert.Equal(t, "", empty[col["input_tokens_total"]])
		assert.Equal(t, "", empty[col["usage_semantic"]])
		assert.Equal(t, "", empty[col["tool_surcharges"]])

		if includeBilling {
			assert.Equal(t, "1.5", full[col["model_ratio"]])
			assert.Equal(t, "0.8", full[col["group_ratio"]])
			assert.Equal(t, "token", full[col["billing_mode"]])
			assert.Equal(t, "base", full[col["matched_tier"]])
			assert.Equal(t, "100", full[col["pre_consumed_quota"]])
			assert.Equal(t, "90", full[col["actual_quota"]])
			assert.Equal(t, "", empty[col["model_ratio"]])
		} else {
			_, hasModelRatio := col["model_ratio"]
			assert.False(t, hasModelRatio, "未开启计费参数时不应出现计费列")
		}
	}
}

// TestExcelSanitizedWriterDetailColumns 覆盖 xlsx 写出，断言与 tsv 一致的取值与表头收尾。
func TestExcelSanitizedWriterDetailColumns(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "脱敏日志.xlsx")
	w, err := NewExcelSanitizedWriter(path, sanitizedTestHeaders, false)
	require.NoError(t, err)

	rows := sanitizedTestRows()
	d0 := ParseRowDetails(sanitizedTestOtherFull, false)
	d0.UncachedInputTokens = 1234
	d0.WebSearchCalls = 2
	require.NoError(t, w.WriteRow(rows[0], 10000, 200, 50, d0))

	d1 := ParseRowDetails("", false)
	require.NoError(t, w.WriteRow(rows[1], 0, 0, 0, d1))
	require.NoError(t, w.Close())

	headers, outRows, err := LoadLogRows(path, "", "")
	require.NoError(t, err)
	require.NotContains(t, headers, "other")
	col := map[string]int{}
	for i, h := range headers {
		col[h] = i
	}
	require.Len(t, outRows, 2)
	assert.Equal(t, "250", outRows[0][col["cache_creation_tokens"]])
	assert.Equal(t, "1234", outRows[0][col["uncached_input_tokens"]])
	assert.Equal(t, "anthropic", outRows[0][col["usage_semantic"]])
	assert.Equal(t, "", outRows[1][col["usage_semantic"]])
}

// TestSanitizedHeadersDropChannelID 渠道号列不进脱敏日志，其余列的表头顺序不变。
func TestSanitizedHeadersDropChannelID(t *testing.T) {
	headers := []string{"id", "model_name", "request_id", "other", "channel_id",
		"cache_tokens", "cache_creation_tokens", "cache_creation_tokens_5m", "cache_creation_tokens_1h"}

	got := buildSanitizedHeaders(headers, false)

	assert.NotContains(t, got, "channel_id")
	assert.NotContains(t, got, "other")
	want := append([]string{"id", "model_name", "request_id"}, SanitizedColumns(false)...)
	assert.Equal(t, want, got, "丢掉 channel_id 不能连带改变别的列的顺序")
}

// TestSanitizedWritersStayAlignedWithoutChannelID 两种写出器丢掉 channel_id 之后，
// 排在它**后面**的列取值没有错位。
//
// 表头与每行的取值是两处各自过滤出来的（buildSanitizedHeaders 与 WriteRow），
// 靠的是同一张 SanitizedDropColumns 表。要是哪天其中一处改成了别的判据，
// 表头少一列而取值没少（或反过来），后面的列会整体错一格——数据照样写得出来、
// 也不报错，只是 quota 那一列里装着别的东西。所以拿 channel_id 后面的 quota 来对。
func TestSanitizedWritersStayAlignedWithoutChannelID(t *testing.T) {
	headers := []string{"id", "model_name", "request_id", "other", "channel_id", "quota"}
	other := `{"admin_info":{"use_channel":["73917"]},"usage_semantic":"anthropic"}`
	row := []string{"1001", "claude-sonnet-5", "req-1", other, "73917", "50000"}

	cases := []struct {
		name string
		file string
		open func(path string) (SanitizedWriter, error)
	}{
		{"xlsx", "脱敏日志.xlsx", func(p string) (SanitizedWriter, error) {
			return NewExcelSanitizedWriter(p, headers, false)
		}},
		{"tsv", "脱敏日志.tsv", func(p string) (SanitizedWriter, error) {
			return NewCSVSanitizedWriter(p, headers, '\t', false)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), tc.file)
			w, err := tc.open(path)
			require.NoError(t, err)
			require.NoError(t, w.WriteRow(row, 0, 0, 0, ParseRowDetails(other, false)))
			require.NoError(t, w.Close())

			gotHeaders, rows, err := LoadLogRows(path, "", "")
			require.NoError(t, err)
			require.Len(t, rows, 1)
			col := map[string]int{}
			for i, h := range gotHeaders {
				col[h] = i
			}

			assert.NotContains(t, gotHeaders, "channel_id")
			assert.Equal(t, "1001", rows[0][col["id"]])
			assert.Equal(t, "req-1", rows[0][col["request_id"]], "channel_id 前面的列")
			assert.Equal(t, "50000", rows[0][col["quota"]], "channel_id 后面的列：错位时最先坏在这里")
			assert.False(t, fileContains(t, path, "73917"), "渠道号不得出现在产物任何位置")
		})
	}
}
