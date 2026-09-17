package handlers

import "testing"

// 渠道白名单写入校验：只接受本人渠道 ID，去重保序；非法/越权一律拒绝。
func TestNormalizeProviderLimitIDs(t *testing.T) {
	owned := map[int64]bool{3: true, 5: true, 7: true}

	cases := []struct {
		name    string
		raw     string
		want    string
		wantErr bool
	}{
		{"empty", "", "", false},
		{"blank", "   ", "", false},
		{"single", "3", "3", false},
		{"multi", "3,5,7", "3,5,7", false},
		{"trim", " 3 , 5 ", "3,5", false},
		{"dedup keeps order", "5,3,5", "5,3", false},
		{"trailing comma", "7,", "7", false},
		{"unknown id rejected", "3,99", "", true},
		{"non numeric rejected", "3,abc", "", true},
		{"zero rejected", "0", "", true},
		{"negative rejected", "-3", "", true},
	}
	for _, c := range cases {
		got, err := normalizeProviderLimitIDs(c.raw, owned)
		if c.wantErr {
			if err == nil {
				t.Errorf("%s: expected error for %q, got %q", c.name, c.raw, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: unexpected error for %q: %v", c.name, c.raw, err)
			continue
		}
		if got != c.want {
			t.Errorf("%s: normalize(%q) = %q, want %q", c.name, c.raw, got, c.want)
		}
	}
}
