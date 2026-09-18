package web

import (
	"compress/gzip"
	"compress/zlib"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Fetching one page, within stated bounds.
//
// Every limit here exists because the alternative is a tool that can be turned
// into a memory problem or a hang by a page that is merely large or merely slow:
// the body is counted while it is read (not after), the content type is checked
// before the body is parsed, and the redirect chain is bounded and re-checked.

// Options configures a Fetcher.
type Options struct {
	// Guard decides which addresses may be reached.
	Guard Guard
	// Timeout bounds one request end to end.
	Timeout time.Duration
	// MaxBytes bounds the response body, counted while reading.
	MaxBytes int64
	// RedirectLimit bounds the redirect chain.
	RedirectLimit int
	// UserAgent identifies this client. Servers that block unknown agents are
	// common enough that it is configurable.
	UserAgent string
	// CacheTTL and CacheMaxEntries bound the in-process content cache. Zero TTL
	// disables it.
	CacheTTL        time.Duration
	CacheMaxEntries int
	Logger          Logger
}

// Defaults for Options.
const (
	DefaultTimeout       = 20 * time.Second
	DefaultMaxBytes      = 2 << 20
	DefaultRedirectLimit = 5
	DefaultUserAgent     = "huan-agent/1.0 (+https://github.com/huan/huan-agent)"
	DefaultCacheTTL      = 5 * time.Minute
	DefaultCacheEntries  = 128
)

// Page is one fetched document.
type Page struct {
	// Title is the page's <title>, empty when it has none.
	Title string
	// URL is the final address after redirects.
	URL string
	// ContentType is the media type the server reported.
	ContentType string
	// Text is the extracted content, sliced to the requested window.
	Text string
	// Offset and TotalChars describe the window: TotalChars is the whole
	// extracted text, so a caller can page through it.
	Offset     int
	TotalChars int
	// Truncated reports that the response body was cut at MaxBytes.
	Truncated bool
	// FetchedAt is when the content was retrieved (not when it was cached).
	FetchedAt time.Time
	// Cached reports that this answer came from the in-process cache.
	Cached bool
	// Note carries anything the caller should know: a page that extracted to
	// almost nothing, a body that was cut.
	Note string
	// Links are the page's same-site links, resolved, up to a bound. They are what
	// makes a fetched page usable for navigation ("read the next section") without
	// a map of the whole site.
	Links []string
}

// Fetcher retrieves pages.
type Fetcher struct {
	opts   Options
	client *http.Client

	mu    sync.Mutex
	cache map[string]cacheEntry
	order []string
}

type cacheEntry struct {
	page   Page
	stored time.Time
}

// NewFetcher builds a Fetcher.
func NewFetcher(opts Options) *Fetcher {
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultTimeout
	}
	if opts.MaxBytes <= 0 {
		opts.MaxBytes = DefaultMaxBytes
	}
	if opts.RedirectLimit <= 0 {
		opts.RedirectLimit = DefaultRedirectLimit
	}
	if strings.TrimSpace(opts.UserAgent) == "" {
		opts.UserAgent = DefaultUserAgent
	}

	f := &Fetcher{opts: opts, cache: map[string]cacheEntry{}}

	dialer := &net.Dialer{Timeout: 10 * time.Second}
	transport := &http.Transport{
		// The dial-time address check is what closes the DNS-rebind gap: the URL
		// check approved a set of addresses, and this refuses to connect anywhere
		// else if the name resolves differently a moment later.
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			ips, err := f.opts.Guard.CheckURL(ctx, "http://"+net.JoinHostPort(host, port))
			if err != nil {
				return nil, err
			}
			var lastErr error
			for _, ip := range ips {
				if err := f.opts.Guard.CheckIP(ip); err != nil {
					lastErr = err
					continue
				}
				conn, derr := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
				if derr == nil {
					return conn, nil
				}
				lastErr = derr
			}
			if lastErr == nil {
				lastErr = fmt.Errorf("web: %s 没有可用地址", addr)
			}
			return nil, lastErr
		},
		Proxy:               http.ProxyFromEnvironment,
		DisableCompression:  false,
		TLSHandshakeTimeout: 10 * time.Second,
	}
	f.client = &http.Client{
		Transport: transport,
		Timeout:   opts.Timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= f.opts.RedirectLimit {
				return fmt.Errorf("web: 重定向超过 %d 次", f.opts.RedirectLimit)
			}
			// Every hop is re-checked. A single check on the first URL is what
			// makes `302 → http://169.254.169.254/` work.
			if _, err := f.opts.Guard.CheckURL(req.Context(), req.URL.String()); err != nil {
				return err
			}
			return nil
		},
	}
	return f
}

// Fetch retrieves a URL and returns its content as text.
//
// offset and maxChars window the extracted text; the window is applied after
// extraction, so a caller can read a long page in pieces without re-fetching it
// (and the cache makes that literal).
func (f *Fetcher) Fetch(ctx context.Context, rawURL string, offset, maxChars int) (Page, error) {
	normalized := strings.TrimSpace(rawURL)
	if normalized == "" {
		return Page{}, fmt.Errorf("web: URL 不能为空")
	}

	if cached, ok := f.fromCache(normalized); ok {
		return window(cached, offset, maxChars), nil
	}

	if _, err := f.opts.Guard.CheckURL(ctx, normalized); err != nil {
		return Page{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, normalized, nil)
	if err != nil {
		return Page{}, fmt.Errorf("web: 构造请求失败：%w", err)
	}
	req.Header.Set("User-Agent", f.opts.UserAgent)
	// Ask for text; a server that has an HTML page generally serves it here too.
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,text/plain;q=0.8,application/json;q=0.7,*/*;q=0.1")

	resp, err := f.client.Do(req)
	if err != nil {
		return Page{}, fmt.Errorf("web: 抓取 %s 失败：%w", normalized, err)
	}
	defer func() { _ = resp.Body.Close() }()

	// resp.URL does not exist: the address actually served is the one on the
	// request that produced this response, after every redirect.
	finalURL := normalized
	if resp.Request != nil && resp.Request.URL != nil {
		finalURL = resp.Request.URL.String()
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return Page{}, fmt.Errorf("web: %s 返回 HTTP %d", finalURL, resp.StatusCode)
	}

	contentType := resp.Header.Get("Content-Type")
	if !allowedContentType(contentType) {
		return Page{}, fmt.Errorf("web: %s 的内容类型是 %q，本工具只读文本类内容（HTML/纯文本/JSON/XML/Markdown）",
			finalURL, firstNonEmpty(contentType, "未声明"))
	}

	body, truncated, err := f.readBody(resp)
	if err != nil {
		return Page{}, err
	}

	page := Page{
		URL:         finalURL,
		ContentType: contentType,
		Truncated:   truncated,
		FetchedAt:   time.Now(),
	}
	if truncated {
		page.Note = fmt.Sprintf("响应体超过 %d KiB，已截断", f.opts.MaxBytes>>10)
	}

	if isHTML(contentType, body) {
		title, text := extract(string(body))
		page.Title, page.Text = title, text
		page.Links = linksFrom(resp.Request.URL, string(body))
	} else {
		page.Text = string(body)
	}
	if strings.TrimSpace(page.Text) == "" {
		page.Note = strings.TrimSpace(page.Note + "；这一页没有可读的正文（正文可能是需要 JavaScript 渲染的）")
	} else if len([]rune(page.Text)) < 200 && isHTML(contentType, body) {
		page.Note = strings.TrimSpace(page.Note + "；正文很短，这一页可能主要由 JavaScript 渲染")
	}

	page.TotalChars = len([]rune(page.Text))
	f.store(normalized, page)
	return window(page, offset, maxChars), nil
}

// readBody reads up to MaxBytes, counting as it goes rather than after.
func (f *Fetcher) readBody(resp *http.Response) ([]byte, bool, error) {
	var reader io.Reader = resp.Body
	switch strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding"))) {
	case "gzip":
		gz, err := gzip.NewReader(resp.Body)
		if err != nil {
			return nil, false, fmt.Errorf("web: 解压 gzip 失败：%w", err)
		}
		defer func() { _ = gz.Close() }()
		reader = gz
	case "deflate":
		zr, err := zlib.NewReader(resp.Body)
		if err != nil {
			return nil, false, fmt.Errorf("web: 解压 deflate 失败：%w", err)
		}
		defer func() { _ = zr.Close() }()
		reader = zr
	}

	limited := io.LimitReader(reader, f.opts.MaxBytes+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, false, fmt.Errorf("web: 读取响应体失败：%w", err)
	}
	if int64(len(body)) > f.opts.MaxBytes {
		return body[:f.opts.MaxBytes], true, nil
	}
	return body, false, nil
}

// window slices the extracted text to the requested rune range.
func window(page Page, offset, maxChars int) Page {
	runes := []rune(page.Text)
	if offset < 0 {
		offset = 0
	}
	if offset > len(runes) {
		offset = len(runes)
	}
	rest := runes[offset:]
	if maxChars > 0 && len(rest) > maxChars {
		rest = rest[:maxChars]
		page.Note = strings.TrimSpace(page.Note + fmt.Sprintf("；正文还有 %d 个字符，用 offset 接着读",
			len(runes)-offset-maxChars))
	}
	page.Text = string(rest)
	page.Offset = offset
	page.TotalChars = len(runes)
	return page
}

// --- cache ---

func (f *Fetcher) fromCache(key string) (Page, bool) {
	if f.opts.CacheTTL <= 0 {
		return Page{}, false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	entry, ok := f.cache[key]
	if !ok {
		return Page{}, false
	}
	if time.Since(entry.stored) > f.opts.CacheTTL {
		delete(f.cache, key)
		return Page{}, false
	}
	page := entry.page
	page.Cached = true
	// FetchedAt stays the original retrieval time: it is when the content was
	// true, and a cache hit does not make it fresher.
	return page, true
}

func (f *Fetcher) store(key string, page Page) {
	if f.opts.CacheTTL <= 0 {
		return
	}
	max := f.opts.CacheMaxEntries
	if max <= 0 {
		max = DefaultCacheEntries
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, exists := f.cache[key]; !exists {
		f.order = append(f.order, key)
	}
	f.cache[key] = cacheEntry{page: page, stored: time.Now()}
	// Oldest first, so the bound is on the number of pages rather than on bytes —
	// a page's extracted text is already bounded by MaxBytes.
	for len(f.order) > max {
		oldest := f.order[0]
		f.order = f.order[1:]
		delete(f.cache, oldest)
	}
}

// --- content type ---

// allowedContentType is the whitelist. Anything else is refused with its type in
// the message rather than dumped into the context as bytes.
func allowedContentType(header string) bool {
	if strings.TrimSpace(header) == "" {
		// A server that declares nothing is treated as HTML: refusing outright
		// would break the many small sites that do not set the header.
		return true
	}
	media := strings.ToLower(strings.TrimSpace(strings.Split(header, ";")[0]))
	switch {
	case strings.HasPrefix(media, "text/"):
		return true
	case media == "application/json", media == "application/xml", media == "application/xhtml+xml",
		media == "application/x-yaml", media == "application/yaml":
		return true
	case strings.HasSuffix(media, "+json"), strings.HasSuffix(media, "+xml"):
		return true
	}
	return false
}

func isHTML(contentType string, body []byte) bool {
	media := strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0]))
	if media == "text/html" || media == "application/xhtml+xml" {
		return true
	}
	if media == "" {
		// Sniff: a page that declares nothing but starts with markup is HTML.
		head := strings.ToLower(strings.TrimSpace(string(body[:min(len(body), 256)])))
		return strings.HasPrefix(head, "<!doctype html") || strings.HasPrefix(head, "<html")
	}
	return false
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
