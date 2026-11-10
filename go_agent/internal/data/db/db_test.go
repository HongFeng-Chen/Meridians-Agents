package db

import (
	"errors"
	"strings"
	"testing"
)

// TestMySQLHint 诊断提示必须是「能照着做」的一句话：
// 常见四类（密码错、库不存在、连不上、握手中断）都要有，且不误伤无关错误。
func TestMySQLHint(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{errors.New("Error 1045: Access denied for user 'root'@'localhost'"), "账号或密码错误"},
		{errors.New("Error 1049: Unknown database 'go_agent'"), "库不存在"},
		{errors.New("dial tcp 127.0.0.1:3306: connect: connection refused"), "MySQL 未启动"},
		{errors.New("invalid connection"), "端口映射失效"},
		{errors.New("unexpected EOF"), "端口映射失效"},
		{errors.New("i/o timeout"), "MySQL 响应超时"},
		{errors.New("something else entirely"), ""},
	}
	for _, c := range cases {
		got := mysqlHint(c.err)
		if c.want == "" {
			if got != "" {
				t.Fatalf("%q 不该有提示，实际 %q", c.err, got)
			}
			continue
		}
		if !strings.Contains(got, c.want) {
			t.Fatalf("%q 的提示应包含 %q，实际 %q", c.err, c.want, got)
		}
	}
}
