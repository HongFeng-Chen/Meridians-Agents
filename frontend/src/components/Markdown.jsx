import { useMemo } from 'react';
import MermaidBlock from './MermaidBlock.jsx';

// 轻量 Markdown 渲染：不引入第三方依赖（mermaid 按需动态加载，仅图渲染时使用）。
// 安全策略：所有源文本先做 HTML 转义再解析，链接仅放行 http/https，
// 因此任意 LLM 输出/提示词内容都不会注入 HTML。
// 支持：标题、粗斜删、行内代码、围栏代码块、引用、无序/有序列表、表格、分割线、链接。

function escapeHtml(s) {
  return s
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;');
}

// 行内解析（入参已转义）。行内代码先用占位符保护，避免里面的 * 等被二次处理。
function inlineMd(escaped) {
  const codes = [];
  let s = escaped.replace(/`([^`]+)`/g, (_, c) => {
    codes.push(c);
    return `\u0000${codes.length - 1}\u0000`;
  });
  s = s
    .replace(/\*\*([^*]+)\*\*/g, '<strong>$1</strong>')
    .replace(/(^|[^*\w])\*([^*\n]+)\*/g, '$1<em>$2</em>')
    .replace(/__([^_]+)__/g, '<strong>$1</strong>')
    .replace(/~~([^~]+)~~/g, '<del>$1</del>')
    .replace(
      /\[([^\]]+)\]\((https?:\/\/[^)\s]+)\)/g,
      '<a href="$2" target="_blank" rel="noopener noreferrer">$1</a>'
    );
  return s.replace(/\u0000(\d+)\u0000/g, (_, idx) => `<code>${codes[Number(idx)]}</code>`);
}

export function renderMarkdown(src) {
  const lines = String(src ?? '').replace(/\r\n?/g, '\n').split('\n');
  const out = [];
  let para = [];
  const flushPara = () => {
    if (para.length) out.push(`<p>${para.map(inlineMd).join('<br/>')}</p>`);
    para = [];
  };
  let i = 0;
  while (i < lines.length) {
    const line = lines[i];

    // 围栏代码块
    const fence = line.match(/^```(.*)$/);
    if (fence) {
      flushPara();
      const lang = escapeHtml(fence[1].trim());
      const buf = [];
      i++;
      while (i < lines.length && !/^```/.test(lines[i])) {
        buf.push(lines[i]);
        i++;
      }
      i++; // 跳过收尾 ```（没有收尾也照常渲染）
      out.push(
        `<pre class="md-code"${lang ? ` data-lang="${lang}"` : ''}><code>${escapeHtml(buf.join('\n'))}</code></pre>`
      );
      continue;
    }

    // 标题
    const h = line.match(/^(#{1,6})\s+(.*)$/);
    if (h) {
      flushPara();
      const n = h[1].length;
      out.push(`<h${n}>${inlineMd(escapeHtml(h[2]))}</h${n}>`);
      i++;
      continue;
    }

    // 分割线
    if (/^\s*(-{3,}|\*{3,}|_{3,})\s*$/.test(line)) {
      flushPara();
      out.push('<hr/>');
      i++;
      continue;
    }

    // 引用块（递归解析内部）
    if (/^\s*>\s?/.test(line)) {
      flushPara();
      const buf = [];
      while (i < lines.length && /^\s*>\s?/.test(lines[i])) {
        buf.push(lines[i].replace(/^\s*>\s?/, ''));
        i++;
      }
      out.push(`<blockquote>${renderMarkdown(buf.join('\n'))}</blockquote>`);
      continue;
    }

    // 表格：| a | b | + 分隔行
    if (
      /^\s*\|.*\|\s*$/.test(line) &&
      i + 1 < lines.length &&
      /^\s*\|[\s:|-]+\|\s*$/.test(lines[i + 1])
    ) {
      flushPara();
      const parseRow = (l) =>
        l
          .trim()
          .replace(/^\||\|$/g, '')
          .split('|')
          .map((c) => inlineMd(escapeHtml(c.trim())));
      const head = parseRow(lines[i]);
      i += 2;
      const rows = [];
      while (i < lines.length && /^\s*\|.*\|\s*$/.test(lines[i])) {
        rows.push(parseRow(lines[i]));
        i++;
      }
      out.push(
        '<div class="md-table-wrap"><table class="md-table"><thead><tr>' +
          head.map((c) => `<th>${c}</th>`).join('') +
          '</tr></thead><tbody>' +
          rows.map((r) => '<tr>' + r.map((c) => `<td>${c}</td>`).join('') + '</tr>').join('') +
          '</tbody></table></div>'
      );
      continue;
    }

    // 列表（含两格缩进的续行并入上一项）
    if (/^\s*([-*+]|\d+[.)])\s+/.test(line)) {
      flushPara();
      const ordered = /^\s*\d/.test(line);
      const items = [];
      while (i < lines.length) {
        const m = lines[i].match(/^(\s*)([-*+]|\d+[.)])\s+(.*)$/);
        if (m) {
          items.push(inlineMd(escapeHtml(m[3])));
          i++;
        } else if (/^\s{2,}\S/.test(lines[i]) && items.length) {
          items[items.length - 1] += `<br/>${inlineMd(escapeHtml(lines[i].trim()))}`;
          i++;
        } else {
          break;
        }
      }
      const tag = ordered ? 'ol' : 'ul';
      out.push(`<${tag}>${items.map((t) => `<li>${t}</li>`).join('')}</${tag}>`);
      continue;
    }

    // 空行 → 结束段落
    if (!line.trim()) {
      flushPara();
      i++;
      continue;
    }

    para.push(line);
    i++;
  }
  flushPara();
  return out.join('\n');
}

// 把源文本切成 md / mermaid 两类片段：```mermaid 围栏交给 MermaidBlock 渲染，其余走 renderMarkdown。
// 返回 null 表示不含 mermaid 图，调用方可直接走原路径。
export function splitSegments(src) {
  const text = String(src ?? '');
  if (!/```mermaid/i.test(text)) return null;
  const lines = text.replace(/\r\n?/g, '\n').split('\n');
  const segs = [];
  let buf = [];
  let i = 0;
  const flush = () => {
    if (buf.length) {
      segs.push({ type: 'md', text: buf.join('\n') });
      buf = [];
    }
  };
  while (i < lines.length) {
    if (/^```\s*mermaid\s*$/i.test(lines[i])) {
      flush();
      const code = [];
      i += 1;
      while (i < lines.length && !/^```/.test(lines[i])) {
        code.push(lines[i]);
        i += 1;
      }
      i += 1; // 跳过收尾 ```
      segs.push({ type: 'mermaid', text: code.join('\n') });
      continue;
    }
    buf.push(lines[i]);
    i += 1;
  }
  flush();
  return segs;
}

export function Markdown({ text, className = '' }) {
  // splitSegments 返回 null 表示不含 mermaid：整体按普通 Markdown 处理（不能当空值，否则纯文本渲染成空白）
  const segs = useMemo(() => (text ? (splitSegments(text) ?? [{ type: 'md', text }]) : null), [text]);
  if (!text || !segs) return null;
  if (segs.length === 1 && segs[0].type === 'md') {
    return (
      <div
        className={`md-body ${className}`.trim()}
        dangerouslySetInnerHTML={{ __html: renderMarkdown(segs[0].text) }}
      />
    );
  }
  return (
    <div className={`md-body ${className}`.trim()}>
      {segs.map((s, idx) =>
        s.type === 'mermaid' ? (
          <MermaidBlock key={idx} code={s.text} />
        ) : (
          <div key={idx} dangerouslySetInnerHTML={{ __html: renderMarkdown(s.text) }} />
        )
      )}
    </div>
  );
}
