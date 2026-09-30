package workbuddy

import (
	"errors"
	"testing"

	"github.com/chengongliang/keygrid/internal/model"
)

// scrub_mode_test.go ScrubModeFor 的档位来源优先级（设计 §3.4）。
//
// 契约：用户 settings.scrub_mode（非空）优先；空/未设置跟随 WORKBUDDY_SCRUB；
// 读设置失败回落环境默认；未知值一律 off；nil runtime / nil provider 不 panic。
// DB 能力复用 state_test.go 的 fakeDB 与 newTestRuntime（未导出接缝，无需真实 DB）。

func TestRuntimeScrubModeForPrecedence(t *testing.T) {
	prov := &model.Provider{ID: 7, UserID: 3}
	cases := []struct {
		name       string
		env        string
		settings   *model.WorkbuddySetting
		settingErr error
		want       string
	}{
		{"用户设置优先于环境", "off", &model.WorkbuddySetting{ScrubMode: "on"}, nil, ScrubOn},
		{"用户 shadow 覆盖环境 on", "on", &model.WorkbuddySetting{ScrubMode: "shadow"}, nil, ScrubShadow},
		{"空设置回落环境", "shadow", &model.WorkbuddySetting{ScrubMode: ""}, nil, ScrubShadow},
		{"空白设置回落环境", "on", &model.WorkbuddySetting{ScrubMode: "  "}, nil, ScrubOn},
		{"设置行缺失回落环境", "shadow", nil, nil, ScrubShadow},
		{"未知设置值归一为 off", "shadow", &model.WorkbuddySetting{ScrubMode: "garbage"}, nil, ScrubOff},
		{"设置大小写与空格被归一", "off", &model.WorkbuddySetting{ScrubMode: " ON "}, nil, ScrubOn},
		{"DB 错误回落环境默认", "shadow", nil, errors.New("db down"), ScrubShadow},
		{"DB 错误且环境未知回落 off", "garbage", nil, errors.New("db down"), ScrubOff},
		{"环境未设置且设置空 => off", "", &model.WorkbuddySetting{ScrubMode: ""}, nil, ScrubOff},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("WORKBUDDY_SCRUB", tc.env)
			db := &fakeDB{settings: tc.settings, settingErr: tc.settingErr}
			rt := newTestRuntime(db, nil, stateTestNow)
			if got := rt.ScrubModeFor(prov); got != tc.want {
				t.Fatalf("ScrubModeFor = %q, want %q (env=%q setting=%+v)", got, tc.want, tc.env, tc.settings)
			}
		})
	}
}

func TestRuntimeScrubModeForNilSafe(t *testing.T) {
	// nil runtime：不能 panic，返回 off。
	var nilRT *Runtime
	if got := nilRT.ScrubModeFor(&model.Provider{ID: 1, UserID: 1}); got != ScrubOff {
		t.Fatalf("nil runtime = %q, want off", got)
	}
	// 零值 runtime（Op 为 nil）：同样 off。
	if got := (&Runtime{}).ScrubModeFor(&model.Provider{ID: 1, UserID: 1}); got != ScrubOff {
		t.Fatalf("zero runtime = %q, want off", got)
	}
	// nil provider：即便已接线也返回 off（不读设置）。
	rt := newTestRuntime(&fakeDB{settings: &model.WorkbuddySetting{ScrubMode: "on"}}, nil, stateTestNow)
	if got := rt.ScrubModeFor(nil); got != ScrubOff {
		t.Fatalf("nil provider = %q, want off", got)
	}
}
