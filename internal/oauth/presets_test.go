package oauth

import "testing"

// 需代理站点的预设标记：OpenAI 默认启用代理（用户可取消勾选），其余预设不默认。
func TestOpenAIPresetNeedsProxy(t *testing.T) {
	p, ok := LookupPreset("openai")
	if !ok {
		t.Fatal("openai preset missing")
	}
	if !p.NeedsProxy {
		t.Fatal("openai preset must default to proxy (NeedsProxy=true)")
	}
}

func TestOtherPresetsNoDefaultProxy(t *testing.T) {
	for _, key := range []string{"kimi", "iflow", "qoder", "glm", "deepseek", "siliconflow"} {
		if p, ok := LookupPreset(key); ok && p.NeedsProxy {
			t.Errorf("preset %q must not default to proxy", key)
		}
	}
}
