import { useEffect, useRef, useState } from 'react';

// Mermaid 图渲染：按需动态加载 mermaid（vite 会单独分包，不影响首屏）。
// 渲染失败（语法错误等）时回退显示错误信息，不影响页面其余内容。

let mermaidPromise = null;
let seq = 0;

function getMermaid() {
  if (!mermaidPromise) {
    mermaidPromise = import('mermaid').then((m) => {
      const mermaid = m.default;
      mermaid.initialize({
        startOnLoad: false,
        securityLevel: 'strict', // 禁止图内 HTML/点击注入
        theme: 'default',
      });
      return mermaid;
    });
  }
  return mermaidPromise;
}

export default function MermaidBlock({ code }) {
  const ref = useRef(null);
  const [err, setErr] = useState('');

  useEffect(() => {
    let alive = true;
    setErr('');
    if (ref.current) ref.current.innerHTML = '';
    getMermaid()
      .then(async (mermaid) => {
        const id = `mmd-${Date.now().toString(36)}-${(seq += 1)}`;
        try {
          const { svg } = await mermaid.render(id, String(code ?? ''));
          if (alive && ref.current) {
            ref.current.innerHTML = svg;
            // 图是异步渲染的：撑高气泡后通知会话流，若用户仍贴底则继续贴底
            window.dispatchEvent(new Event('wb:md-resized'));
          }
        } catch (e) {
          if (alive) setErr('Mermaid 语法错误：' + (e?.message || String(e)));
        }
      })
      .catch((e) => {
        if (alive) setErr('Mermaid 加载失败：' + (e?.message || String(e)));
      });
    return () => {
      alive = false;
    };
  }, [code]);

  return (
    <div className="md-mermaid">
      <div className="md-mermaid-canvas" ref={ref} />
      {err && (
        <div className="md-mermaid-err">
          {err}
          <pre>{code}</pre>
        </div>
      )}
    </div>
  );
}
