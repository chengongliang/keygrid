package handlers

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/chengongliang/keygrid/internal/model"
	"github.com/chengongliang/keygrid/internal/op"
	"github.com/chengongliang/keygrid/internal/server/middleware"
	"github.com/chengongliang/keygrid/internal/server/resp"

	"github.com/go-chi/chi/v5"
	"golang.org/x/crypto/bcrypt"
)

// AdminUsersHandler 用户管理：列表/搜索/改角色/禁用启用/重置密码。
type AdminUsersHandler struct {
	Op *op.Op
}

// GET /api/admin/users?search=&page=&size=
func (h *AdminUsersHandler) List(w http.ResponseWriter, r *http.Request) {
	f := op.AdminUserFilter{
		Search: strings.TrimSpace(r.URL.Query().Get("search")),
	}
	if v, err := strconv.Atoi(r.URL.Query().Get("page")); err == nil {
		f.Page = v
	}
	if v, err := strconv.Atoi(r.URL.Query().Get("size")); err == nil {
		f.Size = v
	}
	rows, total, err := h.Op.AdminListUsers(f)
	if err != nil {
		resp.Internal(w, "list users failed")
		return
	}
	resp.Success(w, map[string]any{"items": rows, "total": total})
}

// PATCH /api/admin/users/{id}  {role?, status?}
func (h *AdminUsersHandler) Update(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		resp.BadRequest(w, "invalid user id")
		return
	}
	var req struct {
		Role   *string `json:"role"`
		Status *string `json:"status"`
	}
	if err := resp.Decode(r, &req); err != nil {
		resp.BadRequest(w, "invalid json body")
		return
	}

	adminID := middleware.UserID(r.Context())
	if adminID == id {
		resp.BadRequest(w, "cannot modify your own role/status")
		return
	}

	if req.Role != nil {
		role := strings.ToLower(strings.TrimSpace(*req.Role))
		if role != "user" && role != "admin" {
			resp.BadRequest(w, "role must be user or admin")
			return
		}
		// 防止降级最后一个 admin
		if role == "user" {
			target, err := h.Op.GetUser(id)
			if err != nil {
				resp.NotFound(w, "user not found")
				return
			}
			if target.Role == "admin" {
				n, err := h.Op.CountAdmins()
				if err == nil && n <= 1 {
					resp.BadRequest(w, "cannot demote the last admin")
					return
				}
			}
		}
		if _, err := h.Op.AdminUpdateUser(id, func(u *model.User) { u.Role = role }); err != nil {
			resp.NotFound(w, "user not found")
			return
		}
		middleware.Audit(h.Op, adminID, middleware.AuditEventAdminRoleChange,
			fmt.Sprintf("user %d role -> %s", id, role), middleware.ClientIP(r), r.UserAgent())
	}

	if req.Status != nil {
		status := strings.ToLower(strings.TrimSpace(*req.Status))
		switch status {
		case "disabled":
			// 不允许禁用自己（会把整个平台锁在外面）
			if err := h.Op.AdminDisableUser(id); err != nil {
				resp.NotFound(w, "user not found")
				return
			}
			middleware.Audit(h.Op, adminID, middleware.AuditEventAdminUserDisable,
				fmt.Sprintf("user %d disabled (api keys revoked)", id), middleware.ClientIP(r), r.UserAgent())
		case "active":
			if err := h.Op.AdminEnableUser(id); err != nil {
				resp.NotFound(w, "user not found")
				return
			}
			middleware.Audit(h.Op, adminID, middleware.AuditEventAdminUserEnable,
				fmt.Sprintf("user %d enabled", id), middleware.ClientIP(r), r.UserAgent())
		default:
			resp.BadRequest(w, "status must be active or disabled")
			return
		}
	}

	if req.Role == nil && req.Status == nil {
		resp.BadRequest(w, "nothing to update (role/status)")
		return
	}
	u, err := h.Op.GetUser(id)
	if err != nil {
		resp.NotFound(w, "user not found")
		return
	}
	resp.Success(w, map[string]any{"id": u.ID, "email": u.Email, "role": u.Role, "status": u.Status})
}

// POST /api/admin/users/{id}/reset_password  → 返回一次性新密码
func (h *AdminUsersHandler) ResetPassword(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		resp.BadRequest(w, "invalid user id")
		return
	}
	newPass, err := genPassword()
	if err != nil {
		resp.Internal(w, "generate password failed")
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(newPass), bcrypt.DefaultCost)
	if err != nil {
		resp.Internal(w, "hash password failed")
		return
	}
	if err := h.Op.AdminSetPassword(id, string(hash)); err != nil {
		resp.NotFound(w, "user not found")
		return
	}
	middleware.Audit(h.Op, middleware.UserID(r.Context()), middleware.AuditEventAdminResetPass,
		fmt.Sprintf("password reset for user %d", id), middleware.ClientIP(r), r.UserAgent())
	resp.Success(w, map[string]any{"password": newPass})
}
