package main

// netfix 解决 Termux/Android 与复杂网络环境的常见坑:
//   1. Termux 没有 /etc/resolv.conf, Go 纯解析器默认走 [::1]:53 会 connection refused
//      → 内置多公共 DNS + 自动解析 /etc/resolv.conf + 真实 DNS 探测排序 + UDP/TCP 自动切换。
//   2. 原版 bug 修复: Go 的 net.Dial("udp", dns) 只在本地建 socket 不发包, 永远返回 nil,
//      导致原版 dnsServers[1..] 和 TCP 53 回退完全是死代码!
//      新版通过探测排序 + 连续重试时自动轮询下一 DNS 及切换 TCP 53 真正实现多 DNS 容灾。
//   3. Termux 的 CA 在 $PREFIX/etc/tls/cert.pem, 不在 Go 默认路径 → 自动附加到根证书池。
//   4. 代理兼容: 默认清空环境代理防止干扰 P2P, 但保存原始环境代理; 当直连信令服务器失败
//      或显式开启 -proxy / -keep-proxy 时, 自动支持通过 HTTP/SOCKS5 代理连接信令服务器。
//   5. 网络切换监听: 监控本机网卡 IP 变化 (如 Wi-Fi <-> 4G/5G 切换、VPN 开关), 立即触发重连。

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// dnsServers 公共 DNS 列表, 依次尝试; 可用 P2PTUN_DNS 覆盖 (csv)。
var dnsServers = []string{
	"223.5.5.5:53",
	"119.29.29.29:53",
	"114.114.114.114:53",
	"8.8.8.8:53",
	"1.1.1.1:53",
}

var (
	ambientProxy   string // 启动前环境中存在的代理 URL (用于直连信令失败时可选回退)
	configuredProxy string
	keepAmbientProxy bool

	dnsMu          sync.RWMutex
	activeDNSList  []string
	preferTCPDNS   atomic.Bool
	dnsDialCounter atomic.Uint64
	lastDNSDialNs  atomic.Int64
)

func init() {
	// 捕获启动前的环境代理, 以便用户指定 -keep-proxy 或直连信令失败时回退
	for _, k := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "ALL_PROXY", "all_proxy"} {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			ambientProxy = v
			break
		}
	}

	if v := os.Getenv("P2PTUN_DNS"); v != "" {
		if custom := parseDNSList(v); len(custom) > 0 {
			dnsServers = custom
		}
	} else {
		// 若系统 /etc/resolv.conf 存在有效非回环 DNS, 追加进候选列表
		sysDNS := readResolvConf("/etc/resolv.conf")
		if len(sysDNS) > 0 {
			seen := make(map[string]bool)
			var merged []string
			for _, s := range append(sysDNS, dnsServers...) {
				if !seen[s] {
					seen[s] = true
					merged = append(merged, s)
				}
			}
			dnsServers = merged
		}
	}
	setActiveDNS(dnsServers)
}

func parseDNSList(csv string) []string {
	var l []string
	seen := make(map[string]bool)
	for _, s := range strings.Split(csv, ",") {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if _, _, err := net.SplitHostPort(s); err != nil {
			// 自动补齐 :53 端口
			if strings.Contains(s, ":") && !strings.HasPrefix(s, "[") {
				s = "[" + s + "]:53"
			} else {
				s = s + ":53"
			}
		}
		if !seen[s] {
			seen[s] = true
			l = append(l, s)
		}
	}
	return l
}

func readResolvConf(path string) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "nameserver" {
			ip := fields[1]
			// 排除本地回环地址 (如 Termux/systemd-resolved 失效时的 127.0.0.1 / ::1)
			if ip == "127.0.0.1" || ip == "::1" || ip == "0.0.0.0" || strings.HasPrefix(ip, "127.") {
				continue
			}
			addr := net.JoinHostPort(ip, "53")
			out = append(out, addr)
		}
	}
	return out
}

func setActiveDNS(list []string) {
	dnsMu.Lock()
	cp := make([]string, len(list))
	copy(cp, list)
	activeDNSList = cp
	dnsMu.Unlock()
}

func getActiveDNS() []string {
	dnsMu.RLock()
	defer dnsMu.RUnlock()
	if len(activeDNSList) == 0 {
		return dnsServers
	}
	cp := make([]string, len(activeDNSList))
	copy(cp, activeDNSList)
	return cp
}

// buildDNSQuery 构造最小标准 DNS A 记录查询报文 (查询 0.peerjs.com 或 example.com)
func buildDNSQuery() []byte {
	// Header (12 bytes): ID=0x1234, Flags=0x0100 (standard query, recursion desired), QDCOUNT=1
	pkt := []byte{
		0x12, 0x34, 0x01, 0x00,
		0x00, 0x01, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00,
		// QNAME: \x07example\x03com\x00
		0x07, 'e', 'x', 'a', 'm', 'p', 'l', 'e',
		0x03, 'c', 'o', 'm', 0x00,
		// QTYPE=A (1), QCLASS=IN (1)
		0x00, 0x01, 0x00, 0x01,
	}
	return pkt
}

func probeSingleDNSUDP(server string, timeout time.Duration) (time.Duration, bool) {
	start := time.Now()
	c, err := net.DialTimeout("udp", server, timeout)
	if err != nil {
		return 0, false
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(timeout))
	q := buildDNSQuery()
	if _, err := c.Write(q); err != nil {
		return 0, false
	}
	buf := make([]byte, 512)
	n, err := c.Read(buf)
	if err != nil || n < 12 || buf[0] != 0x12 || buf[1] != 0x34 {
		return 0, false
	}
	return time.Since(start), true
}

func probeSingleDNSTCP(server string, timeout time.Duration) (time.Duration, bool) {
	start := time.Now()
	c, err := net.DialTimeout("tcp", server, timeout)
	if err != nil {
		return 0, false
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(timeout))
	q := buildDNSQuery()
	frame := make([]byte, 2+len(q))
	binary.BigEndian.PutUint16(frame[:2], uint16(len(q)))
	copy(frame[2:], q)
	if _, err := c.Write(frame); err != nil {
		return 0, false
	}
	resp := make([]byte, 14)
	n, err := c.Read(resp)
	if err != nil || n < 14 || resp[2] != 0x12 || resp[3] != 0x34 {
		return 0, false
	}
	return time.Since(start), true
}

// probeDNSServers 并发探测候选 DNS 服务器的真实可用性与延迟 (解决 UDP 假连通问题)
func probeDNSServers(timeout time.Duration) {
	candidates := getActiveDNS()
	if len(candidates) <= 1 {
		return
	}
	type res struct {
		server string
		rtt    time.Duration
		udpOK  bool
		tcpOK  bool
	}
	results := make([]res, len(candidates))
	var wg sync.WaitGroup
	for i, srv := range candidates {
		wg.Add(1)
		go func(idx int, s string) {
			defer wg.Done()
			if rtt, ok := probeSingleDNSUDP(s, timeout); ok {
				results[idx] = res{server: s, rtt: rtt, udpOK: true}
				return
			}
			if rtt, ok := probeSingleDNSTCP(s, timeout); ok {
				results[idx] = res{server: s, rtt: rtt, tcpOK: true}
				return
			}
			results[idx] = res{server: s, rtt: timeout * 10}
		}(i, srv)
	}
	wg.Wait()

	anyUDP := false
	anyTCP := false
	for _, r := range results {
		if r.udpOK {
			anyUDP = true
		}
		if r.tcpOK {
			anyTCP = true
		}
	}
	if !anyUDP && anyTCP {
		preferTCPDNS.Store(true)
	} else if anyUDP {
		preferTCPDNS.Store(false)
	}

	sort.SliceStable(results, func(i, j int) bool {
		ri, rj := results[i], results[j]
		okI := ri.udpOK || ri.tcpOK
		okJ := rj.udpOK || rj.tcpOK
		if okI != okJ {
			return okI
		}
		return ri.rtt < rj.rtt
	})

	sorted := make([]string, len(results))
	for i, r := range results {
		sorted[i] = r.server
	}
	setActiveDNS(sorted)
}

var (
	resolverOnce sync.Once
	resolver     *net.Resolver

	poolOnce sync.Once
	rootPool *x509.CertPool
)

func netResolver() *net.Resolver {
	resolverOnce.Do(func() {
		resolver = &net.Resolver{
			PreferGo: true,
			Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
				d := net.Dialer{Timeout: 4 * time.Second}
				servers := getActiveDNS()
				if len(servers) == 0 {
					servers = []string{"223.5.5.5:53", "8.8.8.8:53"}
				}

				// 检测是否处于短时间内的连续重试 (说明上一次 DNS 查询超时/被丢弃)
				now := time.Now().UnixNano()
				prev := lastDNSDialNs.Swap(now)
				rapidRetry := prev > 0 && (now-prev) < int64(6*time.Second)

				idx := int(dnsDialCounter.Add(1) - 1)
				if !rapidRetry {
					// 正常首次查询优先用探测排在第一位的最快 DNS
					idx = 0
				}

				// 构建网络协议尝试顺序:
				// 若探测发现当前网络封锁 UDP 53, 或处于连续重试的后半轮, 优先走 TCP 53
				var networks []string
				forceTCP := preferTCPDNS.Load() || (rapidRetry && idx >= len(servers) && idx%2 == 1)
				if strings.HasPrefix(network, "udp") {
					tcpNet := strings.Replace(network, "udp", "tcp", 1)
					if forceTCP {
						networks = []string{tcpNet, network}
					} else {
						networks = []string{network, tcpNet}
					}
				} else {
					networks = []string{network}
				}

				var lastErr error
				for _, nw := range networks {
					for i := 0; i < len(servers); i++ {
						dns := servers[(idx+i)%len(servers)]
						c, err := d.DialContext(ctx, nw, dns)
						if err == nil {
							return c, nil
						}
						lastErr = err
					}
				}
				return nil, lastErr
			},
		}
	})
	return resolver
}

func rootCAs() *x509.CertPool {
	poolOnce.Do(func() {
		pool, _ := x509.SystemCertPool()
		if pool == nil {
			pool = x509.NewCertPool()
		}
		paths := []string{
			"/data/data/com.termux/files/usr/etc/tls/cert.pem",
			os.Getenv("PREFIX") + "/etc/tls/cert.pem",
			"/etc/ssl/certs/ca-certificates.crt",
			"/system/etc/security/cacerts",
		}
		for _, p := range paths {
			if p == "" || p == "/etc/tls/cert.pem" {
				continue
			}
			if pem, err := os.ReadFile(p); err == nil {
				pool.AppendCertsFromPEM(pem)
			}
		}
		rootPool = pool
	})
	return rootPool
}

// clearProxyEnv 默认清理环境代理避免干扰 WebRTC ICE, 但支持保留或显式指定代理
func clearProxyEnv() {
	for _, k := range []string{
		"http_proxy", "https_proxy", "all_proxy", "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY",
		"socks_proxy", "SOCKS_PROXY", "no_proxy", "NO_PROXY",
	} {
		os.Unsetenv(k)
	}
}

// applyNetFix 全局固定解析器, 覆盖 net.Dialer / pion(ICE/STUN 域名解析), 并异步探测最优 DNS。
func applyNetFix() {
	clearProxyEnv()
	net.DefaultResolver = netResolver()
	go probeDNSServers(1500 * time.Millisecond)
}

// wsDialerWithProxy 返回带固定 DNS + Termux CA + 可选代理的 gorilla websocket dialer。
func wsDialerWithProxy(proxyStr string) *websocket.Dialer {
	netDialer := &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 15 * time.Second,
		Resolver:  netResolver(),
	}
	d := &websocket.Dialer{
		HandshakeTimeout: 15 * time.Second,
		TLSClientConfig:  &tls.Config{RootCAs: rootCAs()},
		NetDialContext:   netDialer.DialContext,
	}
	if proxyStr != "" {
		if pu, err := url.Parse(proxyStr); err == nil && pu.Scheme != "" {
			d.Proxy = http.ProxyURL(pu)
		}
	}
	return d
}

// wsDialer 返回默认 websocket dialer (若配置了显式代理或保留代理则启用)
func wsDialer() *websocket.Dialer {
	if configuredProxy != "" {
		return wsDialerWithProxy(configuredProxy)
	}
	if keepAmbientProxy && ambientProxy != "" {
		return wsDialerWithProxy(ambientProxy)
	}
	return wsDialerWithProxy("")
}

// tuneTCPConn 为隧道承载的 TCP 连接开启 NoDelay (降低 SSH 交互延迟) 与 KeepAlive
func tuneTCPConn(c net.Conn) {
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.SetNoDelay(true)
		_ = tc.SetKeepAlive(true)
		_ = tc.SetKeepAlivePeriod(30 * time.Second)
	}
}

// localNetworkFingerprint 获取当前本机非回环网卡 IP 指纹, 用于检测 Wi-Fi / 移动网络切换
func localNetworkFingerprint() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	var ips []string
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		// 忽略常见虚拟网卡避免误触发
		name := strings.ToLower(iface.Name)
		if strings.HasPrefix(name, "docker") || strings.HasPrefix(name, "veth") || strings.HasPrefix(name, "br-") {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			var ip net.IP
			switch v := a.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
				continue
			}
			ips = append(ips, iface.Name+"="+ip.String())
		}
	}
	sort.Strings(ips)
	return strings.Join(ips, ";")
}

// watchNetworkChanges 定期检查本机网络接口指纹, 当发生 Wi-Fi/5G 切换或 IP 变更时回调
func watchNetworkChanges(stop <-chan struct{}, interval time.Duration, onChange func(oldFP, newFP string)) {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	lastFP := localNetworkFingerprint()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			curFP := localNetworkFingerprint()
			if curFP != "" && lastFP != "" && curFP != lastFP {
				old := lastFP
				lastFP = curFP
				go probeDNSServers(1500 * time.Millisecond)
				if onChange != nil {
					onChange(old, curFP)
				}
			} else if curFP != "" && lastFP == "" {
				lastFP = curFP
			}
		}
	}
}