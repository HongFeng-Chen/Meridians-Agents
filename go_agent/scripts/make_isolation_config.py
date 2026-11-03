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


def main():
    with open(SRC, encoding="utf-8") as f:
        text = f.read()

    text = text.replace("addr: 0.0.0.0:8000", "addr: 0.0.0.0:" + HTTP_PORT, 1)
    text = text.replace("addr: 0.0.0.0:9000", "addr: 0.0.0.0:" + GRPC_PORT, 1)
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
