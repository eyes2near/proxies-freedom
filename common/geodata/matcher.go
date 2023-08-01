package geodata

import (
	"net"
	"regexp"
	"strings"

	"pfreedom/common/log"

	v2router "github.com/v2fly/v2ray-core/v4/app/router"
)

func matchDomain(list []*v2router.Domain, target string) bool {
	for _, d := range list {
		switch d.GetType() {
		case v2router.Domain_Full:
			domain := d.GetValue()
			if domain == target {
				log.Tracef("domain %s hit domain(full) rule: %s", target, domain)
				return true
			}
		case v2router.Domain_Domain:
			domain := d.GetValue()
			if strings.HasSuffix(target, domain) {
				idx := strings.Index(target, domain)
				if idx == 0 || target[idx-1] == '.' {
					log.Tracef("domain %s hit domain rule: %s", target, domain)
					return true
				}
			}
		case v2router.Domain_Plain:
			// keyword
			if strings.Contains(target, d.GetValue()) {
				log.Tracef("domain %s hit keyword rule: %s", target, d.GetValue())
				return true
			}
		case v2router.Domain_Regex:
			matched, err := regexp.Match(d.GetValue(), []byte(target))
			if err != nil {
				log.Error("invalid regex", d.GetValue())
				return false
			}
			if matched {
				log.Tracef("domain %s hit regex rule: %s", target, d.GetValue())
				return true
			}
		default:
			log.Debug("unknown rule type:", d.GetType().String())
		}
	}
	return false
}

func matchIP(list []*v2router.CIDR, target net.IP) bool {
	isIPv6 := true
	len := net.IPv6len
	if target.To4() != nil {
		len = net.IPv4len
		isIPv6 = false
	}
	for _, c := range list {
		n := int(c.GetPrefix())
		mask := net.CIDRMask(n, 8*len)
		cidrIP := net.IP(c.GetIp())
		if cidrIP.To4() != nil { // IPv4 CIDR
			if isIPv6 {
				continue
			}
		} else { // IPv6 CIDR
			if !isIPv6 {
				continue
			}
		}
		subnet := &net.IPNet{IP: cidrIP.Mask(mask), Mask: mask}
		if subnet.Contains(target) {
			return true
		}
	}
	return false
}
