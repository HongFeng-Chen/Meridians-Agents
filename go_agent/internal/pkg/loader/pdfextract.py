#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""PDF 文本抽取脚本（被 Go loader 以子进程调用，也可单独使用）。

用法：python pdfextract.py <file.pdf> [--engine pdfplumber|pymupdf]

为什么需要它：纯 Go 的 PDF 库对中文财报/年报这类「CID 字体 + 自定义 ToUnicode + 表格混排」
的 PDF 解析会产出倒序、错字与 ◆ 占位符。pdfplumber（pdfminer.six）与 PyMuPDF（MuPDF）
能正确还原文本层与阅读顺序。

约定：
  - 成功时把 UTF-8 文本写到 stdout，多页时带 "[第 N 页]" 分隔（与 Go 原生抽取保持一致）；
  - 引擎不可用 / 解析失败时写 stderr 并以非 0 退出码结束，由 Go 侧决定是否降级。
"""

import os
import sys


def extract_pdfplumber(path):
    """按页抽取，单页异常不影响整篇。"""
    import pdfplumber

    pages = []
    with pdfplumber.open(path) as pdf:
        for page in pdf.pages:
            try:
                pages.append(page.extract_text() or "")
            except Exception:  # 单页坏对象：跳过，保留其余页
                pages.append("")
    return pages


def extract_pymupdf(path):
    import fitz  # PyMuPDF

    doc = fitz.open(path)
    try:
        return [page.get_text("text") or "" for page in doc]
    finally:
        doc.close()


ENGINES = {
    "pdfplumber": extract_pdfplumber,
    "pymupdf": extract_pymupdf,
}


def main(argv):
    args = [a for a in argv[1:] if not a.startswith("--")]
    engine = ""
    for i, a in enumerate(argv[1:], 1):
        if a.startswith("--engine="):
            engine = a.split("=", 1)[1]
        elif a == "--engine" and i + 1 < len(argv):
            engine = argv[i + 1]

    if not args:
        sys.stderr.write("usage: pdfextract.py <file.pdf> [--engine pdfplumber|pymupdf]\n")
        return 2

    path = args[0]
    if not os.path.isfile(path):
        sys.stderr.write("file not found: %s\n" % path)
        return 2

    order = [engine] if engine else ["pdfplumber", "pymupdf"]
    pages, errors = None, []
    for name in order:
        fn = ENGINES.get(name)
        if fn is None:
            errors.append("%s: unknown engine" % name)
            continue
        try:
            pages = fn(path)
            break
        except Exception as exc:  # 缺依赖、加密、损坏等
            errors.append("%s: %s" % (name, exc))

    if pages is None:
        sys.stderr.write("; ".join(errors) or "no engine available")
        return 3

    multi = len(pages) > 1
    out = []
    for idx, text in enumerate(pages, 1):
        text = (text or "").replace("\x00", "").strip("\n")
        if not text.strip():
            continue
        if multi:
            out.append("[第 %d 页]" % idx)
        out.append(text)

    sys.stdout.write("\n".join(out))
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main(sys.argv))
    except BrokenPipeError:
        sys.exit(0)
