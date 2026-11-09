// Package egress 出站访问策略：服务端主动发起 HTTP 请求时的统一闸门。
//
// 背景（P0）：MCP endpoint、Swagger spec_url、技能包下载地址都来自管理面配置。
// 如果只校验 `http(s)://` 前缀，这些配置就成了内网跳板——攻击者填一个
// http://127.0.0.1:8500 或 http://169.254.169.254，就能让后端替他探测本机
// 管理口、K8s Service 与云厂商元数据。这与 stdio 的命令执行是同一类问题，
// 入口从「起进程」变成了「发请求」。
//
// 三道校验（每一道都能被绕过另一道的手法击穿，必须叠加）：
//
//  1. 写路径静态校验（ValidateURL）：scheme、userinfo、字面量 IP 段、主机白名单、端口。
//     挡住「直接填内网 IP」的配置，且不需要 DNS，配置保存不会因解析失败而报错。
//  2. Dial 阶段（transport.go）：先查主机白名单，再用 net.Dialer.Control 在
//     connect 之前校验**解析出来的 IP**——挡住「域名指向内网」与 DNS rebinding。
//  3. 重定向（transport.go）：每一跳目标重新走 URL 校验，挡住 302 到元数据地址。
//
// 配置（环境变量，进程启动时读取一次，可用 SetPolicy 覆盖）：
//
//	EGRESS_ALLOWED_HOSTS=mcp.example.com,.corp.internal   主机白名单；配了就变白名单模式
//	EGRESS_DENIED_HOSTS=evil.example.com                 额外拉黑（始终生效）
//	EGRESS_ALLOW_PRIVATE=false                           是否允许打内网/回环（默认否；
//	                                                     链路本地段 169.254/16 始终拒绝）
//	EGRESS_ALLOW_PRIVATE_HOSTS=127.0.0.1,.svc.cluster.local
//	                                                     仅对这些主机放行内网/回环。本机或集群内
//	                                                     跑 MCP 服务时用这个，比全局打开
//	                                                     EGRESS_ALLOW_PRIVATE 收得紧得多
//	EGRESS_ALLOWED_PORTS=443,8443                        端口白名单（空 = 不限）
//	EGRESS_MAX_REDIRECTS=5                               最多跟几跳重定向（0 = 不跟随）
//
// 兼容旧变量名：MCP_EGRESS_* 会作为别名读取（历史部署已按此前缀配置）。
//
// 设计取舍：默认「放行公网、禁止内网」而不是默认全禁。主场景是接第三方 SaaS
// 的公开端点，默认全禁会让存量配置全部失效；而内网地址几乎不可能是合法配置，
// 禁掉它收益最大、误伤最小。要收紧到特定域名，配 EGRESS_ALLOWED_HOSTS。
package egress

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
)

// ErrBlocked 出站被策略拒绝（调用方可用 errors.Is 判断并映射成 4xx）
var ErrBlocked = errors.New("出站访问被拒绝")

// Policy 出站访问策略
type Policy struct {
	// AllowedHosts 主机白名单，支持精确域名（含子域）、".example.com"、"*.example.com"
	AllowedHosts []string
	// DeniedHosts 额外拉黑，优先级高于白名单
	DeniedHosts []string
	// AllowPrivate 是否允许访问回环/私网/链路本地地址
	AllowPrivate bool
	// AllowPrivateHosts 仅对这些主机放行回环/私网。
	// 匹配规则同 AllowedHosts（精确主机名、"*.suffix"、".suffix"），IP 字面量按字符串比。
	// 与 AllowPrivate 的区别是「按连接目标收窄」：本机/集群内跑 MCP 服务时，
	// 只需要放行那一个主机，不必把全部 RFC1918 网段一起打开。
	AllowPrivateHosts []string
	// AllowedPorts 端口白名单；空表示不限端口
	AllowedPorts []int
	// MaxRedirects 最多跟随的重定向跳数；0 表示不跟随
	MaxRedirects int
}

// builtinDeniedHosts 一律拒绝的主机名：
//   - localhost / *.localhost 恒等于回环（部分系统还支持 localhost.evil.com 之类的
//     解析投毒，这里一并按后缀拦）；
//   - 云厂商元数据域名，即便 AllowPrivate 打开也必须挡住——它们只会解析到
//     169.254.169.254 / fd00:ec2::254 之类的临时凭据端点。
var builtinDeniedHosts = []string{
	"localhost",
	"metadata.google.internal",
	"metadata.goog",
	"instance-data",
	"instance-data.ec2.internal",
}

// alwaysBlocked 无论 AllowPrivate 是否打开都拒绝的网段。
// 链路本地段（169.254.0.0/16 / fe80::/10）是云厂商元数据服务的地址，
// 放行它等于把「读一把临时凭据」的口子留着；组播/保留段没有任何合法用途。
var alwaysBlocked = parseCIDRs([]string{
	"169.254.0.0/16", // 链路本地（云元数据 169.254.169.254）
	"fe80::/10",      // IPv6 链路本地
	"224.0.0.0/4",    // IPv4 组播
	"ff00::/8",       // IPv6 组播
	"240.0.0.0/4",    // 保留
	"192.0.0.0/24",   // IETF 协议分配
	"2001:db8::/32",  // 文档用
})

// privateBlocked 私网 / 回环 / 保留段。默认拒绝，EGRESS_ALLOW_PRIVATE=true 时放行
// （内网部署里 MCP 服务跑在 127.0.0.1 或 10.x 是常态）。
var privateBlocked = parseCIDRs([]string{
	"0.0.0.0/8",      // 本网络
	"10.0.0.0/8",     // RFC1918
	"100.64.0.0/10",  // CGNAT（云厂商内网常见段）
	"127.0.0.0/8",    // 回环
	"172.16.0.0/12",  // RFC1918
	"192.168.0.0/16", // RFC1918
	"198.18.0.0/15",  // 基准测试
	"::1/128",        // IPv6 回环
	"fc00::/7",       // IPv6 ULA
	"64:ff9b::/96",   // NAT64（可把 IPv4 私网塞进 IPv6 字面量）
})

func parseCIDRs(cidrs []string) []*net.IPNet {
	out := make([]*net.IPNet, 0, len(cidrs))
	for _, c := range cidrs {
		if _, n, err := net.ParseCIDR(c); err == nil {
			out = append(out, n)
		}
	}
	return out
}

var (
	policyMu sync.RWMutex
	policy   = loadPolicy()
)

// loadPolicy 从环境变量加载（MCP_EGRESS_* 为兼容别名）
func loadPolicy() Policy {
	p := Policy{
		AllowedHosts:      normalizePatterns(envList("EGRESS_ALLOWED_HOSTS", "MCP_EGRESS_ALLOWED_HOSTS")),
		DeniedHosts:       normalizePatterns(envList("EGRESS_DENIED_HOSTS", "MCP_EGRESS_DENIED_HOSTS")),
		AllowPrivate:      envBool(false, "EGRESS_ALLOW_PRIVATE", "MCP_EGRESS_ALLOW_PRIVATE"),
		AllowPrivateHosts: normalizePatterns(envList("EGRESS_ALLOW_PRIVATE_HOSTS", "MCP_EGRESS_ALLOW_PRIVATE_HOSTS")),
		MaxRedirects:      envInt(5, "EGRESS_MAX_REDIRECTS", "MCP_EGRESS_MAX_REDIRECTS"),
	}
	for _, s := range envList("EGRESS_ALLOWED_PORTS", "MCP_EGRESS_ALLOWED_PORTS") {
		if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil && n > 0 && n <= 65535 {
			p.AllowedPorts = append(p.AllowedPorts, n)
		}
	}
	return p
}

// SetPolicy 覆盖策略（启动装配与单测使用）
func SetPolicy(p Policy) {
	p.AllowedHosts = normalizePatterns(p.AllowedHosts)
	p.DeniedHosts = normalizePatterns(p.DeniedHosts)
	policyMu.Lock()
	policy = p
	policyMu.Unlock()
}

// CurrentPolicy 读取当前策略快照
func CurrentPolicy() Policy {
	policyMu.RLock()
	defer policyMu.RUnlock()
	return policy
}

// ValidateURL 写路径静态校验：远端地址能否作为出站目标。
func ValidateURL(raw string) error {
	s := strings.TrimSpace(raw)
	if s == "" {
		return fmt.Errorf("%w: 地址不能为空", ErrBlocked)
	}
	u, err := url.Parse(s)
	if err != nil {
		return fmt.Errorf("%w: 地址无法解析: %v", ErrBlocked, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("%w: 只支持 http/https，当前是 %q", ErrBlocked, u.Scheme)
	}
	// userinfo 会被拼进 Host 头与日志，且 http://a@169.254.169.254 这类写法
	// 在人工审查时极易看错，直接拒绝
	if u.User != nil {
		return fmt.Errorf("%w: 地址不允许携带用户名密码（userinfo）", ErrBlocked)
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if host == "" {
		return fmt.Errorf("%w: 地址缺少主机名", ErrBlocked)
	}
	if err := checkPort(u.Port()); err != nil {
		return err
	}
	return CheckHost(host)
}

// CheckHost 主机校验（Dial 前的最后一道主机名关卡）。
// host 可以是域名，也可以是字面量 IP。
func CheckHost(host string) error {
	p := CurrentPolicy()
	h := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if h == "" {
		return fmt.Errorf("%w: 主机名为空", ErrBlocked)
	}
	for _, d := range builtinDeniedHosts {
		if h == d || strings.HasSuffix(h, "."+d) {
			return fmt.Errorf("%w: 禁止访问云厂商元数据域名 %q", ErrBlocked, h)
		}
	}
	if matchPatterns(h, p.DeniedHosts) {
		return fmt.Errorf("%w: 主机 %q 在黑名单内", ErrBlocked, h)
	}
	if ip := net.ParseIP(h); ip != nil {
		return CheckIPForHost(ip, h)
	}
	if len(p.AllowedHosts) > 0 && !matchPatterns(h, p.AllowedHosts) {
		return fmt.Errorf("%w: 主机 %q 不在出站白名单内；请配置 EGRESS_ALLOWED_HOSTS", ErrBlocked, h)
	}
	return nil
}

// CheckIP 解析后的 IP 校验：默认拒绝回环 / 私网 / 链路本地 / 保留段。
//
// 这是防 DNS rebinding 的关键：域名可以在保存配置时解析到公网、在真正 connect
// 时解析到 127.0.0.1。所以必须拿「即将连接的那个 IP」来判断。
//
// 不知道主机名时用这个；知道的话用 CheckIPForHost，才能应用 EGRESS_ALLOW_PRIVATE_HOSTS。
func CheckIP(ip net.IP) error {
	return CheckIPForHost(ip, "")
}

// CheckIPForHost 在 CheckIP 的基础上带上「本次连接的主机名」，
// 使 EGRESS_ALLOW_PRIVATE_HOSTS 能按目标收窄放行范围。
//
// host 传解析前的原始目标（域名或 IP 字面量），空字符串等价于 CheckIP。
func CheckIPForHost(ip net.IP, host string) error {
	if ip == nil {
		return fmt.Errorf("%w: IP 无法解析", ErrBlocked)
	}
	// IPv4-mapped IPv6（::ffff:127.0.0.1）必须还原成 IPv4 再判断，否则会绕过
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	if ip.IsUnspecified() || ip.IsMulticast() || ip.IsInterfaceLocalMulticast() {
		return fmt.Errorf("%w: 目标地址 %s 属于未指定/组播/接口本地，禁止出站", ErrBlocked, ip)
	}
	for _, n := range alwaysBlocked {
		if n.Contains(ip) {
			return fmt.Errorf("%w: 目标地址 %s 属于链路本地/保留网段（%s），禁止出站", ErrBlocked, ip, n.String())
		}
	}
	if allowPrivateFor(host) {
		return nil
	}
	for _, n := range privateBlocked {
		if n.Contains(ip) {
			return fmt.Errorf("%w: 目标地址 %s 属于内网/回环网段（%s），禁止出站；"+
				"确需访问内网请把这台主机加进 EGRESS_ALLOW_PRIVATE_HOSTS（如 127.0.0.1），"+
				"或整体设置 EGRESS_ALLOW_PRIVATE=true", ErrBlocked, ip, n.String())
		}
	}
	return nil
}

// allowPrivateFor 该主机是否被允许访问私网。
// 注意：白名单只影响「私网/回环」这一层，链路本地（169.254/16）等网段始终拒绝。
func allowPrivateFor(host string) bool {
	p := CurrentPolicy()
	if p.AllowPrivate {
		return true
	}
	h := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if h == "" || len(p.AllowPrivateHosts) == 0 {
		return false
	}
	return matchPatterns(h, p.AllowPrivateHosts)
}

// checkPort 端口白名单（未配置则不限）
func checkPort(port string) error {
	p := CurrentPolicy()
	if len(p.AllowedPorts) == 0 || port == "" {
		return nil
	}
	n, err := strconv.Atoi(port)
	if err != nil {
		return fmt.Errorf("%w: 端口 %q 不合法", ErrBlocked, port)
	}
	for _, allowed := range p.AllowedPorts {
		if n == allowed {
			return nil
		}
	}
	return fmt.Errorf("%w: 端口 %d 不在出站端口白名单内", ErrBlocked, n)
}

// matchPatterns 主机是否命中规则集。
// 规则形式：example.com（精确，含子域）｜.example.com / *.example.com（后缀）｜*
func matchPatterns(host string, patterns []string) bool {
	for _, p := range patterns {
		p = strings.TrimSpace(strings.ToLower(p))
		if p == "" {
			continue
		}
		if p == "*" {
			return true
		}
		if suffix, ok := strings.CutPrefix(p, "*."); ok {
			if host == suffix || strings.HasSuffix(host, "."+suffix) {
				return true
			}
			continue
		}
		if suffix, ok := strings.CutPrefix(p, "."); ok {
			if host == suffix || strings.HasSuffix(host, "."+suffix) {
				return true
			}
			continue
		}
		if host == p || strings.HasSuffix(host, "."+p) {
			return true
		}
	}
	return false
}

func normalizePatterns(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s = strings.ToLower(strings.TrimSpace(s)); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// —— 环境变量读取（多变量名按顺序取第一个非空值）——

func envList(keys ...string) []string {
	for _, k := range keys {
		raw := strings.TrimSpace(os.Getenv(k))
		if raw == "" {
			continue
		}
		parts := strings.Split(raw, ",")
		out := make([]string, 0, len(parts))
		for _, p := range parts {
			if s := strings.TrimSpace(p); s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func envBool(def bool, keys ...string) bool {
	for _, k := range keys {
		v := strings.ToLower(strings.TrimSpace(os.Getenv(k)))
		if v == "" {
			continue
		}
		return v == "1" || v == "true" || v == "yes" || v == "on"
	}
	return def
}

func envInt(def int, keys ...string) int {
	for _, k := range keys {
		v := strings.TrimSpace(os.Getenv(k))
		if v == "" {
			continue
		}
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return n
		}
	}
	return def
}
