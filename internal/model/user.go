package model

import "time"

type User struct {
	ID           int64  `gorm:"primaryKey" json:"id"`
	Email        string `gorm:"uniqueIndex;not null" json:"email"`
	Name         string `json:"name"`
	PasswordHash string `json:"-"`
	Role         string `gorm:"default:user" json:"role"` // user | admin
	Status       string `gorm:"default:active" json:"status"`
	// OIDC subject（IdP 全局唯一标识）；NULL = 纯本地账号。
	// 必须可空 + 唯一：PG 唯一索引只豁免 NULL 不豁免空串，
	// 若存空串则第二个本地用户插入必撞 idx_users_oidc_sub。
	OidcSub *string `gorm:"index:idx_users_oidc_sub,unique" json:"-"`
	// 最近一次登录时间（本地或 SSO）
	LastLoginAt *time.Time `json:"last_login_at,omitempty"`

	CreatedAt time.Time `json:"created_at"`
}

func (User) TableName() string { return "users" }
