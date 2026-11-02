// 检索子图的可视化：环形布局 + 有向边
const PALETTE = ['#4f46e5', '#0f766e', '#b45309', '#be123c', '#0369a1', '#7c3aed', '#15803d'];

function hashColor(name) {
  let h = 0;
  for (let i = 0; i < name.length; i += 1) h = (h * 31 + name.charCodeAt(i)) >>> 0;
  return PALETTE[h % PALETTE.length];
}

export default function Subgraph({ entities = [], relations = [], width = 660, height = 380 }) {
  const nodes = entities.slice(0, 12);
  if (nodes.length === 0) return null;

  const cx = width / 2;
  const cy = height / 2;
  const radius = Math.min(width, height) / 2 - 62;
  const positions = new Map();

  nodes.forEach((e, i) => {
    if (i === 0) {
      positions.set(e.name, { x: cx, y: cy, r: 26 });
      return;
    }
    const angle = ((i - 1) / Math.max(1, nodes.length - 1)) * Math.PI * 2 - Math.PI / 2;
    positions.set(e.name, {
      x: cx + Math.cos(angle) * radius,
      y: cy + Math.sin(angle) * radius * 0.78,
      r: 20,
    });
  });

  const edges = [];
  const seen = new Set();
  relations.forEach((rel) => {
    const a = positions.get(rel.source);
    const b = positions.get(rel.target);
    if (!a || !b) return;
    const key = `${rel.source}->${rel.target}`;
    if (seen.has(key)) return;
    seen.add(key);
    edges.push({ ...rel, a, b });
  });

  const short = (s) => (s.length > 8 ? `${s.slice(0, 8)}…` : s);

  return (
    <div className="subgraph">
      <svg viewBox={`0 0 ${width} ${height}`} role="img" aria-label="检索子图">
        <defs>
          <marker
            id="arrow"
            viewBox="0 0 10 10"
            refX="9"
            refY="5"
            markerWidth="6"
            markerHeight="6"
            orient="auto-start-reverse"
          >
            <path d="M 0 0 L 10 5 L 0 10 z" fill="#9aa4b2" />
          </marker>
        </defs>

        {edges.map((e, i) => {
          const dx = e.b.x - e.a.x;
          const dy = e.b.y - e.a.y;
          const len = Math.hypot(dx, dy) || 1;
          const ux = dx / len;
          const uy = dy / len;
          const x1 = e.a.x + ux * (e.a.r + 4);
          const y1 = e.a.y + uy * (e.a.r + 4);
          const x2 = e.b.x - ux * (e.b.r + 8);
          const y2 = e.b.y - uy * (e.b.r + 8);
          return (
            <g key={`${e.source}-${e.target}-${i}`}>
              <line x1={x1} y1={y1} x2={x2} y2={y2} className="edge" markerEnd="url(#arrow)" />
              <title>{`${e.source} → ${e.target}：${e.relation}`}</title>
            </g>
          );
        })}

        {nodes.map((n) => {
          const p = positions.get(n.name);
          const color = hashColor(n.type || n.name);
          return (
            <g key={n.name} transform={`translate(${p.x}, ${p.y})`}>
              <circle r={p.r} fill={`${color}18`} stroke={color} strokeWidth="1.6" />
              <text className="node-label" y={p.r + 15} textAnchor="middle">
                {short(n.name)}
              </text>
              <title>{`${n.name}（${n.type || '实体'}）：${n.description || ''}`}</title>
            </g>
          );
        })}
      </svg>
    </div>
  );
}
