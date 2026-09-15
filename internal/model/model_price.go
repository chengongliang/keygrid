package model

import "time"

// ModelPrice admin 全局模型价格表（计费）：模型 → 每 1M tokens 单价（USD）。
// 匹配规则：精确模型名 > "*" 兜底行 > 未定价（cost=0，免费放行且不计入额度累计）。
// 价格在请求完成时随 usage_logs.cost 落库（价格快照），调价不追溯历史费用。
type ModelPrice struct {
	ID int64 `gorm:"primaryKey" json:"id"`
	// Model 精确模型名（与请求入口模型名一致）；"*" 为兜底价行（想对未定价模型收费时配置）。
	Model string `gorm:"uniqueIndex;not null" json:"model"`
	// PromptPrice / CompletionPrice 单价：USD / 1M tokens（对齐官方 API 定价习惯）。
	PromptPrice     float64   `json:"prompt_price"`
	CompletionPrice float64   `json:"completion_price"`
	Remark          string    `json:"remark"` // 备注（如"对齐官方 API 定价"）
	UpdatedAt       time.Time `json:"updated_at"`
	CreatedAt       time.Time `json:"created_at"`
}

func (ModelPrice) TableName() string { return "model_prices" }
