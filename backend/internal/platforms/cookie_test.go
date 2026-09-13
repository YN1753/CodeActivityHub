package platforms

import "testing"

// 洛谷的 Cookie 有三个常见粘贴形态，之前的实现一律加 "__client_id=" 前缀，
// 用户粘贴 "__client_id=xxx" 时发出去会变成 "__client_id=__client_id=xxx"，
// 洛谷直接当成未登录（401），表现为"验证总说失效"。这里把三种形态钉死。
func TestLuoguCookieHeader(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"纯值", "eyJhbGciOiJIUzI1NiJ9.abc.def", "__client_id=eyJhbGciOiJIUzI1NiJ9.abc.def"},
		{"带名字", "__client_id=eyJhbGciOiJIUzI1NiJ9.abc.def", "__client_id=eyJhbGciOiJIUzI1NiJ9.abc.def"},
		{"整段 cookie", "__client_id=eyJ.abc; _uid=1436039", "__client_id=eyJ.abc; _uid=1436039"},
		{"顺序颠倒", "_uid=1436039; __client_id=eyJ.abc", "_uid=1436039; __client_id=eyJ.abc"},
		{"带引号", `"__client_id=eyJ.abc"`, "__client_id=eyJ.abc"},
		{"带换行与空格", "  __client_id=eyJ.abc  \n", "__client_id=eyJ.abc"},
		{"空值", "   ", ""},
		{"值里带等号（base64 填充）", "eyJ.abc==", "__client_id=eyJ.abc=="},
	}
	for _, tc := range cases {
		if got := luoguCookieHeader(tc.input); got != tc.want {
			t.Errorf("%s: luoguCookieHeader(%q) = %q, 期望 %q", tc.name, tc.input, got, tc.want)
		}
	}
}

func TestLuoguCookieSummary(t *testing.T) {
	// 诊断信息只能出现 cookie 的名字和长度，绝不能带出值本身
	got := luoguCookieSummary("__client_id=supersecretvalue; _uid=1436039")
	want := "__client_id(16), _uid(7)"
	if got != want {
		t.Errorf("luoguCookieSummary = %q, 期望 %q", got, want)
	}
	if luoguCookieSummary("__client_id=abc") != "__client_id(3)" {
		t.Error("单个 cookie 的摘要不正确")
	}
	if luoguCookieSummary("") != "（空）" {
		t.Error("空 cookie 摘要不正确")
	}
}

func TestLuoguVerdict(t *testing.T) {
	cases := map[int]string{
		0: "PENDING", 1: "PENDING", 2: "CE", 3: "OLE", 4: "MLE",
		5: "TLE", 6: "WA", 7: "RE", 11: "UKE", 12: "AC", 14: "UNACCEPTED",
		// Hack 相关与隐藏结果不是真实提交，认不出来的码同样必须返回空串，
		// 由调用方跳过——否则会被错题本规则（非 AC 即错题）收进去。
		21: "", 22: "", 23: "", -1: "", 99: "",
	}
	for status, want := range cases {
		if got := luoguVerdict(status); got != want {
			t.Errorf("luoguVerdict(%d) = %q, 期望 %q", status, got, want)
		}
	}
}
