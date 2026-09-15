package model

import "time"

// Credential 加密后的凭据，与 provider 1:1。
type Credential struct {
	ID            int64      `gorm:"primaryKey"`
	ProviderID    int64      `gorm:"uniqueIndex;not null"`
	EncData       []byte     `gorm:"not null"`       // AES-GCM(json)
	Status        string     `gorm:"default:active"` // active | refreshing | revoked | expired
	ExpiresAt     *time.Time // oauth access_token 过期时间
	LastRefreshAt *time.Time
	LastError     string
	CreatedAt     time.Time
}

func (Credential) TableName() string { return "credentials" }
