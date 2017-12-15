package pfdns

import (
	"net"
	"time"

	"github.com/inverse-inc/packetfence/go/caddy/caddy"
	"github.com/inverse-inc/packetfence/go/coredns/core/dnsserver"
	"github.com/inverse-inc/packetfence/go/coredns/plugin"
	cache "github.com/patrickmn/go-cache"
)

func init() {
	caddy.RegisterPlugin("pfdns", caddy.Plugin{
		ServerType: "dns",
		Action:     setuppfdns,
	})
}

func setuppfdns(c *caddy.Controller) error {
	var pf = &pfdns{}
	var ip net.IP

	for c.Next() {
		// block with extra parameters
		for c.NextBlock() {
			switch c.Val() {

			case "redirectTo":
				arg := c.RemainingArgs()
				ip = net.ParseIP(arg[0])
				if ip == nil {
					return c.Errf("Invalid IP address '%s'", c.Val())
				}
			default:
				return c.Errf("Unknown keyword '%s'", c.Val())
			}
		}
	}

	if err := pf.DbInit(); err != nil {
		return c.Errf("pfdns: unable to initialize database connection")
	}
	if err := pf.PassthrouthsInit(); err != nil {
		return c.Errf("pfdns: unable to initialize passthrough")
	}
	if err := pf.PassthrouthsIsolationInit(); err != nil {
		return c.Errf("pfdns: unable to initialize isolation passthrough")
	}

	if err := pf.WebservicesInit(); err != nil {
		return c.Errf("pfdns: unable to fetch Webservices credentials")
	}

	if err := pf.detectVIP(); err != nil {
		return c.Errf("pfdns: unable to initialize the vip network map")
	}

	if err := pf.DomainPassthroughInit(); err != nil {
		return c.Errf("pfdns: unable to initialize domain passthrough")
	}

	if err := pf.detectType(); err != nil {
		return c.Errf("pfdns: unable to initialize Network Type")
	}

	// Initialize dns filter cache
	hwcache := cache.New(300*time.Second, 310*time.Second)

	pf.DNSFilter = hwcache

	dnsserver.GetConfig(c).AddPlugin(
		func(next plugin.Handler) plugin.Handler {
			pf.RedirectIP = ip
			pf.Next = next
			return pf
		})

	return nil
}
