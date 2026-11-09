#!/usr/bin/env python
# -*- coding: utf-8 -*-
"""从 configs/config.yaml 派生一个「开启认证 + 双用户静态令牌」的验证用配置。

产物：configs-isolation/config.yaml（已被 .gitignore 忽略，含明文测试令牌，不入库）
用途：python scripts/make_isolation_config.py && ./bin/go_agent.exe -conf ./configs-isolation
"""
import os
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
SRC = os.path.join(ROOT, "configs", "config.yaml")
DST_DIR = os.path.join(ROOT, "configs-isolation")
DST = os.path.join(DST_DIR, "config.yaml")

HTTP_PORT = os.environ.get("ISO_HTTP_PORT", "8011")
GRPC_PORT = os.environ.get("ISO_GRPC_PORT", "9011")

users = {
    os.environ.get("ISO_TOKEN_ALICE", "tok-alice"): os.environ.get("ISO_USER_ALICE", "alice:t1"),
    os.environ.get("ISO_TOKEN_BOB", "tok-bob"): os.environ.get("ISO_USER_BOB", "bob:t1"),
}


def set_addr(text, env_name, default_addr, port):
    """把 server 段的 addr 占位符改成本机回环 + 指定端口。

    取代早期的字面量替换：config.yaml 现在写的是 ${HTTP_ADDR:127.0.0.1:8000}，
    写死 "addr: 0.0.0.0:8000" 会静默匹配不到，隔离环境就退回默认端口，
    与本机已在跑的服务撞端口。
    """
    key = "${%s:%s}" % (env_name, default_addr)
    if key not in text:
        raise SystemExit("生成失败：configs/config.yaml 里没有找到 %s" % key)
    return text.replace(key, "127.0.0.1:" + port, 1)


def main():
    with open(SRC, encoding="utf-8") as f:
        text = f.read()

    text = set_addr(text, "HTTP_ADDR", "127.0.0.1:8000", HTTP_PORT)
    text = set_addr(text, "GRPC_ADDR", "127.0.0.1:9000", GRPC_PORT)
    # 认证打开：所有非 publicPaths 接口必须带令牌
    text = text.replace("enabled: ${AUTH_ENABLED:false}", "enabled: true", 1)

    lines = "".join('      %s: "%s"\n' % (tok, ident) for tok, ident in users.items())
    text = text.replace("    staticTokens: {}\n", "    staticTokens:\n" + lines, 1)

    if "staticTokens:\n" not in text:
        print("生成失败：configs/config.yaml 里没有找到 'staticTokens: {}' 占位", file=sys.stderr)
        return 1

    os.makedirs(DST_DIR, exist_ok=True)
    with open(DST, "w", encoding="utf-8") as f:
        f.write(text)

    print("已生成 %s" % DST)
    print("  端口: http=%s grpc=%s" % (HTTP_PORT, GRPC_PORT))
    for tok, ident in users.items():
        print("  令牌: %s → %s" % (tok, ident))
    return 0


if __name__ == "__main__":
    sys.exit(main())
