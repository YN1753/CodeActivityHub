package platforms

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// 洛谷有反爬风控，请求过密会被拒绝甚至封 IP。这里用本地假服务器验证
// "同一域名的两次请求之间有硬性间隔"，不产生任何真实外部流量。
func TestThrottleSpacesConsecutiveRequests(t *testing.T) {
	c := NewClient()
	c.minInterval = 120 * time.Millisecond

	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	start := time.Now()
	for i := 0; i < 3; i++ {
		var out map[string]any
		if err := c.getJSON(context.Background(), srv.URL, &out); err != nil {
			t.Fatalf("第 %d 次请求失败: %v", i+1, err)
		}
	}
	elapsed := time.Since(start)
	if got := atomic.LoadInt32(&hits); got != 3 {
		t.Fatalf("服务端收到 %d 次请求，期望 3 次", got)
	}
	// 3 次请求之间有 2 个间隔
	if minimum := 2 * c.minInterval; elapsed < minimum {
		t.Errorf("3 次请求只花了 %v，节流没生效（期望至少 %v）", elapsed, minimum)
	}
}

func TestThrottleHonoursContextCancellation(t *testing.T) {
	c := NewClient()
	c.minInterval = time.Hour

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()

	// 第一次直接放行
	if err := c.waitForSlot(ctx, "https://example.com/first"); err != nil {
		t.Fatalf("首次请求不该被阻塞: %v", err)
	}
	// 第二次需要等 1 小时，但 ctx 会先取消——必须立刻返回而不是干等
	start := time.Now()
	if err := c.waitForSlot(ctx, "https://example.com/second"); err == nil {
		t.Error("ctx 取消后应返回错误")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("ctx 取消后仍阻塞了 %v", elapsed)
	}
}

func TestThrottleIsSeparatedPerHost(t *testing.T) {
	c := NewClient()
	c.minInterval = 500 * time.Millisecond

	start := time.Now()
	if err := c.waitForSlot(context.Background(), "https://a.example.com/x"); err != nil {
		t.Fatal(err)
	}
	// 不同域名之间不应互相阻塞
	if err := c.waitForSlot(context.Background(), "https://b.example.com/x"); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Errorf("不同域名之间被错误地串行化了，用了 %v", elapsed)
	}
}

func TestDefaultThrottleIsConservative(t *testing.T) {
	if got := NewClient().minInterval; got < 500*time.Millisecond {
		t.Errorf("默认出站间隔 %v 太短，容易触发站点风控", got)
	}
}

func TestConfiguredMinInterval(t *testing.T) {
	cases := []struct {
		env  string
		want time.Duration
	}{
		{"", defaultMinInterval},
		{"1500", 1500 * time.Millisecond},
		{"0", 0},
		{"abc", defaultMinInterval},
		{"-5", defaultMinInterval},
	}
	for _, tc := range cases {
		t.Setenv("CODEACTIVITYHUB_MIN_INTERVAL_MS", tc.env)
		if got := configuredMinInterval(); got != tc.want {
			t.Errorf("env=%q 时得到 %v，期望 %v", tc.env, got, tc.want)
		}
	}
}
