package billing

import (
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/expr-lang/expr"
	"github.com/expr-lang/expr/ast"
	"github.com/expr-lang/expr/vm"
)

// BillingModeRatio / BillingModeTieredExpr 是上游 options 表 billing_setting.billing_mode 的取值。
const (
	BillingModeRatio      = "ratio"
	BillingModeTieredExpr = "tiered_expr"
)

// BillingExprSetting 业务的阶梯表达式配置，来自 options 表
// billing_setting.billing_expr（model → 表达式）与 billing_setting.billing_mode。
type BillingExprSetting struct {
	Exprs map[string]string
	Modes map[string]string
}

func newBillingExprSetting() *BillingExprSetting {
	return &BillingExprSetting{Exprs: map[string]string{}}
}

// Expr 返回模型的阶梯表达式；没有配置时返回 ""。
func (s *BillingExprSetting) Expr(model string) string {
	if s == nil {
		return ""
	}
	return s.Exprs[model]
}

// HasExpr 模型是否配置了阶梯表达式。
func (s *BillingExprSetting) HasExpr(model string) bool {
	return s.Expr(model) != ""
}

// ExprParams 一次请求的计费入参，字段含义与上游 pkg/billingexpr.TokenParams 对齐。
//
// P / C 是「用于定价的」输入、输出 token（已按表达式实际引用的子类变量扣减）；
// Len 是用于阶梯条件判断的输入上下文长度，永不被扣减。
type ExprParams struct {
	P, C float64
	Len  float64
	CR   float64
	CC   float64
	CC1h float64
	Img  float64
	ImgO float64
	AI   float64
	AO   float64
}

// ExprResult 表达式求值结果。
type ExprResult struct {
	// USD 该请求的官方美金刊例（表达式系数本身就是 $/MTok）。
	USD float64
	// MatchedTier 表达式里 tier() 命中的档位名；未调用 tier() 时为空。
	MatchedTier string
}

type compiledExpr struct {
	prog     *vm.Program
	usedVars map[string]bool
}

var (
	exprCacheMu sync.RWMutex
	exprCache   = map[string]*compiledExpr{}
)

// compileExpr 编译表达式（带缓存），并抽取表达式中实际引用的变量名，
// 后者用于决定哪些子类 token 需要从 p/c 中扣减。
func compileExpr(exprStr string) (*compiledExpr, error) {
	exprCacheMu.RLock()
	if c, ok := exprCache[exprStr]; ok {
		exprCacheMu.RUnlock()
		return c, nil
	}
	exprCacheMu.RUnlock()

	prog, err := expr.Compile(exprStr, expr.Env(exprCompileEnv()), expr.AsFloat64())
	if err != nil {
		return nil, fmt.Errorf("编译计费表达式失败: %w", err)
	}
	c := &compiledExpr{prog: prog, usedVars: extractUsedVars(prog)}

	exprCacheMu.Lock()
	if len(exprCache) >= 256 {
		exprCache = map[string]*compiledExpr{}
	}
	exprCache[exprStr] = c
	exprCacheMu.Unlock()
	return c, nil
}

// extractUsedVars 遍历编译后的 AST，收集表达式实际引用到的标识符（变量名）。
func extractUsedVars(prog *vm.Program) map[string]bool {
	vars := map[string]bool{}
	ast.Find(prog.Node(), func(n ast.Node) bool {
		if id, ok := n.(*ast.IdentifierNode); ok {
			vars[id.Value] = true
		}
		return false
	})
	return vars
}

// exprCompileEnv 编译期类型环境。日志出账时没有请求体与请求头，
// param/header/has 只做占位，避免解析期就因未知函数失败。
func exprCompileEnv() map[string]interface{} {
	return map[string]interface{}{
		"p": 0.0, "c": 0.0, "len": 0.0, "cr": 0.0, "cc": 0.0, "cc1h": 0.0,
		"img": 0.0, "img_o": 0.0, "ai": 0.0, "ao": 0.0,
		"tier":                   func(string, float64) float64 { return 0 },
		"header":                 func(string) string { return "" },
		"param":                  func(string) interface{} { return nil },
		"has":                    func(interface{}, string) bool { return false },
		"hour":                   func(string) int { return 0 },
		"minute":                 func(string) int { return 0 },
		"weekday":                func(string) int { return 0 },
		"month":                  func(string) int { return 0 },
		"day":                    func(string) int { return 0 },
		"max":                    math.Max,
		"min":                    math.Min,
		"abs":                    math.Abs,
		"ceil":                   math.Ceil,
		"floor":                  math.Floor,
	}
}

// RunBillingExpr 执行表达式，返回美金刊例与命中档位。
//
// at 是本次请求的发生时间，用作 hour()/weekday() 等时间函数的取值基准：出账面对的是
// 历史日志，必须按请求当时的时刻判断，用 time.Now() 会把「运行出账脚本的时间」当成
// 请求时间，凡带时段系数的表达式都会算错。上游是实时计费，用 time.Now() 无此问题。
func RunBillingExpr(exprStr string, params ExprParams, at time.Time) (ExprResult, error) {
	c, err := compileExpr(exprStr)
	if err != nil {
		return ExprResult{}, err
	}

	res := ExprResult{}
	env := map[string]interface{}{
		"p": params.P, "c": params.C, "len": params.Len,
		"cr": params.CR, "cc": params.CC, "cc1h": params.CC1h,
		"img": params.Img, "img_o": params.ImgO,
		"ai": params.AI, "ao": params.AO,
		"tier": func(name string, value float64) float64 {
			res.MatchedTier = name
			return value
		},
		// 出账场景没有原始请求体/请求头，param/header 一律返回空，
		// has() 仍按上游语义对 nil 返回 false。
		"header": func(string) string { return "" },
		"param":  func(string) interface{} { return nil },
		"has": func(source interface{}, substr string) bool {
			if source == nil || substr == "" {
				return false
			}
			return strings.Contains(fmt.Sprint(source), substr)
		},
		"hour": func(tz string) int { return inZone(at, tz).Hour() },
		"minute": func(tz string) int {
			return inZone(at, tz).Minute()
		},
		"weekday": func(tz string) int { return int(inZone(at, tz).Weekday()) },
		"month":   func(tz string) int { return int(inZone(at, tz).Month()) },
		"day":     func(tz string) int { return inZone(at, tz).Day() },
		"max":     math.Max, "min": math.Min, "abs": math.Abs,
		"ceil": math.Ceil, "floor": math.Floor,
	}

	out, err := expr.Run(c.prog, env)
	if err != nil {
		return ExprResult{}, fmt.Errorf("执行计费表达式失败: %w", err)
	}
	f, ok := out.(float64)
	if !ok {
		return ExprResult{}, fmt.Errorf("计费表达式返回 %T，期望 float64", out)
	}
	res.USD = f
	return res, nil
}

// inZone 把时刻转换到指定时区；时区名为空或非法时退回 UTC（与上游一致）。
func inZone(at time.Time, tz string) time.Time {
	tz = strings.TrimSpace(tz)
	if tz == "" {
		return at.UTC()
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return at.UTC()
	}
	return at.In(loc)
}

// BuildExprParams 把一条日志行的 token 明细整理成表达式入参。
//
// 自动扣减规则（与上游 service/tiered_settle.go 的 BuildTieredTokenParams 一致）：
// 表达式引用了某个子类变量时，该类 token 从 p/c 中扣除、单独按表达式里给出的单价计价；
// 未引用时它们留在 p/c 里按基础价计费。该规则只对 OpenAI/GPT 语义成立——
// 那种语义下 prompt_tokens 已经把缓存、图片等全含在内；Anthropic 语义的 input_tokens
// 本来就只是文本部分，不做扣减。
//
// promptTokens 是日志里的原始 prompt_tokens；uncached 是尚未扣除缓存的部分（Anthropic 语义
// 下等于 promptTokens）。调用方两者都传，由本函数按语义选择。
func BuildExprParams(model string, promptTokens, uncached, completion, cacheRead, cacheWrite5m, cacheWrite1h float64,
	img, imgO, ai, ao float64, exprStr string) ExprParams {

	c, err := compileExpr(exprStr)
	if err != nil {
		// 表达式编译失败时按「不引用任何子类变量」处理，全部 token 走基础价，
		// 数值上退化为普通按量计费，不会因为一处表达式错误丢掉整行价格。
		c = &compiledExpr{usedVars: map[string]bool{}}
	}
	used := c.usedVars

	isClaudeSemantic := InferUsageSemantic(model, "") == "anthropic"

	p := uncached
	comp := completion
	inputLen := promptTokens
	if isClaudeSemantic {
		inputLen = promptTokens + cacheRead + cacheWrite5m + cacheWrite1h
	} else {
		if used["cr"] {
			p -= cacheRead
		}
		if used["cc"] {
			p -= cacheWrite5m
		}
		if used["cc1h"] {
			p -= cacheWrite1h
		}
		if used["img"] {
			p -= img
		}
		if used["ai"] {
			p -= ai
		}
		if used["img_o"] {
			comp -= imgO
		}
		if used["ao"] {
			comp -= ao
		}
	}
	if p < 0 {
		p = 0
	}
	if comp < 0 {
		comp = 0
	}

	return ExprParams{
		P: p, C: comp, Len: inputLen,
		CR: cacheRead, CC: cacheWrite5m, CC1h: cacheWrite1h,
		Img: img, ImgO: imgO, AI: ai, AO: ao,
	}
}

// ExprQuota 表达式输出换算成站点 quota，用于与日志 quota 交叉校验。
// quota = 表达式结果 / 1_000_000 * 每单位额度 * groupRatio。
func ExprQuota(usd, groupRatio float64) float64 {
	return usd / 1_000_000 * QuotaPerCNY * groupRatio
}

// ExprRates 表达式在某个上下文长度下的等效单价（美金/百万 token）。
type ExprRates struct {
	InputPerM      float64
	OutputPerM     float64
	CacheReadPerM  float64
	CacheWritePerM float64
	// CacheWrite1hPerM 表达式未引用 cc1h 时为 0——那表示 1 小时缓存不回显单价，
	// 不能拿 5 分钟单价去乘一个固定倍数（并非所有模型都按 2 倍计）。
	CacheWrite1hPerM float64
	// Pure 表达式是否只由 token 变量线性构成（无 img/ai/ao 等附加项、无常量）。
	// 为 false 时上面的单价不足以还原表达式金额，调用方应直接用表达式算出的金额。
	Pure bool
}

// ExtractExprRates 求表达式在给定上下文长度下的等效单价，用于账单里的单价列。
//
// 做法是把表达式在若干「单变量置 1、其余置 0」的基准点上求值，再取差：
// 每次求值都走完整的档位判断与 hour() 等条件，所以多档表达式、带时段倍率的表达式
// 都能给出该上下文长度下真正生效的那一档单价，而不必去解析表达式文本。
//
// 表达式含 img/ai/ao 这类附加项时，这部分无法用每百万 token 单价表达，
// Pure 置为 false。
func ExtractExprRates(exprStr string, at time.Time, inputLen float64) (ExprRates, error) {
	c, err := compileExpr(exprStr)
	if err != nil {
		return ExprRates{}, err
	}

	eval := func(p, cOut, cr, cc, cc1h float64) (float64, error) {
		env := map[string]interface{}{
			"p": p, "c": cOut, "len": inputLen,
			"cr": cr, "cc": cc, "cc1h": cc1h,
			"img": 0.0, "img_o": 0.0, "ai": 0.0, "ao": 0.0,
			"tier":   func(_ string, v float64) float64 { return v },
			"header": func(string) string { return "" },
			"param":  func(string) interface{} { return nil },
			"has":    func(interface{}, string) bool { return false },
			"hour":   func(tz string) int { return inZone(at, tz).Hour() },
			"minute": func(tz string) int { return inZone(at, tz).Minute() },
			"weekday": func(tz string) int {
				return int(inZone(at, tz).Weekday())
			},
			"month": func(tz string) int { return int(inZone(at, tz).Month()) },
			"day":   func(tz string) int { return inZone(at, tz).Day() },
			"max":   math.Max, "min": math.Min, "abs": math.Abs,
			"ceil": math.Ceil, "floor": math.Floor,
		}
		out, err := expr.Run(c.prog, env)
		if err != nil {
			return 0, err
		}
		f, ok := out.(float64)
		if !ok {
			return 0, fmt.Errorf("表达式返回 %T，期望 float64", out)
		}
		return f, nil
	}

	const unit = 1_000_000.0
	zero, err := eval(0, 0, 0, 0, 0)
	if err != nil {
		return ExprRates{}, err
	}
	// 每个变量单独置 unit（其余为 0），与零点的差再除回 unit，即该变量的系数。
	only := func(idx int) (float64, error) {
		args := [5]float64{}
		args[idx] = unit
		return eval(args[0], args[1], args[2], args[3], args[4])
	}

	coef := [5]float64{}
	for i := range coef {
		v, err := only(i)
		if err != nil {
			return ExprRates{}, err
		}
		coef[i] = (v - zero) / unit
	}

	rates := ExprRates{
		InputPerM:        coef[0],
		OutputPerM:       coef[1],
		CacheReadPerM:    coef[2],
		CacheWritePerM:   coef[3],
		CacheWrite1hPerM: coef[4],
		// 零点不为 0 说明表达式有与用量无关的常数项（如按次基础费），
		// 单价列无法体现，标记为非纯表达式。
		Pure: math.Abs(zero) < 1e-9,
	}
	if c.usedVars["img"] || c.usedVars["img_o"] || c.usedVars["ai"] || c.usedVars["ao"] {
		rates.Pure = false
	}
	return rates, nil
}
