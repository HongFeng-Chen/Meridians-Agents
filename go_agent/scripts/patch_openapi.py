#!/usr/bin/env python3
"""把上传类接口的 OpenAPI 契约补成真实的 multipart/form-data。

背景：kratos 的 protoc-gen-openapi 不认 proto3 的 bytes 字段，遇到
`bytes file` 会直接跳过该字段，请求体退化成 `application/json: {}`，
前端 SDK 生成器就无从判断这是文件上传。

做法：proto 保持 `bytes file`（gRPC 侧语义正确），生成后由本脚本对
指定 operation 的 requestBody 做一次定点替换，声明 multipart/form-data
与 binary 的 file 部分。脚本幂等，可重复执行。

用法：
    python scripts/patch_openapi.py api/openapi.yaml
"""

from __future__ import annotations

import sys

# 需要改写成 multipart 的操作（proto 中 body: "file" 且含 bytes 字段）
TARGET_OPS = {
    "RagService_ExtractText",
    "RagService_UploadAndIngest",
    "ConversationService_UploadMedia",
}

# 生成器产出的空请求体（缩进与 kratos 输出一致：path 4 空格 → method 8 → 字段 12）
EMPTY_BODY = [
    "            requestBody:\n",
    "                content:\n",
    "                    application/json: {}\n",
    "                required: true\n",
]

MULTIPART_BODY = [
    "            requestBody:\n",
    "                content:\n",
    "                    multipart/form-data:\n",
    "                        schema:\n",
    "                            type: object\n",
    "                            properties:\n",
    "                                file:\n",
    "                                    type: string\n",
    "                                    format: binary\n",
    "                            required:\n",
    "                                - file\n",
    "                required: true\n",
]


def split_path_blocks(lines: list[str]) -> list[list[str]]:
    """按顶层 path（4 空格 + '/'）切块，保留块顺序。"""
    blocks: list[list[str]] = []
    for line in lines:
        if line.startswith("    /"):
            blocks.append([line])
        elif blocks:
            blocks[-1].append(line)
        else:
            blocks.append([line])  # 文件头（openapi/info/...）
    return blocks


def patch_block(block: list[str]) -> tuple[list[str], bool]:
    text = "".join(block)
    if not any(f"operationId: {op}" in text for op in TARGET_OPS):
        return block, False
    # 幂等判断不能用「multipart/form-data」——接口描述文字里也可能出现这个词，
    # 用只在 MULTIPART_BODY 里出现的 format: binary 判断
    if "format: binary" in text:
        return block, False  # 已经是目标形态，幂等返回

    out: list[str] = []
    i = 0
    patched = False
    while i < len(block):
        if block[i : i + len(EMPTY_BODY)] == EMPTY_BODY:
            out.extend(MULTIPART_BODY)
            i += len(EMPTY_BODY)
            patched = True
            continue
        out.append(block[i])
        i += 1
    return out, patched


def main() -> int:
    if len(sys.argv) != 2:
        print("用法: python scripts/patch_openapi.py api/openapi.yaml", file=sys.stderr)
        return 2
    path = sys.argv[1]
    with open(path, encoding="utf-8") as f:
        lines = f.readlines()

    changed: list[str] = []
    result: list[str] = []
    for block in split_path_blocks(lines):
        new_block, patched = patch_block(block)
        if patched:
            changed.append(new_block[0].strip().rstrip(":"))
        result.extend(new_block)

    with open(path, "w", encoding="utf-8", newline="\n") as f:
        f.writelines(result)

    if changed:
        print("已改写为 multipart/form-data: " + ", ".join(changed))
    else:
        print("无需改写（已是最新）")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
