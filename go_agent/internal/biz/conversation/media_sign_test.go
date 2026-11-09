package conversation

import (
	"testing"

	"go_agent/internal/pkg/mediaauth"
)

// 附件地址在读取时现签：存储里是裸地址，返回给前端的带签名。
// 这样换密钥 / 重启（随机密钥）不会让历史消息里的附件永久 403。
func TestSignAttachments(t *testing.T) {
	mediaauth.Configure("test-secret")
	defer mediaauth.Configure("")

	msgs := []Message{
		{Attachments: []Attachment{
			{Name: "a.png", URL: "/api/files/202611/a.png", MIME: "image/png"},
			{Name: "b.mp4", URL: "/api/files/202611/b.mp4", MIME: "video/mp4"},
		}},
		{Content: "没有附件的消息"},
	}
	signAttachments(msgs)
	for _, a := range msgs[0].Attachments {
		if !mediaauth.VerifyURL(a.URL) {
			t.Fatalf("附件地址应带有效签名，实际 %q", a.URL)
		}
		if mediaauth.PathOnly(a.URL) != a.URL[:len("/api/files/202611/")+len(a.Name)] {
			t.Fatalf("路径部分不应变化，实际 %q", a.URL)
		}
	}
	// 幂等：重复签名不会叠加参数，也不会失效
	signAttachments(msgs)
	if !mediaauth.VerifyURL(msgs[0].Attachments[0].URL) {
		t.Fatalf("重复签名后应仍可校验，实际 %q", msgs[0].Attachments[0].URL)
	}
}

// mediaDiskRel 要能处理带签名的地址（图片回传给模型时反查磁盘路径）
func TestMediaDiskRelStripsSignature(t *testing.T) {
	mediaauth.Configure("test-secret")
	defer mediaauth.Configure("")
	url := mediaauth.Sign("/api/files/202611/a.png")
	rel, err := mediaDiskRel(url)
	if err != nil {
		t.Fatalf("带签名的地址应可解析: %v", err)
	}
	if rel != "202611/a.png" {
		t.Fatalf("rel=%q", rel)
	}
	if _, err := mediaDiskRel("/api/files/../etc/passwd"); err == nil {
		t.Fatal("路径穿越应被拒绝")
	}
}
