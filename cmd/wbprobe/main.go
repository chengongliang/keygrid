// wbprobe 一次性诊断：用渠道已落库凭据向真实上游发最小 chat 请求，
// 对比「带 Claude Code 指纹」与「脱敏后」两种请求体的上游返回码。
// 只打印状态码与上游 code/msg，不打印 token/正文。用完即删。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/chengongliang/keygrid/internal/conf"
	"github.com/chengongliang/keygrid/internal/crypto"
	"github.com/chengongliang/keygrid/internal/db"
	"github.com/chengongliang/keygrid/internal/model"
	"github.com/chengongliang/keygrid/internal/oauth"
	"github.com/chengongliang/keygrid/internal/op"
	"github.com/chengongliang/keygrid/internal/workbuddy"
)

// claudeCodeSystemPrompt 取自 Claude Code 请求里的指纹片段（用户实测触发 11128）。
const claudeCodeSystemPrompt = "You are Claude Code, Anthropic's official CLI for Claude.\n" +
	"Main branch (you will usually use this for PRs)\n" +
	"cc_entrypoint=cli; cc_version=2.0.1;\n" +
	"x-anthropic-billing-header: acct=abc;\n"

func call(p *model.Provider, tok *oauth.TokenSet, body []byte) (int, string) {
	hdr, err := workbuddy.Headers(p, tok, workbuddy.HeaderOptFor())
	if err != nil {
		return 0, "build headers: " + err.Error()
	}
	hdr.Set("Authorization", "Bearer "+tok.AccessToken)
	target, err := workbuddy.Endpoint(p)
	if err != nil {
		return 0, err.Error()
	}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, target, strings.NewReader(string(body)))
	req.Header = hdr
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<18))
	var obj map[string]any
	_ = json.Unmarshal(raw, &obj)
	code := ""
	if v, ok := obj["code"].(float64); ok {
		code = fmt.Sprintf("code=%d", int(v))
	}
	msg, _ := obj["msg"].(string)
	return resp.StatusCode, code + " msg=" + msg
}

func main() {
	cfg := conf.Load()
	crypto.InitFromSecret(cfg.MasterKey)
	gormDB, err := db.Open(&cfg.Database)
	if err != nil {
		fmt.Println("db:", err)
		os.Exit(1)
	}
	o := &op.Op{DB: gormDB}
	var p model.Provider
	if err := o.DB.Where("oauth_provider = ?", "workbuddy-intl").First(&p).Error; err != nil {
		fmt.Println("provider:", err)
		os.Exit(1)
	}
	cred, err := o.GetCredentialByProviderID(p.ID)
	if err != nil {
		fmt.Println("credential:", err)
		os.Exit(1)
	}
	plain, err := crypto.Decrypt(cred.EncData)
	if err != nil {
		fmt.Println("decrypt:", err)
		os.Exit(1)
	}
	var d oauth.TokenSetData
	_ = json.Unmarshal(plain, &d)
	tok := &oauth.TokenSet{AccessToken: d.AccessToken, RefreshToken: d.RefreshToken, Extra: d.Extra}

	raw, _ := json.Marshal(map[string]any{
		"model":      "deepseek-v4.1-flash",
		"max_tokens": 16,
		"stream":     true,
		"messages": []map[string]string{
			{"role": "system", "content": claudeCodeSystemPrompt},
			{"role": "user", "content": "hi"},
		},
	})
	// 走我们的归一化（含脱敏前）：NormalizeRequest 只归一化，不改指纹内容
	norm, _, _, err := workbuddy.AdaptRequest(&p, raw, "deepseek-v4.1-flash", false)
	if err != nil {
		fmt.Println("adapt:", err)
		return
	}
	status, info := call(&p, tok, norm)
	fmt.Printf("① 未脱敏（Claude Code 指纹原样）: HTTP %d %s\n", status, info)

	scrubbed, rep, err := workbuddy.ScrubRequest(workbuddy.ScrubOn, norm)
	if err != nil {
		fmt.Println("scrub:", err)
		return
	}
	fmt.Printf("   脱敏命中: messages=%d rules=%v\n", rep.Messages, rep.Hits)
	status2, info2 := call(&p, tok, scrubbed)
	fmt.Printf("② 脱敏后: HTTP %d %s\n", status2, info2)
}
