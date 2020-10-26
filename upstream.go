/*
 * Created Feb 23, 2020
 */

package dnsredir

import (
	"crypto/tls"
	"errors"
	"fmt"
	"github.com/coredns/caddy"
	"github.com/coredns/coredns/core/dnsserver"
	"github.com/coredns/coredns/plugin"
	pkgtls "github.com/coredns/coredns/plugin/pkg/tls"
	"github.com/coredns/coredns/plugin/pkg/transport"
	"github.com/miekg/dns"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type reloadableUpstream struct {
	// Flag indicate match any request, i.e. the root zone "."
	matchAny bool
	*NameList
	inline domainSet
	ignored domainSet
	*HealthCheck
	// Bootstrap DNS in IP:Port combo
	bootstrap []string
	ipset interface{}
	Pf interface{}
	noIPv6 bool
}

// reloadableUpstream implements Upstream interface

// Check if given name in upstream name list
// `name' is lower cased and without trailing dot(except for root zone)
func (u *reloadableUpstream) Match(name string) bool {
	if u.matchAny {
		if !plugin.Name(".").Matches(name) {
			panic(fmt.Sprintf("Why %q doesn't match %q?!", name, "."))
		}

		ignored := u.ignored.Match(name)
		if ignored {
			log.Debugf("#0 Skip %q since it's ignored", name)
		}
		return !ignored
	}

	if !u.NameList.Match(name) && !u.inline.Match(name) {
		return false
	}

	if u.ignored.Match(name) {
		log.Debugf("#1 Skip %q since it's ignored", name)
		return false
	}
	return true
}

func (u *reloadableUpstream) Start() error {
	u.periodicUpdate(u.bootstrap)
	u.HealthCheck.Start()
	if err := ipsetSetup(u); err != nil {
		return err
	}
	return nil
}

func (u *reloadableUpstream) Stop() error {
	close(u.stopPathReload)
	close(u.stopUrlReload)
	u.HealthCheck.Stop()
	if err := ipsetShutdown(u); err != nil {
		return err
	}
	return nil
}

// Parses Caddy config input and return a list of reloadable upstream for this plugin
func NewReloadableUpstreams(c *caddy.Controller) ([]Upstream, error) {
	var ups []Upstream

	for c.Next() {
		u, err := newReloadableUpstream(c)
		if err != nil {
			return nil, err
		}
		ups = append(ups, u)
	}

	if ups == nil {
		panic("Why upstream hosts is nil? it shouldn't happen.")
	}
	return ups, nil
}

// see: healthcheck.go/UpstreamHost.Dial()
func protoToNetwork(proto string) string {
	if proto == "tls" {
		return "tcp-tls"
	}
	return proto
}

func newReloadableUpstream(c *caddy.Controller) (Upstream, error) {
	u := &reloadableUpstream{
		NameList: &NameList{
			pathReload:     defaultPathReloadInterval,
			stopPathReload: make(chan struct{}),
			urlReload:      defaultUrlReloadInterval,
			urlReadTimeout: defaultUrlReadTimeout,
			stopUrlReload:  make(chan struct{}),
		},
		ignored: make(domainSet),
		inline: make(domainSet),
		HealthCheck: &HealthCheck{
			stop:          make(chan struct{}),
			maxFails:      defaultMaxFails,
			checkInterval: defaultHcInterval,
			transport: &Transport{
				expire: defaultConnExpire,
				tlsConfig: new(tls.Config),
				recursionDesired: true,
			},
		},
	}

	if err := parseFrom(c, u); err != nil {
		return nil, err
	}

	for c.NextBlock() {
		if err := parseBlock(c, u); err != nil {
			return nil, err
		}
	}

	if u.hosts == nil {
		return nil, c.Errf("missing mandatory property: %q", "to")
	}
	for _, host := range u.hosts {
		addr, tlsServerName := SplitByByte(host.addr, '@')
		host.addr = addr

		host.transport = newTransport()
		// Inherit from global transport settings
		host.transport.recursionDesired = u.transport.recursionDesired
		host.transport.expire = u.transport.expire
		if host.proto == transport.TLS {
			// Deep copy
			host.transport.tlsConfig = new(tls.Config)
			host.transport.tlsConfig.Certificates = u.transport.tlsConfig.Certificates
			host.transport.tlsConfig.RootCAs = u.transport.tlsConfig.RootCAs
			// Don't set TLS server name if addr host part is already a domain name
			if hostPortIsIpPort(addr) {
				host.transport.tlsConfig.ServerName = u.transport.tlsConfig.ServerName
			}

			// TLS server name in tls:// takes precedence over the global one(if any)
			if len(tlsServerName) != 0 {
				tlsServerName = tlsServerName[1:]
				serverName, ok := stringToDomain(tlsServerName)
				if !ok {
					return nil, c.Errf("invalid TLS server name %q", tlsServerName)
				}
				host.transport.tlsConfig.ServerName = serverName
			}
		}

		network := protoToNetwork(host.proto)
		if network == "dns" {
			// Use classic DNS protocol for health checking
			network = "udp"
		}
		host.c = &dns.Client{
			Net:       network,
			TLSConfig: host.transport.tlsConfig,
			Timeout:   defaultHcTimeout,
		}
		host.InitDOH(u)
	}

	if err := u.inline.ForEachDomain(func(name string) error {
		// except takes precedence over INLINE
		if u.ignored.Match(name) {
			return c.Errf("%q %v is conflict with %q", "INLINE", name, "except")
		}
		return nil
	}); err != nil {
		return nil, err
	}

	if u.matchAny {
		if u.inline.Len() != 0 {
			return nil, c.Errf("INLINE %q is forbidden since %q will match all requests", u.inline, ".")
		}
		if u.pathReload != 0 {
			log.Debugf("Reset path_reload %v to zero since %q is matched", u.pathReload, ".")
			u.pathReload = 0
		}
		if u.urlReload != 0 {
			log.Debugf("Reset url_reload %v to zero since %q is matched", u.urlReload, ".")
			u.urlReload = 0
		}
	} else {
		hasPath := false
		hasUrl := false
		for _, item := range u.NameList.items {
			switch item.whichType {
			case NameItemTypePath:
				hasPath = true
			case NameItemTypeUrl:
				hasUrl = true
			default:
				panic(fmt.Sprintf("Unexpected NameItem type %v", item.whichType))
			}
		}
		if !hasPath {
			log.Debugf("Reset path_reload %v to zero since no path found", u.pathReload)
			u.NameList.pathReload = 0
		}
		if !hasUrl {
			log.Debugf("Reset url_reload %v to zero since no url found", u.urlReload)
			u.NameList.urlReload = 0
		}
	}

	if u.inline.Len() != 0 {
		log.Infof("inline: %v", u.inline)
	}

	return u, nil
}

func parseFrom(c *caddy.Controller, u *reloadableUpstream) error {
	forms := c.RemainingArgs()
	n := len(forms)
	if n == 0 {
		return c.ArgErr()
	}

	if n == 1 && forms[0] == "." {
		u.matchAny = true
		log.Infof("Match any")
		return nil
	}

	config := dnsserver.GetConfig(c)
	for _, from := range forms {
		if strings.Index(from, "://") > 0 {
			continue
		}

		if !filepath.IsAbs(from) && config.Root != "" {
			from = filepath.Join(config.Root, from)
		}

		st, err := os.Stat(from)
		if err != nil {
			if os.IsNotExist(err) {
				log.Warningf("File %q doesn't exist", from)
			} else {
				return err
			}
		} else if st != nil && !st.Mode().IsRegular() {
			log.Warningf("File %q isn't a regular file", from)
		}
	}

	items, err := NewNameItemsWithForms(forms)
	if err != nil {
		return err
	}
	u.items = items
	log.Infof("FROM...: %v", forms)
	return nil
}

func parseBlock(c *caddy.Controller, u *reloadableUpstream) error {
	switch dir := c.Val(); dir {
	case "path_reload":
		dur, err := parseDuration(c)
		if err != nil {
			return err
		}
		if dur < minPathReloadInterval && dur != 0 {
			return c.Errf("%v: minimal interval is %v", dir, minPathReloadInterval)
		}
		u.pathReload = dur
		log.Infof("%v: %v", dir, u.pathReload)
	case "url_reload":
		args := c.RemainingArgs()
		n := len(args)
		if n != 1 && n != 2 {
			return c.ArgErr()
		}
		dur, err := parseDuration0(dir, args[0])
		if err != nil {
			return c.Err(err.Error())
		}
		if dur < minUrlReloadInterval && dur != 0 {
			return c.Errf("%v: minimal reload interval is %v", dir, minUrlReloadInterval)
		}
		if n == 2 {
			dur, err := parseDuration0(dir, args[1])
			if err != nil {
				return c.Err(err.Error())
			}
			if dur < minUrlReadTimeout {
				return c.Errf("%v: minimal read timeout is %v", dir, minUrlReadTimeout)
			}
			u.urlReadTimeout = dur
		}
		u.urlReload = dur
		log.Infof("%v: %v %v", dir, u.urlReload, u.urlReadTimeout)
	case "except":
		// Multiple "except"s will be merged together
		args := c.RemainingArgs()
		if len(args) == 0 {
			return c.ArgErr()
		}
		for _, name := range args {
			if !u.ignored.Add(name) {
				log.Warningf("%q isn't a domain name", name)
			}
		}
		log.Infof("%v: %v", dir, u.ignored)
	case "spray":
		if len(c.RemainingArgs()) != 0 {
			return c.ArgErr()
		}
		u.spray = &Spray{}
		log.Infof("%v: enabled", dir)
	case "policy":
		arr := c.RemainingArgs()
		if len(arr) != 1 {
			return c.ArgErr()
		}
		policy, ok := SupportedPolicies[arr[0]]
		if !ok {
			return c.Errf("unknown policy: %q", arr[0])
		}
		u.policy = policy
		log.Infof("%v: %v", dir, arr[0])
	case "max_fails":
		n, err := parseInt32(c)
		if err != nil {
			return err
		}
		u.maxFails = n
		log.Infof("%v: %v", dir, n)
	case "health_check":
		args := c.RemainingArgs()
		n := len(args)
		if n != 1 && n != 2 {
			return c.ArgErr()
		}
		dur, err := parseDuration0(dir, args[0])
		if err != nil {
			return c.Err(err.Error())
		}
		if dur < minHcInterval && dur != 0 {
			return c.Errf("%v: minimal interval is %v", dir, minHcInterval)
		}
		if n == 2 && args[1] != "no_rec" {
			return c.Errf("%v: unknown option: %v", dir, args[1])
		}
		u.checkInterval = dur
		u.transport.recursionDesired = n == 1
		log.Infof("%v: %v %v", dir, u.checkInterval, u.transport.recursionDesired)
	case "to":
		// Multiple "to"s will be merged together
		if err := parseTo(c, u); err != nil {
			return err
		}
	case "expire":
		dur, err := parseDuration(c)
		if err != nil {
			return err
		}
		if dur < minExpireInterval && dur != 0 {
			return c.Errf("%v: minimal interval is %v", dir, minExpireInterval)
		}
		u.transport.expire = dur
		log.Infof("%v: %v", dir, dur)
	case "tls":
		args := c.RemainingArgs()
		if len(args) > 3 {
			return c.ArgErr()
		}
		tlsConfig, err := pkgtls.NewTLSConfigFromArgs(args...)
		if err != nil {
			return err
		}
		// Merge server name if tls_servername set previously
		tlsConfig.ServerName = u.transport.tlsConfig.ServerName
		u.transport.tlsConfig = tlsConfig
		log.Infof("%v: %v", dir, args)
	case "tls_servername":
		args := c.RemainingArgs()
		if len(args) != 1 {
			return c.ArgErr()
		}
		serverName, ok := stringToDomain(args[0])
		if !ok {
			return c.Errf("%v: %q isn't a valid domain name", dir, args[0])
		}
		u.transport.tlsConfig.ServerName = serverName
		log.Infof("%v: %v", dir, serverName)
	case "bootstrap":
		if err := parseBootstrap(c, u); err != nil {
			return err
		}
	case "ipset":
		if err := ipsetParse(c, u); err != nil {
			return err
		}
	case "pf":
		if err := pfParse(c, u); err != nil {
			return err
		}
	case "no_ipv6":
		args := c.RemainingArgs()
		if len(args) != 0 {
			return c.ArgErr()
		}
		u.noIPv6 = true
		log.Infof("%v: %v", dir, u.noIPv6)
	default:
		if len(c.RemainingArgs()) != 0 ||!u.inline.Add(dir) {
			return c.Errf("unknown property: %q", dir)
		}
		if u.ignored.Len() != 0 {
			return c.Errf("%q must comes before %q", "INLINE", "except")
		}
	}
	return nil
}

// Return a non-negative int32
// see: https://golang.org/pkg/builtin/#int
func parseInt32(c *caddy.Controller) (int32, error) {
	dir := c.Val()
	args := c.RemainingArgs()
	if len(args) != 1 {
		return 0, c.ArgErr()
	}

	n, err := strconv.Atoi(args[0])
	if err != nil {
		return 0, err
	}

	// In case of n is 64-bit
	if n < 0 || n > 0x7fffffff {
		return 0, c.Errf("%v: value %v of out of non-negative int32 range", dir, n)
	}

	return int32(n), nil
}

func parseDuration0(dir, arg string) (time.Duration, error) {
	duration, err := time.ParseDuration(arg)
	if err != nil {
		return 0, err
	}

	if duration < 0 {
		return 0, errors.New(fmt.Sprintf("%v: negative time duration %v", dir, arg))
	}
	return duration, nil
}

// Return a non-negative time.Duration and an error(if any)
func parseDuration(c *caddy.Controller) (time.Duration, error) {
	dir := c.Val()
	args := c.RemainingArgs()
	if len(args) != 1 {
		return 0, c.ArgErr()
	}
	dur, err := parseDuration0(dir, args[0])
	if err == nil {
		return dur, nil
	}
	return dur, c.Err(err.Error())
}

func parseTo(c *caddy.Controller, u *reloadableUpstream) error {
	args := c.RemainingArgs()
	if len(args) == 0 {
		return c.ArgErr()
	}

	toHosts, err := HostPort(args)
	if err != nil {
		return err
	}

	for _, host := range toHosts {
		trans, addr := SplitTransportHost(host)
		log.Infof("Transport: %v Address: %v", trans, addr)

		uh := &UpstreamHost{
			proto: trans,
			// Not an error, host and tls server name will be separated later
			addr: addr,
			downFunc: checkDownFunc(u),
		}
		u.hosts = append(u.hosts, uh)

		log.Infof("Upstream: %v", uh)
	}

	return nil
}

func parseBootstrap(c *caddy.Controller, u *reloadableUpstream) error {
	dir := c.Val()
	args := c.RemainingArgs()
	if len(args) == 0 {
		return c.ArgErr()
	}

	var list []string
	for _, hp := range args {
		host, port, err := net.SplitHostPort(hp)
		if err != nil {
			if strings.Contains(err.Error(), "missing port in address") {
				host = hp
				port = "53"
			} else {
				return c.Errf("%v: %v", dir, err)
			}
		} else {
			if port, err := strconv.Atoi(port); err != nil || port <= 0 {
				if err == nil {
					err =  fmt.Errorf("non-positive port %v", port)
				}
				return c.Errf("%v: %v", dir, err)
			}
		}

		if strings.HasPrefix(host, "[") {
			if !strings.HasSuffix(host, "]") {
				panic(fmt.Sprintf("Why %q doesn't have close bracket?!", host))
			}
			// Strip the brackets
			host = host[1:len(host)-1]
		}

		// XXX: Doesn't support IPv6 with zone
		ip := net.ParseIP(host)
		if ip == nil {
			return c.Errf("%v: %q isn't a valid IP address", dir, host)
		}
		if ip.To4() != nil {
			hp = ip.String() + ":" + port
		} else {
			if ip.To16() == nil {
				panic(fmt.Sprintf("%v: expected an IPv6 address, got %v", dir, ip))
			}
			hp = "[" + ip.String() + "]:" + port
		}

		list = append(list, hp)
	}

	u.bootstrap = append(u.bootstrap, list...)
	log.Infof("%v: %v", dir, list)
	return nil
}

const (
	defaultMaxFails       = 3

	defaultPathReloadInterval = 2 * time.Second
	defaultUrlReloadInterval  = 30 * time.Minute
	defaultUrlReadTimeout = 30 * time.Second

	defaultHcInterval     = 2000 * time.Millisecond
	defaultHcTimeout      = 5000 * time.Millisecond
)

const (
	minPathReloadInterval = 1 * time.Second
	minUrlReloadInterval  = 15 * time.Second
	minUrlReadTimeout = 3 * time.Second

	minHcInterval     = 1 * time.Second
	minExpireInterval = 1 * time.Second
)
