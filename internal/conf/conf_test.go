package conf

import "testing"

// TestValidateRejectsWeakDefaults 公开示例值必须被拒绝启动（防“复制 .env.example 后忘改”）。
func TestValidateRejectsWeakDefaults(t *testing.T) {
	cases := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{"内置默认 JWT_SECRET", Config{JWTSecret: "dev-insecure-secret"}, true},
		{"compose 示例 JWT_SECRET", Config{JWTSecret: "dev-insecure-secret-change-me"}, true},
		{"示例 MASTER_KEY", Config{JWTSecret: "s3cret-xyz", MasterKey: "please-change-me-to-a-long-random-secret"}, true},
		{"自定义强随机值", Config{JWTSecret: "e2e-test-jwt-secret-dev-only", MasterKey: "e2e-test-master-key-0f34c9a1"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate()
			if (err != nil) != tc.wantErr {
				t.Fatalf("Validate() err=%v, wantErr=%v", err, tc.wantErr)
			}
		})
	}
}

// TestLoadDefaultsAreRejected 未设置 env 时回退内置默认值，且 Validate 必须拦截。
func TestLoadDefaultsAreRejected(t *testing.T) {
	t.Setenv("JWT_SECRET", "")
	t.Setenv("MASTER_KEY", "")
	cfg := Load()
	if cfg.JWTSecret != "dev-insecure-secret" {
		t.Fatalf("JWTSecret = %q, want built-in default", cfg.JWTSecret)
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate() should reject built-in defaults")
	}
}
