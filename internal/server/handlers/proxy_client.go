package handlers

import (
	"errors"
	"net/http"
	"time"

	"github.com/chengongliang/keygrid/internal/httpx"
	"github.com/chengongliang/keygrid/internal/op"
	"github.com/chengongliang/keygrid/internal/ssrf"
)

// proxy_client.go 探测/测试类请求的上游 client（转发主循环 clientFor 的 handler 侧对应物）：
// 渠道勾选代理 → 走平台统一代理（用户不能自填代理地址，出口统一由管理员控制）；
// 勾选代理但平台未配置 → 返回错误，语义与转发路径一致（不静默直连）。
// 未勾选代理 → 原有 ssrf.SafeTransport client（私网 dial 防护保留）。
// 注意：走代理时 SafeTransport 的 DialContext 不生效 —— 出口信任链转移到代理侧，
// base_url 仍在保存/探测入口做过 SSRF 静态校验。
func probeClient(o *op.Op, useProxy bool, timeout time.Duration) (*http.Client, error) {
	if !useProxy {
		return &http.Client{Timeout: timeout, Transport: ssrf.SafeTransport(timeout)}, nil
	}
	proxyURL, err := o.ProxyURL()
	if err != nil {
		return nil, err
	}
	if proxyURL == "" {
		return nil, errors.New("channel requires proxy, but platform proxy is not configured")
	}
	return httpx.Client(timeout, proxyURL)
}
