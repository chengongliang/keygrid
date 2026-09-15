package model

import "time"

// Invitation 邀请码（注册策略=invite 时使用）：admin 生成，注册时消费。
type Invitation struct {
	ID        int64      `gorm:"primaryKey" json:"id"`
	Code      string     `gorm:"uniqueIndex;not null" json:"code"`
	CreatedBy int64      `json:"created_by"`
	UsedBy    *int64     `json:"used_by"` // nil = 未使用
	UsedAt    *time.Time `json:"used_at"`
	ExpiresAt time.Time  `json:"expires_at"`
	CreatedAt time.Time  `json:"created_at"`
}

func (Invitation) TableName() string { return "invitations" }
