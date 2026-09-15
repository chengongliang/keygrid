package oauth

// presets.go 国内 provider 预设目录（数据源自 9router open-sse/providers/registry）。
//
// 三类消费方：
//   - GET /api/providers/meta       → 前端「添加渠道」向导的 provider 选择列表
//   - GET /api/providers/presets    → 向导表单预填（base_url/模型目录/协议/认证方式）
//   - 后端 Create handler 的 oauth 默认 base_url（ProviderPresets 单一事实来源）
//
// Models 只挑各平台主流可直接调用的模型（全量列表见 9router registry）；
// 用户仍可在 model_map 自定义任意映射。

// ProviderPreset 单个 provider 的预设模板。
type ProviderPreset struct {
	Key         string   `json:"key"`                   // 'glm' | 'deepseek' | ... | oauth 用 adapter key
	Kind        string   `json:"kind"`                  // 'api_key' | 'oauth'
	Protocol    string   `json:"protocol"`              // 'openai' | 'anthropic'
	BaseURL     string   `json:"base_url"`              // chat completions 完整可转发 base（relay 拼 /v1/chat/completions）
	APIKeyURL   string   `json:"api_key_url,omitempty"` // 去哪申请 key（前端展示用）
	SignupURL   string   `json:"signup_url,omitempty"`  // OAuth 类注册页
	Models      []string `json:"models"`                // 内置模型目录
	DefaultPri  int      `json:"default_priority"`      // 9router priority 对齐，方便多渠道排布
	AuthMode    string   `json:"auth_mode,omitempty"`   // oauth 附加说明：'oauth+apikey 双模式' 等
	Description string   `json:"description,omitempty"` // 一句话说明（聚合平台、订阅额度等）
	// NeedsProxy 该站点通常需代理才能访问（如 OpenAI）。前端选预设时默认勾选
	// 「通过平台代理访问」；后端创建时未显式传 use_proxy 也按此默认。
	NeedsProxy bool `json:"needs_proxy,omitempty"`
}

// providerPresets 全量预设（顺序即前端展示顺序）。
var providerPresets = []ProviderPreset{
	// ---- api_key 类（OpenAI 兼容）----
	{
		Key: "glm", Kind: "api_key", Protocol: "openai",
		BaseURL:     "https://open.bigmodel.cn/api/coding/paas/v4",
		APIKeyURL:   "https://open.bigmodel.cn/usercenter/apikeys",
		Models:      []string{"glm-5.3", "glm-5.3-flash", "glm-5.2", "glm-5.1", "glm-5", "glm-4.7", "glm-4.6v", "glm-4.6", "glm-4.5-air"},
		DefaultPri:  130,
		Description: "智谱 GLM（中国站），Coding 计划专用端点",
	},
	{
		Key: "deepseek", Kind: "api_key", Protocol: "openai",
		BaseURL:     "https://api.deepseek.com",
		APIKeyURL:   "https://platform.deepseek.com/api_keys",
		Models:      []string{"deepseek-v4-pro", "deepseek-v4-flash", "deepseek-chat", "deepseek-reasoner"},
		DefaultPri:  110,
		Description: "DeepSeek 官方 API（OpenAI 兼容）",
	},
	{
		Key: "siliconflow", Kind: "api_key", Protocol: "openai",
		BaseURL:     "https://api.siliconflow.com/v1",
		APIKeyURL:   "https://cloud.siliconflow.com/account/ak",
		Models:      []string{"deepseek-ai/DeepSeek-V4-Pro", "deepseek-ai/DeepSeek-V4-Flash", "deepseek-ai/DeepSeek-V3.2", "deepseek-ai/DeepSeek-R1", "Qwen/Qwen3.5-397B-A17B", "Qwen/Qwen3.5-122B-A10B", "zai-org/GLM-5.1", "zai-org/GLM-5", "moonshotai/Kimi-K2.6", "openai/gpt-oss-120b", "MiniMaxAI/MiniMax-M2.5"},
		DefaultPri:  250,
		Description: "硅基流动聚合平台：DeepSeek/Qwen/GLM/Kimi 一站可用",
	},
	{
		Key: "volcengine-ark", Kind: "api_key", Protocol: "openai",
		BaseURL:     "https://ark.cn-beijing.volces.com/api/coding/v3",
		APIKeyURL:   "https://console.volcengine.com/ark/region:ark+cn-beijing/apiKey",
		Models:      []string{"Doubao-Seed-2.0-Code", "Doubao-Seed-2.0-pro", "Doubao-Seed-2.0-lite", "Doubao-Seed-Code", "DeepSeek-V4-Pro", "DeepSeek-V4-Flash", "GLM-5.1", "Kimi-K2.6", "MiniMax-M2.7"},
		DefaultPri:  270,
		Description: "火山方舟（豆包 Doubao-Seed 系列 + 聚合模型）",
	},
	{
		Key: "qianfan", Kind: "api_key", Protocol: "openai",
		BaseURL:     "https://qianfan.baidubce.com/v2",
		APIKeyURL:   "https://console.bce.baidu.com/qianfan/ais/console/applicationConsole/application",
		Models:      []string{"deepseek-v4-pro", "deepseek-v4-flash", "glm-5.2", "glm-5.1", "kimi-k2.6", "qwen3.5-397b-a17b"},
		DefaultPri:  120,
		Description: "百度千帆 v2（OpenAI 兼容）",
	},
	{
		Key: "hunyuan", Kind: "api_key", Protocol: "openai",
		BaseURL:     "https://api.hunyuan.cloud.tencent.com/v1",
		APIKeyURL:   "https://console.cloud.tencent.com/hunyuan/api-key",
		Models:      []string{"hunyuan-turbos-latest", "hunyuan-t1-latest"},
		DefaultPri:  115,
		Description: "腾讯混元",
	},
	{
		Key: "xiaomi-mimo", Kind: "api_key", Protocol: "openai",
		BaseURL:     "https://api.xiaomimimo.com/v1",
		APIKeyURL:   "https://platform.xiaomimimo.com/console/api-keys",
		Models:      []string{"mimo-v2.5-pro", "mimo-v2.5", "mimo-v2-omni", "mimo-v2-flash"},
		DefaultPri:  290,
		Description: "小米 MiMo",
	},
	{
		Key: "minimax-cn", Kind: "api_key", Protocol: "anthropic",
		BaseURL:     "https://api.minimaxi.com",
		APIKeyURL:   "https://platform.minimaxi.com/user-center/basic-information/interface-key",
		Models:      []string{"MiniMax-M3", "MiniMax-M2.7", "MiniMax-M2.5", "MiniMax-M2.1"},
		DefaultPri:  190,
		Description: "MiniMax 中国（anthropic 协议 /v1/messages，OpenAI 兼容端点也可用）",
	},

	// ---- OAuth 订阅类 ----
	{
		Key: "openai", Kind: "oauth", Protocol: "openai",
		BaseURL:     "https://chatgpt.com/backend-api/codex/responses",
		SignupURL:   "https://chatgpt.com/codex",
		Models:      []string{"gpt-5.4", "gpt-5.4-mini", "gpt-5.5", "gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna", "gpt-5.3-codex-spark", "gpt-5.6-sol-image"},
		AuthMode:    "PKCE 浏览器授权（ChatGPT Plus/Pro 订阅；授权后粘贴回调 URL）",
		NeedsProxy:  true, // OpenAI 站点国内直连不可达，默认启用平台代理（用户可取消勾选）
		Description: "OpenAI Codex（ChatGPT 订阅额度，Responses 协议，平台自动续期）",
	},
	{
		Key: "kimi", Kind: "oauth", Protocol: "anthropic",
		BaseURL:   "https://api.kimi.com/coding",
		SignupURL: "https://www.kimi.com/code",
		Models:    []string{"kimi-k3", "k3", "kimi-for-coding", "kimi-for-coding-highspeed", "kimi-k2.7-code", "kimi-k2.6", "kimi-k2.5", "kimi-latest"},
		AuthMode:  "device_code 授权登录",
	},
	{
		Key: "iflow", Kind: "oauth", Protocol: "openai",
		BaseURL:     "https://apis.iflow.cn/v1",
		SignupURL:   "https://iflow.cn",
		Models:      []string{"qwen3-coder-plus", "qwen3-max", "qwen3-vl-plus", "qwen3-235b", "kimi-k2", "deepseek-v3.2", "glm-4.7"},
		AuthMode:    "authorization_code + Basic Auth（手机号登录）",
		Description: "心流 iFlow（阿里系，免费额度大方）",
	},
	{
		Key: "qoder", Kind: "oauth", Protocol: "openai",
		BaseURL:     "https://api3.qoder.sh",
		SignupURL:   "https://qoder.com",
		Models:      []string{"ultimate", "auto", "performance", "efficient", "qmodel_preview", "qmodel_latest", "kmodel_latest", "gm51model", "dmodel", "dfmodel"},
		AuthMode:    "device_token 轮询（本地 PKCE+nonce+machine_id）；也支持 PAT (pt-…)",
		Description: "Qoder（阿里系 IDE 订阅）",
	},
	{
		Key: "trae", Kind: "oauth", Protocol: "openai",
		BaseURL:     "https://core-normal.trae.ai/api/remote/v1",
		SignupURL:   "https://www.trae.ai",
		Models:      []string{"auto", "work", "gemini-3.1-pro", "gemini-3-flash-solo", "minimax-m3", "kimi-k2.5", "gpt-5.4"},
		AuthMode:    "GetLoginGuidance → ExchangeToken（marscode/trae.ai 双域名）",
		Description: "Trae（字节跳动 marscode）",
	},
	{
		Key: "codebuddy-cn", Kind: "oauth", Protocol: "openai",
		BaseURL:     "https://copilot.tencent.com/v2",
		SignupURL:   "https://copilot.tencent.com",
		Models:      []string{"glm-5.2", "glm-5.1", "glm-5.0", "minimax-m3", "kimi-k2.7", "kimi-k2.6", "hy3-preview", "deepseek-v4-pro", "deepseek-v4-flash"},
		AuthMode:    "oauth+apikey 双模式（腾讯 CodeBuddy）",
		Description: "CodeBuddy CN（腾讯云 AI 编程助手，OpenAI 兼容统一网关）",
	},
}

// ProviderPresets 预设只读副本（handler 直接 range 使用）。
func ProviderPresets() []ProviderPreset {
	out := make([]ProviderPreset, len(providerPresets))
	copy(out, providerPresets)
	return out
}

// LookupPreset 按 key 查预设（oauth 默认 base_url 用）。
func LookupPreset(key string) (ProviderPreset, bool) {
	for _, p := range providerPresets {
		if p.Key == key {
			return p, true
		}
	}
	return ProviderPreset{}, false
}

// DefaultBaseURL 兼容旧 map 语义：oauth provider 默认上游。
// kimi/openai/anthropic 之外的新增 oauth provider 从 presets 取。
func DefaultBaseURL(oauthProviderKey string) string {
	if v, ok := LookupPreset(oauthProviderKey); ok {
		return v.BaseURL
	}
	return ""
}
