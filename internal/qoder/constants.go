package qoder

// constants.go Qoder 上游常量，移植自 9router open-sse/shared/qoder/constants.js。
//
// Qoder 有两个区域站点，端点形状完全一致，只有主机名不同：
//
//	              国际站(intl)              国内站(cn)
//	设备流+用户信息  openapi.qoder.sh          openapi.qoder.com.cn
//	token 刷新      center.qoder.sh           gateway.qoder.com.cn
//	推理+模型目录   api3.qoder.sh             gateway.qoder.com.cn
//	浏览器授权页    qoder.com/device/...      qoder.com.cn/device/...
//
// 推理与模型列表都挂在 /algo 前缀下，并且每次请求都要 COSY 签名（cosy.go）。
const (
	// RegionIntl 国际站。
	RegionIntl = "intl"
	// RegionCN 国内站。
	RegionCN = "cn"
)

// COSY 头常量：上游签名校验会核对这几项，不能随意改。
const (
	IDEVersion   = "1.0.0"
	ClientType   = "5"
	DataPolicy   = "disagree"
	LoginVersion = "v2"
	MachineOS    = "x86_64_windows"
	MachineType  = "5"
)

// 推理相关路径（都在 {chatBase}/algo 之下）。
const (
	// ChatSigPath 对话端点（SSE，私有协议）。
	ChatSigPath = "/api/v2/service/pro/sse/agent_chat_generation"
	// ModelListPath 模型目录（含每个模型的 model_config）。
	ModelListPath = "/api/v2/model/list"
	// ImageUploadSigPath 图片上传（multipart PUT）。
	ImageUploadSigPath = "/api/v2/image/upload"
)

// 载荷体量约束（9router mediaConfig / qoder constants）。
const (
	// MaxImageBytes 单图上传上限，超过则替换为占位文本。
	MaxImageBytes = 10 << 20
	// MaxPayloadBytes 重写后 messages 仍超过此体积，就继续剥离残留 data URI。
	MaxPayloadBytes = 6 << 20
	// InlineFallbackMaxBytes 上传失败时仍可内联的 data URI 上限。
	InlineFallbackMaxBytes = 512 << 10
)

// ContextTierHeadroom 上下文档位选择的预估余量（分词误差留白）。
const ContextTierHeadroom = 0.15

// ContextTierEnv 档位策略环境变量：auto（默认）| max | default | 具体档位名（如 1M）。
const ContextTierEnv = "QODER_CONTEXT_TIER"

// rsaPublicKey COSY 加密用的 RSA 公钥（取自 Qoder IDE v0.9，与 qodercli 线上流量一致）。
const rsaPublicKey = `-----BEGIN PUBLIC KEY-----
MIGfMA0GCSqGSIb3DQEBAQUAA4GNADCBiQKBgQDA8iMH5c02LilrsERw9t6Pv5Nc
4k6Pz1EaDicBMpdpxKduSZu5OANqUq8er4GM95omAGIOPOh+Nx0spthYA2BqGz+l
6HRkPJ7S236FZz73In/KVuLnwI8JJ2CbuJap8kvheCCZpmAWpb/cPx/3Vr/J6I17
XcW+ML9FoCI6AOvOzwIDAQAB
-----END PUBLIC KEY-----`
