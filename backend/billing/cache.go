package billing

import (
	"encoding/json"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// ExtractCacheFields 从 other/upstream_request_id 之类的文本字段中容错提取
// cache_creation_tokens / cache_tokens。对应 extract_cache_columns.py 的
// extract_cache_fields：依次尝试原文、URL 解码、去转义，再尝试整体 JSON、
// 嵌入式 JSON、最后正则兜底。
func ExtractCacheFields(value string) (cacheCreation *int64, cacheTokens *int64) {
	text := strings.TrimSpace(value)
	if text == "" {
		return nil, nil
	}

	for _, candidate := range iterTextCandidates(text) {
		parsed, ok := parseJSONValue(candidate)
		if !ok {
			parsed, ok = parseEmbeddedJSON(candidate)
		}
		if ok {
			cc, ct := extractFromParsedObject(parsed)
			if cc != nil || ct != nil {
				return cc, ct
			}
			if s, isStr := parsed.(string); isStr {
				nestedCC, nestedCT := ExtractCacheFields(s)
				if nestedCC != nil || nestedCT != nil {
					return nestedCC, nestedCT
				}
			}
		}

		cc, _ := extractByRegex(candidate, CacheCreationColumnName)
		ct, _ := extractByRegex(candidate, CacheTokensColumnName)
		if cc != nil || ct != nil {
			return cc, ct
		}
	}
	return nil, nil
}

func iterTextCandidates(text string) []string {
	candidates := []string{text}
	if decoded, err := url.QueryUnescape(text); err == nil && decoded != text {
		candidates = append(candidates, decoded)
	}
	for _, c := range append([]string(nil), candidates...) {
		unescaped := strings.ReplaceAll(c, `\"`, `"`)
		unescaped = strings.ReplaceAll(unescaped, `\'`, `'`)
		if unescaped != c {
			candidates = append(candidates, unescaped)
		}
	}
	seen := make(map[string]bool, len(candidates))
	unique := make([]string, 0, len(candidates))
	for _, c := range candidates {
		if !seen[c] {
			seen[c] = true
			unique = append(unique, c)
		}
	}
	return unique
}

// parseJSONValue 解析整段文本为 JSON；若结果是字符串，再尝试二次解析（双重编码）。
func parseJSONValue(text string) (interface{}, bool) {
	var parsed interface{}
	if err := json.Unmarshal([]byte(text), &parsed); err != nil {
		return nil, false
	}
	if s, ok := parsed.(string); ok {
		var nested interface{}
		if err := json.Unmarshal([]byte(s), &nested); err == nil {
			return nested, true
		}
		return s, true
	}
	return parsed, true
}

// parseEmbeddedJSON 从周围文本中截取第一个 '{' 到最后一个 '}' 再解析。
func parseEmbeddedJSON(text string) (interface{}, bool) {
	start := strings.Index(text, "{")
	end := strings.LastIndex(text, "}")
	if start < 0 || end <= start {
		return nil, false
	}
	return parseJSONValue(text[start : end+1])
}

func extractFromParsedObject(data interface{}) (cc *int64, ct *int64) {
	if v, ok := findKeyRecursive(data, CacheCreationColumnName); ok {
		if n, ok2 := parseNumber(v); ok2 {
			cc = &n
		}
	}
	if v, ok := findKeyRecursive(data, CacheTokensColumnName); ok {
		if n, ok2 := parseNumber(v); ok2 {
			ct = &n
		}
	}
	return
}

func findKeyRecursive(data interface{}, key string) (interface{}, bool) {
	switch t := data.(type) {
	case map[string]interface{}:
		if v, ok := t[key]; ok {
			return v, true
		}
		for _, v := range t {
			if found, ok := findKeyRecursive(v, key); ok {
				return found, true
			}
		}
	case []interface{}:
		for _, item := range t {
			if found, ok := findKeyRecursive(item, key); ok {
				return found, true
			}
		}
	}
	return nil, false
}

var cacheFieldRegexCache = map[string]*regexp.Regexp{}

func extractByRegex(text, key string) (*int64, bool) {
	q := regexp.QuoteMeta(key)
	patterns := []string{
		`"` + q + `"\s*:\s*"?(-?\d+(?:\.\d+)?)"?`,
		`'` + q + `'\s*:\s*'?(-?\d+(?:\.\d+)?)'?`,
		`\\?"` + q + `\\?"\s*:\s*\\?"?(-?\d+(?:\.\d+)?)\\?"?`,
		q + `\s*[=:]\s*'?"?(-?\d+(?:\.\d+)?)'?"?`,
	}
	for _, p := range patterns {
		re := cacheFieldRegexCache[p]
		if re == nil {
			re = regexp.MustCompile(p)
			cacheFieldRegexCache[p] = re
		}
		if m := re.FindStringSubmatch(text); m != nil {
			if n, ok := parseNumber(m[1]); ok {
				return &n, true
			}
		}
	}
	return nil, false
}

// parseNumber 对应 parse_number：bool 视为不可用，字符串去引号后按浮点解析再截断为整数。
func parseNumber(value interface{}) (int64, bool) {
	switch t := value.(type) {
	case nil:
		return 0, false
	case bool:
		return 0, false
	case float64:
		return int64(t), true
	case string:
		s := strings.Trim(strings.TrimSpace(t), `"'`)
		if s == "" {
			return 0, false
		}
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return 0, false
		}
		return int64(f), true
	default:
		return 0, false
	}
}
