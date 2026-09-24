package handlers

import (
	"net/http"
	"testing"

	"github.com/chengongliang/keygrid/internal/model"

	"golang.org/x/crypto/bcrypt"
)

// 自助改密校验：SSO-only 账号禁用、新密码强度、旧密码验签。
// 落库路径（op.SetPassword）依赖 Postgres 测试基建，由 scripts/e2e_admin.sh 覆盖。
func TestCheckPasswordChange(t *testing.T) {
	hash, err := bcrypt.GenerateFromPassword([]byte("old-pass-123"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name   string
		hash   string
		old    string
		newPwd string
		want   int
	}{
		{"ok", string(hash), "old-pass-123", "new-pass-456", 0},
		{"sso-only forbidden", "", "", "new-pass-456", http.StatusForbidden},
		{"new password too short", string(hash), "old-pass-123", "short", http.StatusBadRequest},
		{"wrong old password", string(hash), "wrong-pass", "new-pass-456", http.StatusBadRequest},
		{"empty old password", string(hash), "", "new-pass-456", http.StatusBadRequest},
	}
	for _, c := range cases {
		status, msg, _ := checkPasswordChange(c.hash, c.old, c.newPwd)
		if status != c.want {
			t.Errorf("%s: status=%d (%s), want %d", c.name, status, msg, c.want)
		}
	}
}

// 会话用户信息必须携带 has_password：前端据此对 SSO-only 账号禁用改密入口。
func TestSessionUserHasPasswordFlag(t *testing.T) {
	if got := sessionUser(&model.User{ID: 1, Email: "sso@example.com"}); got["has_password"] != false {
		t.Errorf("oidc-only user: has_password=%v, want false", got["has_password"])
	}
	if got := sessionUser(&model.User{ID: 2, Email: "local@example.com", PasswordHash: "x"}); got["has_password"] != true {
		t.Errorf("local user: has_password=%v, want true", got["has_password"])
	}
}
