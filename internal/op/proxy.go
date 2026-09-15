package op

import (
	"sync"
	"time"

	"github.com/chengongliang/keygrid/internal/model"
)

// proxy.go 平台出口代理读取（relay 转发热路径每渠道调用，直查 DB 会多一次往返）。
// 30s TTL 缓存：管理员修改代理配置后最长 30s 全量生效，可接受（保存即热生效的
// 语义对管理端读路径不变，转发面走缓存）。

const proxyTTL = 30 * time.Second

type proxyCache struct {
	mu      sync.Mutex
	value   string
	expires time.Time
}

// ProxyURL 读平台代理地址（SettingProxyURL）；未配置返回 ("", nil)。
func (o *Op) ProxyURL() (string, error) {
	if o.proxyCache == nil {
		o.proxyCache = &proxyCache{}
	}
	c := o.proxyCache
	c.mu.Lock()
	defer c.mu.Unlock()
	if time.Now().Before(c.expires) {
		return c.value, nil
	}
	v, err := o.GetSetting(model.SettingProxyURL)
	if err != nil {
		return "", err
	}
	c.value, c.expires = v, time.Now().Add(proxyTTL)
	return v, nil
}
