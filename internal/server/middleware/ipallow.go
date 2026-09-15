package middleware

import (
	"net"
	"strings"
)

// IPAllowed 检查 clientIP 是否命中白名单。whitelist 为逗号分隔的
// 精确 IP（1.2.3.4）或 CIDR（10.0.0.0/8）；空白名单 = 不限制，恒放行。
// 解析失败的条目直接视为不命中（写入端已校验格式）。
func IPAllowed(whitelist, clientIP string) bool {
	if strings.TrimSpace(whitelist) == "" {
		return true
	}
	ip := net.ParseIP(clientIP)
	if ip == nil {
		return false
	}
	for _, item := range strings.Split(whitelist, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if _, cidr, err := net.ParseCIDR(item); err == nil {
			if cidr.Contains(ip) {
				return true
			}
			continue
		}
		if want := net.ParseIP(item); want != nil && want.Equal(ip) {
			return true
		}
	}
	return false
}

// ModelAllowed 检查请求模型是否在 key 的模型限制内。limit 空 = 不限制。
// 逗号分隔模型名，精确匹配。
func ModelAllowed(limit, model string) bool {
	if strings.TrimSpace(limit) == "" {
		return true
	}
	for _, m := range strings.Split(limit, ",") {
		if strings.TrimSpace(m) == model {
			return true
		}
	}
	return false
}
