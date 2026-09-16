package handlers

import (
	"testing"

	"codeactivityhub/backend/internal/models"
)

// 钉住平台状态的读时自愈规则：
//  1. 未配置平台的 error/warning → unconfigured（不该告警）
//  2. 网络类 error（超时/断连）→ warning（红条只认 error）
//  3. 凭证类 error（如洛谷 401）必须保留，不能被误吞
func TestNormalizeStatuses(t *testing.T) {
	rows := []models.PlatformStatus{
		{Platform: "codeforces", Status: "error",
			Message: `Get "https://codeforces.com/api/user.info?handles=x": context deadline exceeded (Client.Timeout exceeded while awaiting headers)`},
		{Platform: "luogu", Status: "error", Message: "洛谷返回 401 请先登录"},
		{Platform: "leetcode", Status: "error", Message: "LeetCode 用户不存在：请填个人主页 URL 里 /u/ 后面那段用户名"},
		{Platform: "atcoder", Status: "ok", Message: "从 AtCoder 获取 35 条提交"},
	}
	configured := map[string]bool{"codeforces": true, "luogu": true, "atcoder": true}
	out := normalizeStatuses(rows, configured)

	if out[0].Status != "warning" {
		t.Errorf("网络类 error 应自愈为 warning，得到 %q（message=%q）", out[0].Status, out[0].Message)
	}
	if out[1].Status != "error" {
		t.Errorf("凭证类 error 不该被吞掉，得到 %q", out[1].Status)
	}
	if out[2].Status != "unconfigured" {
		t.Errorf("未配置平台的 error 应归一化为 unconfigured，得到 %q", out[2].Status)
	}
	if out[3].Status != "ok" {
		t.Errorf("正常状态不应被改动，得到 %q", out[3].Status)
	}
}
