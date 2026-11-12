// Package agentcfg 提供「智能体一键配置」：把本机 AI 客户端指向本网关。
//
// 设计约束（照搬 qoder2api-hub 的四条铁律）：
//  1. 非侵入：文本级增量编辑 YAML/TOML/JSON，只动 qoder-proxy 那一段，
//     保留用户注释、缩进和其它 provider。
//  2. 可回滚：写入前把原件字节备份到 state 目录，restore 精确还原；
//     我们新建的文件在 restore 时删除并清理空目录。
//  3. 密钥安全：API Key 只落客户端本地配置；state 里只存 sha256 与备份路径。
//  4. plan 即 apply：Plan 与 Integrate 共享同一构建器，预览即真实写入。
package agentcfg

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// deepMerge 递归合并 JSON 对象；patch 之外的同级键存活，输入不被修改。
func deepMerge(base map[string]any, patch map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range base {
		out[k] = v
	}
	for k, v := range patch {
		if pv, ok := v.(map[string]any); ok {
			if bv, ok := out[k].(map[string]any); ok {
				out[k] = deepMerge(bv, pv)
				continue
			}
		}
		out[k] = v
	}
	return out
}

// ---- 文本级 YAML 编辑器（子集）----

var yamlKeyRE = regexp.MustCompile(`^([A-Za-z0-9_][A-Za-z0-9_.-]*)\s*:(?:\s|$)`)

func yamlIndent(line string) int {
	return len(line) - len(strings.TrimLeft(line, " "))
}

func yamlKey(line string) string {
	stripped := strings.TrimSpace(line)
	if stripped == "" || strings.HasPrefix(stripped, "#") || strings.HasPrefix(stripped, "-") {
		return ""
	}
	m := yamlKeyRE.FindStringSubmatch(strings.TrimLeft(line, " "))
	if m == nil {
		return ""
	}
	return m[1]
}

func yamlBlockEnd(lines []string, start, indent int) int {
	idx := start + 1
	for idx < len(lines) {
		line := lines[idx]
		if strings.TrimSpace(line) != "" && yamlIndent(line) <= indent {
			break
		}
		idx++
	}
	return idx
}

func yamlInsertPoint(lines []string, start, end int) int {
	idx := end
	for idx > start && strings.TrimSpace(lines[idx-1]) == "" {
		idx--
	}
	if idx > start {
		return idx
	}
	return end
}

// YAMLUpsertInline 就地 upsert 嵌套键（值为 JSON 字面量）。块外字节保持不变。
func YAMLUpsertInline(text, keyPath string, value any) (string, error) {
	var keys []string
	for _, part := range strings.Split(keyPath, ".") {
		if p := strings.TrimSpace(part); p != "" {
			keys = append(keys, p)
		}
	}
	if len(keys) == 0 {
		return "", fmt.Errorf("empty key path")
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	lines, err := yamlUpsertAt(strings.Split(text, "\n"), 0, -1, 0, keys, string(raw))
	if err != nil {
		return "", err
	}
	return strings.Join(lines, "\n"), nil
}

func yamlUpsertAt(lines []string, start, end, indent int, keys []string, valueJSON string) ([]string, error) {
	if end < 0 {
		end = len(lines)
	}
	key := keys[0]
	for idx := start; idx < end; idx++ {
		line := lines[idx]
		if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") ||
			yamlIndent(line) != indent || yamlKey(line) != key {
			continue
		}
		after := strings.TrimSpace(strings.SplitN(line, ":", 2)[1])
		childEnd := yamlBlockEnd(lines, idx, indent)
		if len(keys) == 1 {
			lines[idx] = strings.Repeat(" ", indent) + key + ": " + valueJSON
			return lines, nil
		}
		if after != "" {
			expanded, err := yamlInlineToBlock(lines, idx, key, after)
			if err != nil {
				return nil, err
			}
			lines = expanded
			childEnd = yamlBlockEnd(lines, idx, indent)
		}
		return yamlUpsertAt(lines, idx+1, childEnd, indent+2, keys[1:], valueJSON)
	}
	insert := yamlInsertPoint(lines, start, end)
	if len(keys) == 1 {
		return insertLines(lines, insert, []string{strings.Repeat(" ", indent) + key + ": " + valueJSON}), nil
	}
	newLines := []string{strings.Repeat(" ", indent) + key + ":"}
	child, err := yamlUpsertAt(nil, 0, -1, indent+2, keys[1:], valueJSON)
	if err != nil {
		return nil, err
	}
	return insertLines(lines, insert, append(newLines, child...)), nil
}
func insertLines(lines []string, at int, extra []string) []string {
	if at < 0 {
		at = 0
	}
	if at > len(lines) {
		at = len(lines)
	}
	out := make([]string, 0, len(lines)+len(extra))
	out = append(out, lines[:at]...)
	out = append(out, extra...)
	out = append(out, lines[at:]...)
	return out
}

func yamlInlineToBlock(lines []string, idx int, key, inlineValue string) ([]string, error) {
	indent := yamlIndent(lines[idx])
	value := strings.TrimSpace(inlineValue)
	if strings.HasPrefix(value, "{") && strings.HasSuffix(value, "}") {
		inner := strings.TrimSpace(value[1 : len(value)-1])
		out := []string{strings.Repeat(" ", indent) + key + ":"}
		if inner != "" {
			for _, part := range strings.Split(inner, ",") {
				if p := strings.TrimSpace(part); p != "" {
					out = append(out, strings.Repeat(" ", indent+2)+p)
				}
			}
		}
		replaced := append([]string{}, lines[:idx]...)
		replaced = append(replaced, out...)
		replaced = append(replaced, lines[idx+1:]...)
		return replaced, nil
	}
	if value == "" {
		return lines, nil
	}
	return nil, fmt.Errorf("cannot nest keys under `%s: %s` - only an inline {..} mapping can be expanded", key, inlineValue)
}

// yamlScalar 始终输出带双引号的 YAML 标量。YAML 会把裸数字/布尔/null 解析成
// 非字符串，而 DSH 的 credentials refs 对类型做硬校验。
func yamlScalar(v string) string {
	raw, _ := json.Marshal(v)
	return string(raw)
}

// YAMLRefsUpsert 在顶层 `refs:` 块内 upsert 一个键。
func YAMLRefsUpsert(text, key, value string) (string, error) {
	rendered := key + ": " + yamlScalar(value)
	lines := strings.Split(text, "\n")
	for idx := 0; idx < len(lines); idx++ {
		line := lines[idx]
		if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		if yamlIndent(line) != 0 || yamlKey(line) != "refs" {
			continue
		}
		if after := strings.TrimSpace(strings.SplitN(line, ":", 2)[1]); after != "" {
			return "", fmt.Errorf("cannot upsert into `refs: %s` - only a block-style refs mapping is supported", after)
		}
		end := yamlBlockEnd(lines, idx, 0)
		for j := idx + 1; j < end; j++ {
			if strings.TrimSpace(lines[j]) != "" && yamlIndent(lines[j]) == 2 && yamlKey(lines[j]) == key {
				lines[j] = "  " + rendered
				return strings.Join(lines, "\n"), nil
			}
		}
		insert := yamlInsertPoint(lines, idx+1, end)
		lines = insertLines(lines, insert, []string{"  " + rendered})
		return strings.Join(lines, "\n"), nil
	}
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	lines = append(lines, "refs:", "  "+rendered)
	return strings.Join(lines, "\n") + "\n", nil
}

// RefsValueIsQuotedScalar 写后自检：refs 块里该键必须是带双引号的标量。
func RefsValueIsQuotedScalar(text, key string) bool {
	for _, line := range strings.Split(text, "\n") {
		stripped := strings.TrimSpace(line)
		if !strings.HasPrefix(stripped, key+":") {
			continue
		}
		value := strings.TrimSpace(strings.SplitN(stripped, ":", 2)[1])
		return len(value) >= 2 && strings.HasPrefix(value, `"`) && strings.HasSuffix(value, `"`)
	}
	return false
}

// ---- 文本级 TOML 编辑器（子集）----

var (
	tomlTableRE = regexp.MustCompile(`^\s*\[([A-Za-z0-9_.-]+)\]\s*(?:#.*)?$`)
	tomlKeyRE   = regexp.MustCompile(`^\s*([A-Za-z0-9_-]+)\s*=`)
)

func tomlValue(v any) string {
	switch x := v.(type) {
	case bool:
		if x {
			return "true"
		}
		return "false"
	case int:
		return strconv.Itoa(x)
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	default:
		raw, _ := json.Marshal(fmt.Sprint(v))
		return string(raw)
	}
}

// TOMLUpsertTopKey upsert 顶层 `key = value`（写在第一个 [table] 前）。
func TOMLUpsertTopKey(text, key string, value any) string {
	lines := strings.Split(text, "\n")
	rendered := key + " = " + tomlValue(value)
	firstTable := len(lines)
	for idx := 0; idx < len(lines); idx++ {
		if tomlTableRE.MatchString(lines[idx]) {
			firstTable = idx
			break
		}
		if m := tomlKeyRE.FindStringSubmatch(lines[idx]); m != nil && m[1] == key {
			lines[idx] = rendered
			return strings.Join(lines, "\n")
		}
	}
	insert := firstTable
	for insert > 0 && strings.TrimSpace(lines[insert-1]) == "" {
		insert--
	}
	return strings.Join(insertLines(lines, insert, []string{rendered}), "\n")
}

// TOMLUpsertTable upsert `[table]` 下的键值（表不存在则建在文件末尾）。
func TOMLUpsertTable(text, table string, mapping map[string]any) string {
	lines := strings.Split(text, "\n")
	for idx := 0; idx < len(lines); idx++ {
		m := tomlTableRE.FindStringSubmatch(lines[idx])
		if m == nil || m[1] != table {
			continue
		}
		end := idx + 1
		for end < len(lines) && !tomlTableRE.MatchString(lines[end]) {
			end++
		}
		body := map[string]any{}
		for k, v := range mapping {
			body[k] = v
		}
		for j := idx + 1; j < end; j++ {
			km := tomlKeyRE.FindStringSubmatch(lines[j])
			if km == nil {
				continue
			}
			if v, ok := body[km[1]]; ok {
				lines[j] = km[1] + " = " + tomlValue(v)
				delete(body, km[1])
			}
		}
		if len(body) > 0 {
			insert := end
			for insert > idx+1 && strings.TrimSpace(lines[insert-1]) == "" {
				insert--
			}
			extra := make([]string, 0, len(body))
			// 固定顺序，避免 map 遍历导致 plan 每次都不一样。
			for _, k := range sortedKeys(body) {
				extra = append(extra, k+" = "+tomlValue(body[k]))
			}
			lines = insertLines(lines, insert, extra)
		}
		return strings.Join(lines, "\n")
	}
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	lines = append(lines, "", "["+table+"]")
	for _, k := range sortedKeys(mapping) {
		lines = append(lines, k+" = "+tomlValue(mapping[k]))
	}
	return strings.Join(lines, "\n") + "\n"
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	// 小表手写插入排序，避免为稳定输出引入额外依赖。
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}

// ManagedBlock 替换（或追加）由 begin/end 注释包裹的托管块。
func ManagedBlock(text, name, body string) (string, error) {
	begin := "# qoder-proxy:" + name + ":begin"
	end := "# qoder-proxy:" + name + ":end"
	if strings.Count(text, begin) != strings.Count(text, end) || strings.Count(text, begin) > 1 {
		return "", fmt.Errorf("invalid existing Qoder managed block")
	}
	block := begin + "\n" + strings.TrimRight(body, "\n") + "\n" + end + "\n"
	if strings.Contains(text, begin) {
		re := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(begin) + `\n[\s\S]*?^` + regexp.QuoteMeta(end) + `\n?`)
		return re.ReplaceAllString(text, block), nil
	}
	return strings.TrimRight(text, "\n") + "\n" + block, nil
}
