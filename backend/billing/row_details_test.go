package billing

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestParseRowDetailsAnthropicClaude 覆盖 Claude 5m/1h 分档缓存 + 常见归一化字段的形态。
func TestParseRowDetailsAnthropicClaude(t *testing.T) {
	other := `{"usage_semantic":"anthropic","cache_tokens":1000,"cache_creation_tokens_5m":200,` +
		`"cache_creation_tokens_1h":50,"input_tokens_total":1250,"cache_write_tokens":250,` +
		`"web_search_call_count":2}`

	d := ParseRowDetails(other, false)
	assert.Equal(t, "anthropic", d.UsageSemantic)
	require.NotNil(t, d.InputTokensTotal)
	assert.Equal(t, 1250.0, *d.InputTokensTotal)
	require.NotNil(t, d.CacheWriteTokens)
	assert.Equal(t, 250.0, *d.CacheWriteTokens)
	assert.Equal(t, BillingDetails{}, d.Billing, "includeBilling=false 时 Billing 应为零值")
}

// TestParseRowDetailsOpenAINativeCacheWrite 覆盖 OpenAI 原生 cache_write_tokens 形态，
// 以及图片/音频/推理等明细字段。
func TestParseRowDetailsOpenAINativeCacheWrite(t *testing.T) {
	other := `{"usage_semantic":"openai","cache_write_tokens":300,"text_input":10,"text_output":20,` +
		`"audio_input":5,"audio_output":6,"image_output":7,` +
		`"completion_tokens_details":{"reasoning_tokens":42}}`

	d := ParseRowDetails(other, false)
	assert.Equal(t, "openai", d.UsageSemantic)
	require.NotNil(t, d.CacheWriteTokens)
	assert.Equal(t, 300.0, *d.CacheWriteTokens)
	require.NotNil(t, d.TextInput)
	assert.Equal(t, 10.0, *d.TextInput)
	require.NotNil(t, d.TextOutput)
	assert.Equal(t, 20.0, *d.TextOutput)
	require.NotNil(t, d.AudioInput)
	assert.Equal(t, 5.0, *d.AudioInput)
	require.NotNil(t, d.AudioOutput)
	assert.Equal(t, 6.0, *d.AudioOutput)
	require.NotNil(t, d.ImageOutput)
	assert.Equal(t, 7.0, *d.ImageOutput)
	require.NotNil(t, d.ReasoningTokens)
	assert.Equal(t, 42.0, *d.ReasoningTokens)
}

// TestParseRowDetailsAudioInputFallback 覆盖 audio_input 为 0 时回退 audio_input_token_count。
func TestParseRowDetailsAudioInputFallback(t *testing.T) {
	d := ParseRowDetails(`{"audio_input":0,"audio_input_token_count":15}`, false)
	require.NotNil(t, d.AudioInput)
	assert.Equal(t, 15.0, *d.AudioInput)
}

// TestParseRowDetailsDoubleEncodedAndURLEncoded 覆盖双重编码 JSON（外层是带引号的字符串）
// 与 URL 编码形态：json.Unmarshal 到 map 会失败，函数必须返回零值，不得 panic、不得报错。
func TestParseRowDetailsDoubleEncodedAndURLEncoded(t *testing.T) {
	cases := []string{
		`"{\"cache_tokens\":1000}"`,    // 双重编码：整体是一个 JSON 字符串字面量
		`%7B%22cache_tokens%22%3A5%7D`, // URL 编码，不是合法 JSON
	}
	for _, other := range cases {
		require.NotPanics(t, func() {
			d := ParseRowDetails(other, false)
			assert.Equal(t, RowDetails{}, d)
		})
	}
}

// TestParseRowDetailsNotJSON 覆盖完全不是 JSON 的 other，走容错路径：不得 panic、不得报错。
func TestParseRowDetailsNotJSON(t *testing.T) {
	require.NotPanics(t, func() {
		d := ParseRowDetails("not-json-at-all", false)
		assert.Equal(t, RowDetails{}, d)
	})
}

// TestParseRowDetailsEmptyString 覆盖空字符串，返回零值。
func TestParseRowDetailsEmptyString(t *testing.T) {
	assert.Equal(t, RowDetails{}, ParseRowDetails("", false))
	assert.Equal(t, RowDetails{}, ParseRowDetails("   ", false))
}

// TestParseRowDetailsToolSurcharges 覆盖 tool_surcharges 单元素/多元素/空数组。
func TestParseRowDetailsToolSurcharges(t *testing.T) {
	single := ParseRowDetails(`{"tool_surcharges":[{"name":"web_search","count":1,"price":10}]}`, false)
	assert.Equal(t, "web_search×1@10", single.ToolSurcharges)

	multi := ParseRowDetails(`{"tool_surcharges":[{"name":"web_search","count":1,"price":10},`+
		`{"name":"image_gen","count":2,"price":5}]}`, false)
	assert.Equal(t, "web_search×1@10;image_gen×2@5", multi.ToolSurcharges)

	empty := ParseRowDetails(`{"tool_surcharges":[]}`, false)
	assert.Equal(t, "", empty.ToolSurcharges)

	missingField := ParseRowDetails(`{"tool_surcharges":[{"name":"web_search","count":1}]}`, false)
	assert.Equal(t, "", missingField.ToolSurcharges, "count/price 缺失的元素应被跳过")
}

// TestParseRowDetailsIncludeBilling 覆盖 includeBilling 开关：关闭时 Billing 始终为零值，
// 开启时按 other 同名键填充。
func TestParseRowDetailsIncludeBilling(t *testing.T) {
	other := `{"model_ratio":1.5,"group_ratio":0.8,"billing_mode":"token","matched_tier":"base",` +
		`"pre_consumed_quota":100,"actual_quota":90}`

	off := ParseRowDetails(other, false)
	assert.Equal(t, BillingDetails{}, off.Billing)

	on := ParseRowDetails(other, true)
	require.NotNil(t, on.Billing.ModelRatio)
	assert.Equal(t, 1.5, *on.Billing.ModelRatio)
	require.NotNil(t, on.Billing.GroupRatio)
	assert.Equal(t, 0.8, *on.Billing.GroupRatio)
	assert.Equal(t, "token", on.Billing.BillingMode)
	assert.Equal(t, "base", on.Billing.MatchedTier)
	require.NotNil(t, on.Billing.PreConsumedQuota)
	assert.Equal(t, 100.0, *on.Billing.PreConsumedQuota)
	require.NotNil(t, on.Billing.ActualQuota)
	assert.Equal(t, 90.0, *on.Billing.ActualQuota)
}
