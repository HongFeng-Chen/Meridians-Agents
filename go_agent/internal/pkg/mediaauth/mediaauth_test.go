package mediaauth

import (
	"net/url"
	"strings"
	"testing"
)

func TestSignVerify(t *testing.T) {
	Configure("test-secret")
	defer Configure("")
	if !Enabled() {
		t.Fatal("配置密钥后应启用签名")
	}

	signed := Sign("/api/files/202611/a.png")
	if !strings.HasPrefix(signed, "/api/files/202611/a.png?sig=") {
		t.Fatalf("应追加签名参数，实际 %q", signed)
	}
	if !VerifyURL(signed) {
		t.Fatal("自己签的地址应校验通过")
	}
	if VerifyURL("/api/files/202611/a.png") {
		t.Fatal("没有签名的地址不应通过")
	}
	// 换路径：签名不匹配（挡住「拿自己的签名去读别人的文件」）
	tampered := strings.Replace(signed, "a.png", "b.png", 1)
	if VerifyURL(tampered) {
		t.Fatal("改路径后签名应失效")
	}
	// 换签名：不匹配
	if VerifyURL("/api/files/202611/a.png?sig=deadbeef") {
		t.Fatal("伪造签名不应通过")
	}
	// 幂等：重复签名不会叠加参数
	again := Sign(signed)
	if strings.Count(again, "sig=") != 1 {
		t.Fatalf("重复签名应替换而不是叠加，实际 %q", again)
	}
	if !VerifyURL(again) {
		t.Fatalf("重复签名后仍应可校验，实际 %q", again)
	}
	// 其他查询参数保留
	withQ := Sign("/api/files/202611/a.png?v=2")
	if !strings.Contains(withQ, "v=2") || !VerifyURL(withQ) {
		t.Fatalf("其他参数应保留且可校验，实际 %q", withQ)
	}
	if got := PathOnly(withQ); got != "/api/files/202611/a.png" {
		t.Fatalf("PathOnly 应去掉查询串，实际 %q", got)
	}
}

func TestSignDisabled(t *testing.T) {
	Configure("")
	if Enabled() {
		t.Fatal("未配置密钥不应启用签名")
	}
	if got := Sign("/api/files/x/a.png"); got != "/api/files/x/a.png" {
		t.Fatalf("未启用时应原样返回，实际 %q", got)
	}
	if !Verify("/api/files/x/a.png", "") {
		t.Fatal("未启用时应放行")
	}
}

// 不同密钥签出来的结果必须不同（否则换密钥等于没换）
func TestSignDependsOnSecret(t *testing.T) {
	Configure("secret-a")
	a := Sign("/api/files/x/a.png")
	Configure("secret-b")
	b := Sign("/api/files/x/a.png")
	if a == b {
		t.Fatalf("不同密钥应得到不同签名，实际都是 %q", a)
	}
	if VerifyURL(a) {
		t.Fatal("换密钥后旧签名应失效")
	}
	q, _ := url.ParseQuery(strings.SplitN(b, "?", 2)[1])
	if len(q.Get(param)) != 32 {
		t.Fatalf("签名应为 32 位 hex，实际 %q", q.Get(param))
	}
}
