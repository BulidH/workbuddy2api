package qoder

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

// 网关本地 web 工具：当客户端声明内置 web_search / web_fetch 时，把它们改写成
// 同名 function 声明，由网关执行（DDG → Bing → Wikipedia），再把结果回灌模型。
// 这是唯一由模型驱动出网的路径，因此 SSRF 防护是硬要求：
//   - 仅 http/https，拒绝带 userinfo 的 URL；
//   - 解析后的每个地址必须是全球可路由单播（拒绝私网/环回/链路本地/CGNAT）；
//   - 重定向逐跳复检；最多 5 跳；
//   - 不走环境代理，响应体/正文字节上限固定。
const (
	WebSearchName = "web_search"
	WebFetchName  = "web_fetch"

	webMaxResults = 5
	webMaxFetch   = 20000
	webMaxBody    = 4 << 20
	webTimeout    = 15 * time.Second
	webCacheTTL   = 10 * time.Minute
	webMinGap     = 500 * time.Millisecond

	// WebToolMaxRounds 本地 web 工具最多回灌轮数。
	WebToolMaxRounds = 3
)

var webSearchDeclTypes = map[string]bool{
	"web_search": true, "web_search_preview": true, "web_search_preview_2025_03_11": true,
}

// WebToolNames 返回客户端声明的内置 web 工具名集合。
func WebToolNames(tools []any) map[string]bool {
	names := map[string]bool{}
	for _, raw := range tools {
		t, _ := raw.(map[string]any)
		kind, _ := t["type"].(string)
		if webSearchDeclTypes[kind] {
			names[WebSearchName] = true
		}
		if kind == WebFetchName {
			names[WebFetchName] = true
		}
	}
	return names
}

// RewriteWebTools 把内置 web 工具声明替换成网关自己的同名 function 声明。
func RewriteWebTools(tools []any, names map[string]bool) []any {
	out := make([]any, 0, len(tools)+2)
	for _, raw := range tools {
		t, _ := raw.(map[string]any)
		kind, _ := t["type"].(string)
		if webSearchDeclTypes[kind] || kind == WebFetchName {
			continue
		}
		out = append(out, raw)
	}
	for _, decl := range webToolDeclarations() {
		fn, _ := decl["function"].(map[string]any)
		name, _ := fn["name"].(string)
		if names[name] {
			out = append(out, decl)
		}
	}
	return out
}

func webToolDeclarations() []map[string]any {
	return []map[string]any{
		map[string]any{"type": "function", "function": map[string]any{
			"name":        WebSearchName,
			"description": "Search the public web and return the top results (title, url, snippet).",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{"type": "string", "description": "Search query"},
					"limit": map[string]any{"type": "integer", "description": "Max results (1-5)"},
				},
				"required": []any{"query"},
			},
		}},
		map[string]any{"type": "function", "function": map[string]any{
			"name":        WebFetchName,
			"description": "Fetch a public web page and return readable text (HTML stripped).",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"url": map[string]any{"type": "string", "description": "http(s) URL"},
				},
				"required": []any{"url"},
			},
		}},
	}
}

// RunWebTool 执行一次本地 web 工具调用，返回 JSON 字符串（永不返回 error，
// 错误也会序列化为 {"error":...} 回灌给模型）。
func RunWebTool(ctx context.Context, name, args string) string {
	var in map[string]any
	if strings.TrimSpace(args) != "" {
		_ = json.Unmarshal([]byte(args), &in)
	}
	switch name {
	case WebSearchName:
		q, _ := in["query"].(string)
		limit := intFromAny(in["limit"])
		if limit <= 0 || limit > webMaxResults {
			limit = webMaxResults
		}
		res, err := WebSearch(ctx, q, limit)
		if err != nil {
			return errJSON(err)
		}
		raw, _ := json.Marshal(map[string]any{"query": q, "results": res})
		return string(raw)
	case WebFetchName:
		u, _ := in["url"].(string)
		text, err := WebFetch(ctx, u)
		if err != nil {
			return errJSON(err)
		}
		raw, _ := json.Marshal(map[string]any{"url": u, "content": text})
		return string(raw)
	}
	return errJSON(fmt.Errorf("unknown web tool %q", name))
}

func errJSON(err error) string {
	raw, _ := json.Marshal(map[string]any{"error": err.Error()})
	return string(raw)
}

// WebResult 一条搜索命中。
type WebResult struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet"`
}

var webHTTP = &http.Client{
	Timeout: webTimeout,
	Transport: &http.Transport{
		Proxy:               nil, // 显式禁用环境代理，避免绕过 SSRF 校验
		DialContext:         safeDialContext,
		MaxIdleConns:        8,
		IdleConnTimeout:     60 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
	},
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many redirects")
		}
		return nil
	},
}

func safeDialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("no address for %s", host)
	}
	var lastErr error
	for _, ip := range ips {
		if !IsPublicIP(ip.IP) {
			lastErr = fmt.Errorf("blocked non-public address %s for %s", ip.IP, host)
			continue
		}
		d := net.Dialer{Timeout: 10 * time.Second}
		conn, err := d.DialContext(ctx, network, net.JoinHostPort(ip.IP.String(), port))
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no public address for %s", host)
	}
	return nil, lastErr
}

// IsPublicIP 判断是否全球可路由单播地址。
func IsPublicIP(ip net.IP) bool {
	if ip == nil || ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() ||
		ip.IsInterfaceLocalMulticast() {
		return false
	}
	if v4 := ip.To4(); v4 != nil {
		// CGNAT 100.64.0.0/10 与 0.0.0.0/8 等保留段。
		if v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127 {
			return false
		}
		if v4[0] == 0 || v4[0] == 127 || v4[0] == 169 && v4[1] == 254 {
			return false
		}
		return true
	}
	// IPv6：拒绝 ULA fc00::/7。
	if len(ip) >= 16 && ip[0]&0xfe == 0xfc {
		return false
	}
	return true
}

// ValidateURL 校验待抓取 URL（协议/凭据/主机名）。
func ValidateURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("only http/https URLs are allowed")
	}
	if u.User != nil {
		return nil, fmt.Errorf("URLs with credentials are not allowed")
	}
	if u.Host == "" {
		return nil, fmt.Errorf("URL host is required")
	}
	return u, nil
}

func webDo(ctx context.Context, req *http.Request, limit int64) ([]byte, error) {
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent",
			"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0 Safari/537.36")
	}
	resp, err := webHTTP.Do(req.WithContext(ctx))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("upstream status %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, limit+1))
}

// ---- 搜索：带 TTL 缓存 + single-flight + 引擎轮换 ----

type webCacheEntry struct {
	at  time.Time
	val []any
}

var webCache struct {
	sync.Mutex
	items map[string]webCacheEntry
}

func cacheGet(key string) ([]any, bool) {
	webCache.Lock()
	defer webCache.Unlock()
	e, ok := webCache.items[key]
	if !ok || time.Since(e.at) > webCacheTTL {
		return nil, false
	}
	return e.val, true
}

func cachePut(key string, val []any) {
	webCache.Lock()
	defer webCache.Unlock()
	if webCache.items == nil {
		webCache.items = map[string]webCacheEntry{}
	}
	if len(webCache.items) > 256 {
		webCache.items = map[string]webCacheEntry{}
	}
	webCache.items[key] = webCacheEntry{at: time.Now(), val: val}
}

var webThrottle struct {
	sync.Mutex
	last time.Time
}

func webPace(ctx context.Context) error {
	webThrottle.Lock()
	wait := webMinGap - time.Since(webThrottle.last)
	if wait > 0 {
		webThrottle.last = time.Now().Add(wait)
	} else {
		webThrottle.last = time.Now()
		wait = 0
	}
	webThrottle.Unlock()
	if wait <= 0 {
		return nil
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// WebSearch 依次尝试 DuckDuckGo、Bing、Wikipedia；全部失败返回最后一个错误。
func WebSearch(ctx context.Context, query string, limit int) ([]any, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, fmt.Errorf("query is required")
	}
	if limit <= 0 || limit > webMaxResults {
		limit = webMaxResults
	}
	key := fmt.Sprintf("search:%d:%s", limit, strings.ToLower(query))
	if hit, ok := cacheGet(key); ok {
		return hit, nil
	}
	if err := webPace(ctx); err != nil {
		return nil, err
	}
	engines := []func(context.Context, string, int) ([]WebResult, error){
		searchDuckDuckGo, searchBing, searchWikipedia,
	}
	var lastErr error
	for _, fn := range engines {
		res, err := fn(ctx, query, limit)
		if err == nil && len(res) > 0 {
			out := make([]any, 0, len(res))
			for _, r := range res {
				out = append(out, map[string]any{"title": r.Title, "url": r.URL, "snippet": r.Snippet})
			}
			cachePut(key, out)
			return out, nil
		}
		if err != nil {
			lastErr = err
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no search results")
	}
	return nil, lastErr
}

var (
	ddgResultRE = regexp.MustCompile(`(?s)<a[^>]+class="result__a"[^>]+href="([^"]+)"[^>]*>(.*?)</a>`)
	ddgSnipRE   = regexp.MustCompile(`(?s)<a[^>]+class="result__snippet"[^>]*>(.*?)</a>`)
	bingItemRE  = regexp.MustCompile(`(?s)<li class="b_algo".*?<h2><a href="([^"]+)"[^>]*>(.*?)</a></h2>(.*?)</li>`)
	tagRE       = regexp.MustCompile(`(?s)<[^>]+>`)
)

func searchDuckDuckGo(ctx context.Context, query string, limit int) ([]WebResult, error) {
	form := url.Values{"q": {query}}
	req, err := http.NewRequest(http.MethodPost,
		"https://html.duckduckgo.com/html/", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	raw, err := webDo(ctx, req, webMaxBody)
	if err != nil {
		return nil, err
	}
	text := string(raw)
	links := ddgResultRE.FindAllStringSubmatch(text, -1)
	snips := ddgSnipRE.FindAllStringSubmatch(text, -1)
	out := make([]WebResult, 0, limit)
	for i, m := range links {
		if len(out) >= limit {
			break
		}
		u := decodeDDGLink(m[1])
		title := cleanHTML(m[2])
		snip := ""
		if i < len(snips) {
			snip = cleanHTML(snips[i][1])
		}
		if u != "" {
			out = append(out, WebResult{Title: title, URL: u, Snippet: snip})
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("duckduckgo: no results")
	}
	return out, nil
}

func decodeDDGLink(raw string) string {
	raw = html.UnescapeString(raw)
	if strings.HasPrefix(raw, "//duckduckgo.com/l/?uddg=") {
		raw = "https://" + strings.TrimPrefix(raw, "//")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	if target := u.Query().Get("uddg"); target != "" {
		return target
	}
	return raw
}

func searchBing(ctx context.Context, query string, limit int) ([]WebResult, error) {
	u := "https://www.bing.com/search?q=" + url.QueryEscape(query)
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	raw, err := webDo(ctx, req, webMaxBody)
	if err != nil {
		return nil, err
	}
	matches := bingItemRE.FindAllStringSubmatch(string(raw), -1)
	out := make([]WebResult, 0, limit)
	for _, m := range matches {
		if len(out) >= limit {
			break
		}
		out = append(out, WebResult{
			Title:   cleanHTML(m[2]),
			URL:     html.UnescapeString(m[1]),
			Snippet: cleanHTML(m[3]),
		})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("bing: no results")
	}
	return out, nil
}

func searchWikipedia(ctx context.Context, query string, limit int) ([]WebResult, error) {
	for _, host := range []string{"zh.wikipedia.org", "en.wikipedia.org"} {
		u := "https://" + host + "/w/api.php?action=query&list=search&format=json&srlimit=" +
			fmt.Sprint(limit) + "&srsearch=" + url.QueryEscape(query)
		req, err := http.NewRequest(http.MethodGet, u, nil)
		if err != nil {
			continue
		}
		raw, err := webDo(ctx, req, webMaxBody)
		if err != nil {
			continue
		}
		var payload struct {
			Query struct {
				Search []struct {
					Title   string `json:"title"`
					Snippet string `json:"snippet"`
				} `json:"search"`
			} `json:"query"`
		}
		if json.Unmarshal(raw, &payload) != nil {
			continue
		}
		out := make([]WebResult, 0, len(payload.Query.Search))
		for _, r := range payload.Query.Search {
			out = append(out, WebResult{
				Title:   r.Title,
				URL:     "https://" + host + "/wiki/" + url.PathEscape(strings.ReplaceAll(r.Title, " ", "_")),
				Snippet: cleanHTML(r.Snippet),
			})
		}
		if len(out) > 0 {
			return out, nil
		}
	}
	return nil, fmt.Errorf("wikipedia: no results")
}

var (
	scriptREs = []*regexp.Regexp{
		regexp.MustCompile(`(?is)<script[^>]*>.*?</script>`),
		regexp.MustCompile(`(?is)<style[^>]*>.*?</style>`),
		regexp.MustCompile(`(?is)<noscript[^>]*>.*?</noscript>`),
	}
	blockRE = regexp.MustCompile(`(?i)</(p|div|li|h[1-6]|tr|br)\s*>`)
)

func cleanHTML(s string) string {
	// 搜索摘要里也可能带 <script>/<style>，这里统一剥离。
	for _, re := range scriptREs {
		s = re.ReplaceAllString(s, " ")
	}
	s = blockRE.ReplaceAllString(s, "\n")
	s = tagRE.ReplaceAllString(s, " ")
	s = html.UnescapeString(s)
	fields := strings.Fields(s)
	return strings.Join(fields, " ")
}

// WebFetch 抓取公开页面并抽取可读文本（最多 20000 字符）。
func WebFetch(ctx context.Context, rawURL string) (string, error) {
	u, err := ValidateURL(rawURL)
	if err != nil {
		return "", err
	}
	key := "fetch:" + u.String()
	if webCache.Lock(); webCache.items != nil {
		if e, ok := webCache.items[key]; ok && time.Since(e.at) <= webCacheTTL {
			webCache.Unlock()
			if len(e.val) > 0 {
				s, _ := e.val[0].(string)
				return s, nil
			}
		}
	}
	webCache.Unlock()
	if err := webPace(ctx); err != nil {
		return "", err
	}
	req, err := http.NewRequest(http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	raw, err := webDo(ctx, req, webMaxBody)
	if err != nil {
		return "", err
	}
	text := string(raw)
	if strings.Contains(strings.ToLower(text), "<html") {
		for _, re := range scriptREs {
			text = re.ReplaceAllString(text, " ")
		}
		text = cleanHTML(text)
	}
	if len(text) > webMaxFetch {
		text = text[:webMaxFetch]
	}
	if strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("empty response body")
	}
	cachePut(key, []any{text})
	return text, nil
}
