package billing

import (
	"math"
	"testing"
	"time"
)

// 表达式取自线上 options 表 billing_setting.billing_expr 的真实配置，
// 系数本身就是「美金/百万 token」，这里核对解析、档位判断与单价折算是否一致。
const (
	exprGPT54 = `len < 272000 ? tier("base", p * 2.5 + c * 15 + cr * 0.25) : tier("tier_2", p * 5 + c * 22.5 + cr * 0.5)`
	exprSol   = `len < 272000 ? tier("tier_1", p * 4 + c * 20 + cr * 0.4 + cc * 5) : tier("tier_2", p * 8 + c * 30 + cr * 0.8 + cc * 10)`
	exprGlm5  = `len < 32000 ? tier("short_input", p * 4 + c * 18) : tier("long_input", p * 6 + c * 22)`
	// 带时段倍率的表达式：9-12 点与 14-18 点各翻倍。
	exprDeepSeek = `(tier("default", p * 1 + c * 4 + cr * 0.02)) * (hour("Asia/Shanghai") >= 9 && hour("Asia/Shanghai") < 12 ? 2 : 1) * (hour("Asia/Shanghai") >= 14 && hour("Asia/Shanghai") < 18 ? 2 : 1)`
)

func TestRunBillingExprTierSelection(t *testing.T) {
	at := time.Date(2026, 8, 1, 3, 0, 0, 0, time.UTC)

	// 系数是「美金/百万 token」，token 数按 1 计便于验算：
	// 上下文 100 < 27.2 万，命中低档：1×2.5 + 1×15 = 17.5 美金
	res, err := RunBillingExpr(exprGPT54, ExprParams{P: 1, C: 1, Len: 100}, at)
	if err != nil {
		t.Fatalf("RunBillingExpr 失败: %v", err)
	}
	if res.MatchedTier != "base" {
		t.Errorf("期望命中档位 base，实际 %q", res.MatchedTier)
	}
	if math.Abs(res.USD-17.5) > 1e-9 {
		t.Errorf("低档美金期望 17.5，实际 %v", res.USD)
	}

	// 上下文 30 万 >= 27.2 万，命中高档：5 + 22.5 = 27.5 美金
	res, err = RunBillingExpr(exprGPT54, ExprParams{P: 1, C: 1, Len: 300_000}, at)
	if err != nil {
		t.Fatalf("RunBillingExpr 失败: %v", err)
	}
	if res.MatchedTier != "tier_2" {
		t.Errorf("期望命中档位 tier_2，实际 %q", res.MatchedTier)
	}
	if math.Abs(res.USD-27.5) > 1e-9 {
		t.Errorf("高档美金期望 27.5，实际 %v", res.USD)
	}
}

// 台阶按 len（输入上下文长度）判断，而不是按用于定价的 p。校验 BuildExprParams
// 传进来的 Len 确实来自原始 prompt_tokens。
func TestBuildExprParamsLenAndAutoExclusion(t *testing.T) {
	// 表达式引用了 cr，缓存读 token 应从 p 中扣除
	params := BuildExprParams("gpt-5.4", 400_000, 400_000, 50_000, 100_000, 0, 0, 0, 0, 0, 0, exprGPT54)
	if params.Len != 400_000 {
		t.Errorf("Len 期望 400000，实际 %v", params.Len)
	}
	if params.P != 300_000 {
		t.Errorf("引用 cr 后期望 P=300000，实际 %v", params.P)
	}
	if params.CR != 100_000 {
		t.Errorf("CR 期望 100000，实际 %v", params.CR)
	}

	// glm-5 不引用 cr，缓存读留在 p 里按基础价计
	params = BuildExprParams("glm-5", 400_000, 400_000, 50_000, 100_000, 0, 0, 0, 0, 0, 0, exprGlm5)
	if params.P != 400_000 {
		t.Errorf("未引用 cr 时期望 P=400000，实际 %v", params.P)
	}
}

// Anthropic 语义下 input_tokens 本身只是文本部分，不再做扣减；len 计入缓存。
func TestBuildExprParamsClaudeSemantic(t *testing.T) {
	params := BuildExprParams("claude-sonnet-5", 400_000, 400_000, 50_000, 100_000, 1_000, 2_000, 0, 0, 0, 0, exprGPT54)
	if params.P != 400_000 {
		t.Errorf("Claude 语义下 P 期望 400000，实际 %v", params.P)
	}
	if params.Len != 503_000 {
		t.Errorf("Claude 语义下 Len 期望 503000（含缓存读写），实际 %v", params.Len)
	}
}

// 带 hour() 的表达式必须按请求发生时刻判断，而不是按运行出账脚本的时刻。
func TestRunBillingExprUsesRequestTime(t *testing.T) {
	base := ExprParams{P: 1, C: 0, Len: 1}

	// 北京时间 10:00 落在 9-12 点的翻倍区间，系数 ×2 → 2 美金
	peak := time.Date(2026, 8, 1, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	res, err := RunBillingExpr(exprDeepSeek, base, peak)
	if err != nil {
		t.Fatalf("RunBillingExpr 失败: %v", err)
	}
	if math.Abs(res.USD-2.0) > 1e-9 {
		t.Errorf("高峰时段美金期望 2.0，实际 %v", res.USD)
	}

	// 北京时间 13:00 两个区间都不占，系数 ×1 → 1 美金
	off := time.Date(2026, 8, 1, 13, 0, 0, 0, time.FixedZone("CST", 8*3600))
	res, err = RunBillingExpr(exprDeepSeek, base, off)
	if err != nil {
		t.Fatalf("RunBillingExpr 失败: %v", err)
	}
	if math.Abs(res.USD-1.0) > 1e-9 {
		t.Errorf("平峰时段美金期望 1.0，实际 %v", res.USD)
	}
}

func TestExtractExprRates(t *testing.T) {
	at := time.Date(2026, 8, 1, 3, 0, 0, 0, time.UTC)

	rates, err := ExtractExprRates(exprGPT54, at, 100_000)
	if err != nil {
		t.Fatalf("ExtractExprRates 失败: %v", err)
	}
	if rates.InputPerM != 2.5 || rates.OutputPerM != 15 || rates.CacheReadPerM != 0.25 {
		t.Errorf("低档单价解析不符: %+v", rates)
	}
	if !rates.Pure {
		t.Errorf("gpt-5.4 表达式应对应纯单价")
	}

	// 上下文变长后，同一表达式应给出高档单价
	rates, err = ExtractExprRates(exprGPT54, at, 300_000)
	if err != nil {
		t.Fatalf("ExtractExprRates 失败: %v", err)
	}
	if rates.InputPerM != 5 || rates.OutputPerM != 22.5 || rates.CacheReadPerM != 0.5 {
		t.Errorf("高档单价解析不符: %+v", rates)
	}

	// cc 与 p 的系数不同（4 对 5），必须各自独立取出
	rates, err = ExtractExprRates(exprSol, at, 100_000)
	if err != nil {
		t.Fatalf("ExtractExprRates 失败: %v", err)
	}
	if rates.InputPerM != 4 || rates.CacheWritePerM != 5 {
		t.Errorf("gpt-5.6-sol 单价解析不符: %+v", rates)
	}

	// 未引用 cc1h 时 1 小时缓存单价为 0，不能拿 5 分钟单价乘固定倍数
	if rates.CacheWrite1hPerM != 0 {
		t.Errorf("表达式未引用 cc1h 时该单价期望 0，实际 %v", rates.CacheWrite1hPerM)
	}

	// 时段倍率会体现在折算出的单价上：10 点档口 ×2
	peak := time.Date(2026, 8, 1, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	rates, err = ExtractExprRates(exprDeepSeek, peak, 1000)
	if err != nil {
		t.Fatalf("ExtractExprRates 失败: %v", err)
	}
	if math.Abs(rates.InputPerM-2.0) > 1e-9 || math.Abs(rates.OutputPerM-8.0) > 1e-9 {
		t.Errorf("高峰时段等效单价期望 输入2/输出8，实际 %+v", rates)
	}

	// 含图片/音频附加项时无法折算成单价，应标记为非纯
	realtime := `tier("base", p * 4 + c * 24 + cr * 0.4 + img * 5 + ai * 32 + ao * 64)`
	rates, err = ExtractExprRates(realtime, at, 1000)
	if err != nil {
		t.Fatalf("ExtractExprRates 失败: %v", err)
	}
	if rates.Pure {
		t.Errorf("含图片/音频附加项的表达式不应标记为纯单价")
	}
}

// ParseBillingExpr 读的是上游写进日志 other 里的表达式，出账时优先采用它，
// 这样即使 options 表后来改过规则，历史账单仍按当时的规则复算。
func TestParseBillingExprFromLog(t *testing.T) {
	other := `{"billing_mode":"tiered_expr","expr_b64":"bGVuIDwgMjcyMDAwID8gdGllcigiYmFzZSIsIHAgKiAyLjUpIDogdGllcigidGllcl8yIiwgcCAqIDUp","matched_tier":"base"}`
	got := ParseBillingExpr(other)
	if got != `len < 272000 ? tier("base", p * 2.5) : tier("tier_2", p * 5)` {
		t.Errorf("表达式还原不符: %q", got)
	}
	if tier := ParseMatchedTier(other); tier != "base" {
		t.Errorf("命中档位期望 base，实际 %q", tier)
	}
	if ParseBillingExpr(`{"cache_tokens":1000}`) != "" {
		t.Errorf("日志无表达式时期望空串")
	}
}

// TestAggregateUsesOptionTableExpr 表达式来自 options 表（日志里没有 expr_b64）时，
// 也要按表达式出刊例，而不是回落到查不到条目的 ModelRatio/ModelPrice。
func TestAggregateUsesOptionTableExpr(t *testing.T) {
	headers := []string{"model_name", "group", "prompt_tokens", "completion_tokens", "quota", "other", "created_at"}
	rows := [][]string{
		// 200k 以内，命中第一档：p*2.5 + c*15 = 0.2*2.5 + 0.05*15 = 1.25 美金
		{"gpt-5.4", "default", "200000", "50000", "100", "", "1755000000"},
		// 超过 272k，命中第二档：p*5 + c*22.5 = 0.3*5 + 0.05*22.5 = 2.625 美金
		{"gpt-5.4", "default", "300000", "50000", "100", "", "1755000000"},
	}
	setting := &BillingExprSetting{Exprs: map[string]string{"gpt-5.4": exprGPT54}}

	result, err := AggregateFromRows(rows, headers, nil, 7.2, false, setting, nil)
	if err != nil {
		t.Fatalf("AggregateFromRows 失败: %v", err)
	}
	if len(result.Rows) != 1 {
		t.Fatalf("期望聚合成 1 行，实际 %d", len(result.Rows))
	}
	agg := result.Rows[0]
	if agg.BillingMode != BillingModeTieredExpr {
		t.Errorf("期望计费模式 %s，实际 %s", BillingModeTieredExpr, agg.BillingMode)
	}
	if want := 1.25 + 2.625; math.Abs(agg.OfficialUSD-want) > 1e-6 {
		t.Errorf("刊例美金期望 %v，实际 %v", want, agg.OfficialUSD)
	}
	// 两档都命中过，档位名应都留下
	if len(agg.ExprTiers) != 2 {
		t.Errorf("期望记录 2 个档位，实际 %v", agg.ExprTiers)
	}
	if !HasKnownListPrice(agg) {
		t.Errorf("表达式模型应算「有价」，HasKnownListPrice=false")
	}
}
