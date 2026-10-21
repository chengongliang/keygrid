package qoder

import (
	"net/http"
	"strings"
	"testing"

	"github.com/chengongliang/keygrid/internal/model"
)

// encoding_test.go 对照 9router tests/unit/qoder.test.js 的向量：
// 期望值由 9router 的 qoderEncodeBody 直接产出。

func TestEncodeBodyVectors(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"abc", "MSK#"},
		{"hello", "q$FruHPH"},
		{"hello world", "YuHp$Hq&J(WPHFru"},
		{`{"model":"auto","messages":[{"role":"user","content":"你好"}]}`,
			"Mu(QwDSQwBMn*QG(&QPSWBE.Jep$$SJLNYBySkr*NOWrD,ByBEF^DxB*GoKmYKtu(CLuoByBOg.JHq*GoKtDxj^#"},
	}
	for _, c := range cases {
		if got := string(EncodeBody([]byte(c.in))); got != c.want {
			t.Errorf("EncodeBody(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestEncodeBodyPreservesLength(t *testing.T) {
	for _, in := range []string{"abcdef", "hello", "a", "0123456789abcdef"} {
		got := EncodeBody([]byte(in))
		// 输出长度 = 标准 base64 长度
		want := (len(in) + 2) / 3 * 4
		if len(got) != want {
			t.Errorf("EncodeBody(%q) len = %d, want %d", in, len(got), want)
		}
	}
}

func TestEncodeBodyOnlyCustomAlphabet(t *testing.T) {
	allowed := map[byte]bool{}
	for i := 0; i < len(customAlphabet); i++ {
		allowed[customAlphabet[i]] = true
	}
	allowed['$'] = true
	got := EncodeBody([]byte("hello world this is a longer string for testing 0123456789"))
	for _, c := range got {
		if !allowed[c] {
			t.Fatalf("unexpected byte in encoded output: %q", c)
		}
	}
}

func TestEncodeBodyDeterministic(t *testing.T) {
	a := string(EncodeBody([]byte("abc")))
	b := string(EncodeBody([]byte("abc")))
	if a != b {
		t.Fatalf("encoding not deterministic: %q vs %q", a, b)
	}
	if string(EncodeBody([]byte("abc"))) == string(EncodeBody([]byte("xyz"))) {
		t.Fatal("different inputs produced identical output")
	}
}

// ---- cosy 签名 ----

func TestBuildHeadersShape(t *testing.T) {
	creds := Creds{
		UserID:    "test-user-id",
		Token:     "dt-test-token",
		Name:      "Test",
		Email:     "test@example.com",
		MachineID: "fixed-machine-id",
	}
	h, err := BuildHeaders(nil, ModelListURL(RegionIntl, creds.Token), creds)
	if err != nil {
		t.Fatalf("BuildHeaders: %v", err)
	}
	required := []string{
		"Authorization", "Cosy-Key", "Cosy-User", "Cosy-Date", "Cosy-Version",
		"Cosy-Machineid", "Cosy-Machinetoken", "Cosy-Machinetype", "Cosy-Machineos",
		"Cosy-Clienttype", "Cosy-Clientip", "Cosy-Bodyhash", "Cosy-Bodylength",
		"Cosy-Sigpath", "Cosy-Data-Policy", "Login-Version", "X-Request-Id",
	}
	for _, k := range required {
		if v := h.Get(k); v == "" {
			t.Errorf("missing header %s", k)
		}
	}
	auth := h.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer COSY.") || strings.Count(auth, ".") != 2 {
		t.Errorf("Authorization = %q, want Bearer COSY.<payload>.<sig>", auth)
	}
	if got := h.Get("Cosy-Sigpath"); got != "/api/v2/model/list" {
		t.Errorf("Cosy-Sigpath = %q", got)
	}
	if got := h.Get("Cosy-Bodyhash"); got != "d41d8cd98f00b204e9800998ecf8427e" {
		t.Errorf("empty body hash = %q", got)
	}
	if got := h.Get("Cosy-Bodylength"); got != "0" {
		t.Errorf("empty body length = %q", got)
	}
	if got := h.Get("Cosy-Machineid"); got != "fixed-machine-id" {
		t.Errorf("machine id = %q", got)
	}
	if got := h.Get("Cosy-User"); got != "test-user-id" {
		t.Errorf("cosy user = %q", got)
	}
}

func TestBuildHeadersChatSigPath(t *testing.T) {
	creds := Creds{UserID: "u", Token: "dt-x", MachineID: "m"}
	h, err := BuildHeaders([]byte("body"), ChatURL(RegionIntl, creds.Token), creds)
	if err != nil {
		t.Fatalf("BuildHeaders: %v", err)
	}
	if got := h.Get("Cosy-Sigpath"); got != ChatSigPath {
		t.Errorf("Cosy-Sigpath = %q, want %q", got, ChatSigPath)
	}
	if got := h.Get("Cosy-Bodyhash"); got != md5Hex([]byte("body")) {
		t.Errorf("body hash mismatch: %q", got)
	}
	if got := h.Get("Cosy-Bodylength"); got != "4" {
		t.Errorf("body length = %q", got)
	}
}

func TestBuildHeadersRequiresCreds(t *testing.T) {
	if _, err := BuildHeaders(nil, "https://api3.qoder.sh/algo/x", Creds{Token: "t"}); err == nil {
		t.Fatal("expected error for missing userId")
	}
	if _, err := BuildHeaders(nil, "https://api3.qoder.sh/algo/x", Creds{UserID: "u"}); err == nil {
		t.Fatal("expected error for missing token")
	}
}

func TestBuildHeadersRandomPerCall(t *testing.T) {
	creds := Creds{UserID: "u", Token: "dt-x", MachineID: "fixed"}
	a, _ := BuildHeaders([]byte("p"), ChatURL(RegionIntl, creds.Token), creds)
	b, _ := BuildHeaders([]byte("p"), ChatURL(RegionIntl, creds.Token), creds)
	// 每次请求新鲜 AES key → 签名与 Cosy-Key 必然不同；稳定项必须一致。
	if a.Get("X-Request-Id") == b.Get("X-Request-Id") {
		t.Error("X-Request-Id should differ per call")
	}
	if a.Get("Authorization") == b.Get("Authorization") {
		t.Error("signature should differ per call (fresh AES key)")
	}
	for _, k := range []string{"Cosy-User", "Cosy-Bodyhash", "Cosy-Bodylength", "Cosy-Sigpath", "Cosy-Machineid"} {
		if a.Get(k) != b.Get(k) {
			t.Errorf("%s should be stable across calls: %q != %q", k, a.Get(k), b.Get(k))
		}
	}
}

// ---- 区域与端点 ----

func TestIsQoder(t *testing.T) {
	cases := []struct {
		p    *model.Provider
		want bool
	}{
		{&model.Provider{OAuthProvider: "qoder"}, true},
		{&model.Provider{OAuthProvider: "qoder-cn"}, true},
		{&model.Provider{Kind: "api_key", BaseURL: "https://api3.qoder.sh"}, true},
		{&model.Provider{Kind: "api_key", BaseURL: "https://api3.qoder.sh/v1"}, true},
		{&model.Provider{Kind: "api_key", BaseURL: "gateway.qoder.com.cn"}, true},
		{&model.Provider{OAuthProvider: "kimi", BaseURL: "https://api.kimi.com/coding"}, false},
		{&model.Provider{BaseURL: "https://api.openai.com/v1"}, false},
		{nil, false},
	}
	for i, c := range cases {
		if got := IsQoder(c.p); got != c.want {
			t.Errorf("case %d: IsQoder = %v, want %v", i, got, c.want)
		}
	}
}

func TestRegionOf(t *testing.T) {
	if got := RegionOf(&model.Provider{OAuthProvider: "qoder-cn"}); got != RegionCN {
		t.Errorf("qoder-cn region = %q", got)
	}
	if got := RegionOf(&model.Provider{OAuthProvider: "qoder"}); got != RegionIntl {
		t.Errorf("qoder region = %q", got)
	}
	if got := RegionOf(&model.Provider{BaseURL: "https://gateway.qoder.com.cn"}); got != RegionCN {
		t.Errorf("cn base_url region = %q", got)
	}
	if got := RegionOf(&model.Provider{BaseURL: "https://api3.qoder.sh"}); got != RegionIntl {
		t.Errorf("intl base_url region = %q", got)
	}
}

func TestInferenceBaseRoutesJobTokensToAPI2(t *testing.T) {
	// 国际站：dt- 走 api3，jt- 走 api2（api3 对 jt- 回 403 Login expired）。
	if got := InferenceBase(RegionIntl, "dt-abc"); got != "https://api3.qoder.sh" {
		t.Errorf("dt- base = %q", got)
	}
	if got := InferenceBase(RegionIntl, "jt-abc"); got != "https://api2.qoder.sh" {
		t.Errorf("jt- base = %q", got)
	}
	// 国内站：统一走网关。
	if got := InferenceBase(RegionCN, "jt-abc"); got != "https://gateway.qoder.com.cn" {
		t.Errorf("cn jt- base = %q", got)
	}
	if got := InferenceBase(RegionCN, "dt-abc"); got != "https://gateway.qoder.com.cn" {
		t.Errorf("cn dt- base = %q", got)
	}
}

func TestChatURL(t *testing.T) {
	u := ChatURL(RegionIntl, "dt-abc")
	want := "https://api3.qoder.sh/algo" + ChatSigPath + "?FetchKeys=llm_model_result&AgentId=agent_common&Encode=1"
	if u != want {
		t.Errorf("ChatURL = %q, want %q", u, want)
	}
}

func TestPATDetection(t *testing.T) {
	if !IsPAT("pt-abc") {
		t.Error("pt- should be PAT")
	}
	if IsPAT("dt-abc") || IsPAT("jt-abc") {
		t.Error("dt-/jt- are not PAT")
	}
}

var _ = http.MethodGet
