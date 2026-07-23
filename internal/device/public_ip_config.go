package device

import "github.com/zanescope/vohive/internal/config"

type publicIPProbeSourceConfigurer interface {
	SetPublicIPProbeURLs(ipv4URLs, ipv6URLs []string)
}

func (p *Pool) configurePublicIPProbeSources(target publicIPProbeSourceConfigurer) {
	if target == nil {
		return
	}
	sources := config.DefaultPublicIPProbeConfig()
	if p != nil && p.cfg != nil {
		if len(p.cfg.PublicIPProbe.IPv4URLs) > 0 {
			sources.IPv4URLs = append([]string(nil), p.cfg.PublicIPProbe.IPv4URLs...)
		}
		if len(p.cfg.PublicIPProbe.IPv6URLs) > 0 {
			sources.IPv6URLs = append([]string(nil), p.cfg.PublicIPProbe.IPv6URLs...)
		}
	}
	target.SetPublicIPProbeURLs(sources.IPv4URLs, sources.IPv6URLs)
}
