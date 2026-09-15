package providers

import "github.com/chengongliang/keygrid/internal/oauth"

// init.go 适配器注册（对应 9router src/lib/oauth/providers/index.js）。
// kimi/openai/anthropic 与国内 provider OAuth 适配（iflow/qoder/trae/codebuddy-cn）。
func init() {
	oauth.Register(Kimi{})
	oauth.Register(OpenAI{})
	oauth.Register(Anthropic{})
	oauth.Register(IFlow{})
	oauth.Register(Qoder{})
	oauth.Register(Trae{})
	oauth.Register(CodeBuddyCN{})
}
