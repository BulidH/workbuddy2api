// vpc.go 企业 VPC 专属端点解析：把实例名或实例域名推导成官方三个主机。
package qoder

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// VPCDomainSuffix 企业 VPC 域名后缀（与 qoder2api-hub 一致）。
const VPCDomainSuffix = "vpc.qoder.com.cn"

var vpcLabelRe = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,53}[a-z0-9])?$`)

// ParseVPCEndpoint 把实例名（acme）或三个按服务拆分的域名
// （acme.vpc.qoder.com.cn / acme-openapi.vpc.qoder.com.cn / acme-gateway...）
// 解析成端点。路径、端口、凭据、query、fragment 或空白一律拒绝。
func ParseVPCEndpoint(raw string) (Endpoint, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return Endpoint{}, fmt.Errorf("empty vpc endpoint")
	}
	if strings.ContainsAny(raw, "\r\n\t") {
		return Endpoint{}, fmt.Errorf("invalid vpc endpoint: control characters")
	}
	target := raw
	if !strings.Contains(target, "://") {
		target = "https://" + target
	}
	u, err := url.Parse(target)
	if err != nil {
		return Endpoint{}, fmt.Errorf("invalid vpc endpoint: %w", err)
	}
	if u.Scheme != "https" || u.Hostname() == "" || u.Port() != "" ||
		u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
		(u.Path != "" && u.Path != "/") {
		return Endpoint{}, fmt.Errorf("invalid vpc endpoint: expected an instance name or a domain under %s", VPCDomainSuffix)
	}
	host := strings.ToLower(u.Hostname())
	instance := host
	if strings.HasSuffix(host, "."+VPCDomainSuffix) {
		instance = strings.TrimSuffix(host, "."+VPCDomainSuffix)
		instance = strings.TrimSuffix(instance, "-openapi")
		instance = strings.TrimSuffix(instance, "-gateway")
	} else if strings.Contains(host, ".") {
		return Endpoint{}, fmt.Errorf("invalid vpc endpoint: expected an instance name or a domain under %s", VPCDomainSuffix)
	}
	if !vpcLabelRe.MatchString(instance) {
		return Endpoint{}, fmt.Errorf("invalid vpc endpoint: bad instance name %q", instance)
	}
	domain := instance + "." + VPCDomainSuffix
	return Endpoint{
		OpenAPI: "https://" + instance + "-openapi." + VPCDomainSuffix,
		Gateway: "https://" + instance + "-gateway." + VPCDomainSuffix,
		Website: "https://" + domain,
		Domain:  domain,
	}, nil
}
