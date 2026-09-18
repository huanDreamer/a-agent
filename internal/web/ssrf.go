// Package web fetches a URL and turns it into text an agent can read.
//
// It exists because the alternative is `curl`: a modern documentation page is
// hundreds of kilobytes of HTML whose text is mostly navigation, and what arrives
// in the model's context is a truncated shell of it. Fetching + extracting the
// main content is the whole value — without it this package is a wrapper around a
// command the agent already has.
//
// Two things here are security boundaries rather than features:
//
//   - **The address check.** The model can ask for any URL, and without a check
//     `http://169.254.169.254/` (cloud metadata), `http://127.0.0.1:1933/` (the
//     local OpenViking server) and `http://10.0.0.1/` are all reachable. It is
//     re-checked on every redirect and again at dial time, because a 302 and a DNS
//     rebind are the two classic ways past a check that only runs once.
//   - **The untrusted-content framing.** A fetched page is external content that
//     can contain instructions aimed at the model. The tool's result says so
//     explicitly, and the prompt tells the model to treat it as data.
package web

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strings"
)

// Guard decides which URLs may be fetched.
type Guard struct {
	// AllowPrivate permits loopback, private and link-local addresses. It is for a
	// deployment whose wiki lives on the LAN, and it is off by default because the
	// addresses it permits include the cloud metadata endpoint and the agent's own
	// local services.
	AllowPrivate bool
	// resolver is swappable so a test can script a DNS rebind.
	resolver func(ctx context.Context, host string) ([]net.IP, error)
	// Logger records refusals at debug level, so an operator can see why a fetch
	// was blocked.
	Logger Logger
}

// Logger is the slice of logging this package needs.
type Logger interface {
	Debug(msg string, keysAndValues ...any)
	Warn(msg string, keysAndValues ...any)
}

// CheckURL validates a URL's scheme and resolved addresses.
//
// It returns the addresses it approved, so the caller can pin the dial to them
// rather than resolving a second time — the gap between the two resolutions is
// where a rebind lives.
func (g Guard) CheckURL(ctx context.Context, raw string) ([]net.IP, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("web: 无法解析 URL %q：%w", raw, err)
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
	case "":
		return nil, fmt.Errorf("web: URL 缺少协议（要 http:// 或 https://）")
	default:
		return nil, fmt.Errorf("web: 不支持 %q 协议，只允许 http 与 https", u.Scheme)
	}

	host := u.Hostname()
	if host == "" {
		return nil, fmt.Errorf("web: URL 里没有主机名")
	}

	resolve := g.resolver
	if resolve == nil {
		resolve = defaultResolver
	}
	ips, err := resolve(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("web: 无法解析 %q：%w", host, err)
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("web: %q 没有解析到任何地址", host)
	}
	if g.AllowPrivate {
		return ips, nil
	}
	for _, ip := range ips {
		if reason := blockedReason(ip); reason != "" {
			return nil, fmt.Errorf("web: %s 解析到 %s（%s），出于安全考虑拒绝抓取；"+
				"确实需要访问内网就设置 tools.web.allow_private: true", host, ip, reason)
		}
	}
	return ips, nil
}

// CheckIP is the dial-time check: the address actually being connected to must be
// one the URL check would have approved.
//
// Without it a hostname that resolved to a public address during the check and to
// a private one a moment later (a DNS rebind) would be dialled anyway.
func (g Guard) CheckIP(ip net.IP) error {
	if g.AllowPrivate || ip == nil {
		return nil
	}
	if reason := blockedReason(ip); reason != "" {
		return fmt.Errorf("web: 连接到 %s 被拒绝（%s）", ip, reason)
	}
	return nil
}

// blockedReason names why an address is refused, or "" when it is acceptable.
//
// The list is the one that matters for server-side request forgery: anything that
// is not routable on the public internet. The cloud metadata endpoint is called
// out by name because it is the specific address that turns "the model read a web
// page" into "the model has the instance's credentials".
func blockedReason(ip net.IP) string {
	// The metadata address is named before the general link-local case, because it
	// is inside that range and the operator reading the refusal should see what
	// they were actually about to reach.
	if ip.String() == "169.254.169.254" {
		return "云元数据服务地址（会交出实例凭据）"
	}
	switch {
	case ip.IsUnspecified():
		return "未指定地址"
	case ip.IsLoopback():
		return "环回地址"
	case ip.IsInterfaceLocalMulticast(), ip.IsMulticast():
		return "多播地址"
	case ip.IsLinkLocalUnicast():
		return "链路本地地址"
	case ip.IsPrivate():
		// Go's IsPrivate covers RFC 1918 and IPv6 unique local addresses (fc00::/7).
		return "内网地址"
	}
	return ""
}

func defaultResolver(ctx context.Context, host string) ([]net.IP, error) {
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	out := make([]net.IP, 0, len(addrs))
	for _, a := range addrs {
		out = append(out, a.IP)
	}
	return out, nil
}
