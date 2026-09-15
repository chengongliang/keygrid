package relay

import (
	"testing"

	"github.com/chengongliang/keygrid/internal/model"
)

// clientFor 渠道级出口代理：未勾选 → 默认直连 client；
// 勾选但 Op 不可用/平台未配置 → 明确报错（转发主循环 failover，不静默直连）。
func TestClientForDirect(t *testing.T) {
	h := NewHandler(nil)
	c, err := h.clientFor(&model.Provider{Name: "glm", UseProxy: false})
	if err != nil {
		t.Fatalf("direct client: %v", err)
	}
	if c != h.HTTPClient {
		t.Fatal("non-proxy channel must use the shared direct client")
	}
}

func TestClientForProxyRequiresConfig(t *testing.T) {
	h := NewHandler(nil) // Op=nil：代理配置不可读 → 一律视为未配置
	_, err := h.clientFor(&model.Provider{Name: "codex", UseProxy: true})
	if err == nil {
		t.Fatal("proxy channel without configured proxy must fail")
	}
}
