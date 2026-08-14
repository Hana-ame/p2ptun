package main

// netfix 解决 Termux/Android 的网络坑 (参考 wintools/pkg/netdial):
//   - Termux 没有 /etc/resolv.conf, Go 纯解析器默认走 [::1]:53 会
//     connection refused → 固定走公共 DNS。
//   - Termux 的 CA 在 $PREFIX/etc/tls/cert.pem,不在 Go 默认路径,
//     wss 握手会报 unknown authority → 附加到根证书池。

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// dnsServers 公共 DNS 列表, 依次尝试; 可用 P2PTUN_DNS 覆盖 (csv)。
var dnsServers = []string{
	"223.5.5.5:53",
	"114.114.114.114:53",
	"8.8.8.8:53",
	"1.1.1.1:53",
}

func init() {
	if v := os.Getenv("P2PTUN_DNS"); v != "" {
		var l []string
		for _, s := range strings.Split(v, ",") {
			s = strings.TrimSpace(s)
			if s != "" {
				l = append(l, s)
			}
		}
		if len(l) > 0 {
			dnsServers = l
		}
	}
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
				d := net.Dialer{Timeout: 5 * time.Second}
				var err error
				for _, dns := range dnsServers {
					var c net.Conn
					c, err = d.DialContext(ctx, network, dns)
					if err == nil {
						return c, nil
					}
				}
				return nil, err
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

// applyNetFix 全局固定解析器, 覆盖 net.Dialer / pion(ICE/STUN 域名解析)。
func applyNetFix() {
	net.DefaultResolver = netResolver()
}

// wsDialer 返回带固定 DNS + Termux CA 的 gorilla websocket dialer。
func wsDialer() *websocket.Dialer {
	return &websocket.Dialer{
		HandshakeTimeout: 15 * time.Second,
		TLSClientConfig:  &tls.Config{RootCAs: rootCAs()},
	}
}