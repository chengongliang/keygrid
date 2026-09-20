package db

import (
	"fmt"
	"log"
	"os"
	"time"

	"github.com/chengongliang/keygrid/internal/conf"
	"github.com/chengongliang/keygrid/internal/model"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func Open(cfg *conf.DatabaseConfig) (*gorm.DB, error) {
	dsn := fmt.Sprintf(
		"host=%s port=%s user=%s password=%s dbname=%s sslmode=disable TimeZone=Asia/Shanghai",
		cfg.Host, cfg.Port, cfg.User, cfg.Password, cfg.DBName,
	)
	return gorm.Open(postgres.Open(dsn), &gorm.Config{
		Logger: logger.New(
			log.New(os.Stdout, "\r\n", log.LstdFlags),
			logger.Config{
				SlowThreshold: 200 * time.Millisecond,
				LogLevel:      logger.Warn,
				// record not found 是正常业务路径（鉴权查 key、GetSetting 读未设置项），
				// 不算错误，关掉刷屏；慢查询与真实 SQL 错误仍照常打印。
				IgnoreRecordNotFoundError: true,
			},
		),
	})
}

func Migrate(gormDB *gorm.DB) error {
	// 历史列名修正：早期 GORM 默认 NamingStrategy 把 OAuthProvider 建成了 o_auth_provider，
	// 与手写 SQL（op.ListRefreshDueCredentials 用 oauth_provider）不一致，导致 oauth
	// 刷新扫描持续报 SQLSTATE 42703。必须在 AutoMigrate 之前 rename，否则 AutoMigrate
	// 会先新建空的 oauth_provider 列再撞名。幂等：仅当旧列存在且新列不存在时执行。
	if hasColumn(gormDB, "providers", "o_auth_provider") && !hasColumn(gormDB, "providers", "oauth_provider") {
		if err := gormDB.Exec(`ALTER TABLE providers RENAME COLUMN o_auth_provider TO oauth_provider`).Error; err != nil {
			return fmt.Errorf("rename legacy column providers.o_auth_provider: %w", err)
		}
	}

	if err := gormDB.AutoMigrate(
		&model.User{},
		&model.ApiKey{},
		&model.Provider{},
		&model.Credential{},
		&model.UsageLog{},
		&model.OAuthState{},
		&model.AuditLog{},
		&model.PlatformSetting{},
		&model.Invitation{},
		&model.OidcState{},     // OIDC SSO 一次性 state
		&model.ModelPrice{},    // 计费：admin 全局模型价格表
		&model.QuotaSnapshot{}, // 额度快照（openai codex /wham/usage 同步结果）
		&model.RequestError{},  // 失败请求诊断详情（默认保留 7 天，后台任务清理）
	); err != nil {
		return err
	}

	// 数据修正：oidc_sub 唯一索引只豁免 NULL 不豁免空串，
	// 存量本地账号的空串会让第二个用户插入/修改必撞 idx_users_oidc_sub。
	// 幂等：GORM 会把 *string 零值写成 NULL，但历史行可能已是 ''。
	return gormDB.Exec("UPDATE users SET oidc_sub = NULL WHERE oidc_sub = ''").Error
}

// hasColumn 用 information_schema 判断列是否存在（不依赖 GORM migrator 的 tag 解析，行为更透明）。
func hasColumn(db *gorm.DB, table, column string) bool {
	var n int64
	db.Raw(
		`SELECT count(*) FROM information_schema.columns WHERE table_schema = current_schema() AND table_name = ? AND column_name = ?`,
		table, column,
	).Scan(&n)
	return n > 0
}
