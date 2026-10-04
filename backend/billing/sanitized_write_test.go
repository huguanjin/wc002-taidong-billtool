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
