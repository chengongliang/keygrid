package handlers

import (
	"testing"

	"github.com/chengongliang/keygrid/internal/model"
	"github.com/chengongliang/keygrid/internal/oauth"
)

func TestCheckQoderDuplicateAccountSkipsPATAndMissingIdentity(t *testing.T) {
	h := &OAuthHandler{}
	if err := h.checkQoderDuplicateAccount(&model.Provider{Kind: "api_key", OAuthProvider: "qoder"}, &oauth.TokenSet{}); err != nil {
		t.Fatalf("PAT channels must not use OAuth duplicate lookup: %v", err)
	}
	if err := h.checkQoderDuplicateAccount(&model.Provider{Kind: "oauth", OAuthProvider: "qoder-cn"}, &oauth.TokenSet{}); err != nil {
		t.Fatalf("missing identity must skip duplicate lookup: %v", err)
	}
}

func TestFindDuplicateQoderAccount(t *testing.T) {
	accounts := []qoderAccountCredential{
		{ProviderID: 2, Name: "Qoder main", Status: "active", Identity: "email:user@example.com"},
		{ProviderID: 3, Name: "Qoder disabled", Status: "disabled", Identity: "email:disabled@example.com"},
	}
	if duplicate := findDuplicateQoderAccount("email:user@example.com", 1, accounts); duplicate == nil || duplicate.ProviderName != "Qoder main" {
		t.Fatalf("expected duplicate channel, got %+v", duplicate)
	}
	if duplicate := findDuplicateQoderAccount("email:user@example.com", 2, accounts); duplicate != nil {
		t.Fatalf("same channel reauthorization should be allowed, got %+v", duplicate)
	}
	if duplicate := findDuplicateQoderAccount("email:disabled@example.com", 1, accounts); duplicate != nil {
		t.Fatalf("inactive channel should not block authorization, got %+v", duplicate)
	}
	if duplicate := findDuplicateQoderAccount("email:new@example.com", 1, accounts); duplicate != nil {
		t.Fatalf("different account should be allowed, got %+v", duplicate)
	}
}
