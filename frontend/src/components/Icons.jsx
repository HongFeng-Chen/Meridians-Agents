// 轻量内联 SVG 图标集（无第三方依赖）
import React from 'react';

function Svg({ size = 18, children, ...rest }) {
  return (
    <svg
      width={size}
      height={size}
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.7"
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
      {...rest}
    >
      {children}
    </svg>
  );
}

export const IconDashboard = (p) => (
  <Svg {...p}>
    <rect x="3" y="3" width="7" height="7" rx="1.6" />
    <rect x="14" y="3" width="7" height="7" rx="1.6" />
    <rect x="3" y="14" width="7" height="7" rx="1.6" />
    <rect x="14" y="14" width="7" height="7" rx="1.6" />
  </Svg>
);

export const IconUpload = (p) => (
  <Svg {...p}>
    <path d="M12 15V4" />
    <path d="m7.5 8.5 4.5-4.5 4.5 4.5" />
    <path d="M4 15v3.5A1.5 1.5 0 0 0 5.5 20h13a1.5 1.5 0 0 0 1.5-1.5V15" />
  </Svg>
);

export const IconSearch = (p) => (
  <Svg {...p}>
    <circle cx="11" cy="11" r="6.5" />
    <path d="m16 16 4.5 4.5" />
  </Svg>
);

export const IconFlask = (p) => (
  <Svg {...p}>
    <path d="M9.5 3h5M10.5 3v5.2L5.2 17.2A2 2 0 0 0 7 20h10a2 2 0 0 0 1.8-2.8L13.5 8.2V3" />
    <path d="M7.6 14.5h8.8" />
  </Svg>
);

export const IconGraph = (p) => (
  <Svg {...p}>
    <circle cx="6" cy="6.5" r="2.6" />
    <circle cx="18" cy="6.5" r="2.6" />
    <circle cx="12" cy="18" r="2.6" />
    <path d="M8.4 7.8 10.4 16M15.6 7.8 13.6 16M8.6 6.5h6.8" />
  </Svg>
);

export const IconDatabase = (p) => (
  <Svg {...p}>
    <ellipse cx="12" cy="5.5" rx="7.5" ry="2.8" />
    <path d="M4.5 5.5v6c0 1.5 3.4 2.8 7.5 2.8s7.5-1.3 7.5-2.8v-6" />
    <path d="M4.5 11.5v6c0 1.5 3.4 2.8 7.5 2.8s7.5-1.3 7.5-2.8v-6" />
  </Svg>
);

export const IconSettings = (p) => (
  <Svg {...p}>
    <path d="M4 7h9M17 7h3M4 17h3M11 17h9" />
    <circle cx="15" cy="7" r="2.2" />
    <circle cx="9" cy="17" r="2.2" />
  </Svg>
);

export const IconDoc = (p) => (
  <Svg {...p}>
    <path d="M14 3H7a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h10a2 2 0 0 0 2-2V8z" />
    <path d="M14 3v5h5M9 13h6M9 17h4" />
  </Svg>
);

export const IconTrash = (p) => (
  <Svg {...p}>
    <path d="M4 7h16M10 11v6M14 11v6" />
    <path d="M6 7l1 12a2 2 0 0 0 2 2h6a2 2 0 0 0 2-2l1-12M9 7V4.8A1.8 1.8 0 0 1 10.8 3h2.4A1.8 1.8 0 0 1 15 4.8V7" />
  </Svg>
);

export const IconRefresh = (p) => (
  <Svg {...p}>
    <path d="M20 11a8 8 0 1 0-2.3 5.7" />
    <path d="M20 4.5V11h-6.2" />
  </Svg>
);

export const IconSpark = (p) => (
  <Svg {...p}>
    <path d="M12 3.5 13.7 9l5.5 1.7-5.5 1.7L12 18l-1.7-5.6L4.8 10.7 10.3 9z" />
  </Svg>
);

export const IconArrowRight = (p) => (
  <Svg {...p}>
    <path d="M5 12h13M13 6.5 18.5 12 13 17.5" />
  </Svg>
);

export const IconCheck = (p) => (
  <Svg {...p}>
    <path d="m4.5 12.5 5 5 10-11" />
  </Svg>
);

export const IconAlert = (p) => (
  <Svg {...p}>
    <circle cx="12" cy="12" r="8.5" />
    <path d="M12 8v5M12 16.2v.1" />
  </Svg>
);

export const IconLink = (p) => (
  <Svg {...p}>
    <path d="M9.5 14.5 14.5 9.5" />
    <path d="M7.5 12 5.7 13.8a3.9 3.9 0 0 0 5.5 5.5L13 17.5" />
    <path d="M16.5 12l1.8-1.8a3.9 3.9 0 0 0-5.5-5.5L11 6.5" />
  </Svg>
);

// 提示词模板：代码尖括号 + 文本行
export const IconPrompt = (p) => (
  <Svg {...p}>
    <path d="m8.5 8-4 4 4 4" />
    <path d="m15.5 8 4 4-4 4" />
    <path d="M13.2 6.5 10.8 17.5" />
  </Svg>
);

// 记忆：大脑轮廓 + 分叉
export const IconBrain = (p) => (
  <Svg {...p}>
    <path d="M9.5 4.5a3 3 0 0 0-3 3 3 3 0 0 0-1.5 5.4A3 3 0 0 0 7.5 19c1 0 1.6-.4 2-.9V6.2a2 2 0 0 0-2-1.7Z" />
    <path d="M14.5 4.5a3 3 0 0 1 3 3 3 3 0 0 1 1.5 5.4A3 3 0 0 1 16.5 19c-1 0-1.6-.4-2-.9V6.2a2 2 0 0 1 2-1.7Z" />
  </Svg>
);

// 数据源：插头
export const IconPlug = (p) => (
  <Svg {...p}>
    <path d="M9 3v5M15 3v5" />
    <path d="M6.5 8h11v3a5.5 5.5 0 0 1-11 0V8Z" />
    <path d="M12 16.5V21" />
  </Svg>
);

// 技能：立方体
export const IconBox = (p) => (
  <Svg {...p}>
    <path d="m12 3 7.5 4.2v9.6L12 21l-7.5-4.2V7.2L12 3Z" />
    <path d="m4.7 7.4 7.3 4.1 7.3-4.1M12 21v-9.5" />
  </Svg>
);


// AI 平台：云 + 接入点，代表外部广场（千问 / 百炼）
export const IconCloud = (p) => (
  <Svg {...p}>
    <path d="M7 18.5a4 4 0 0 1-.4-8A5.5 5.5 0 0 1 17.4 10a3.8 3.8 0 0 1 .6 7.6" />
    <path d="M7 18.5h10" />
    <path d="M12 13v5.5M9.7 15.7 12 13.4l2.3 2.3" />
  </Svg>
);

export const IconClock = (p) => (
  <Svg {...p}>
    <circle cx="12" cy="12" r="9" />
    <path d="M12 7v5l3.2 1.8" />
  </Svg>
);

export const IconChat = (p) => (
  <Svg {...p}>
    <path d="M21 12a8 8 0 0 1-8 8H5l-2 2V12a8 8 0 0 1 8-8h2a8 8 0 0 1 8 8Z" />
    <path d="M8.5 11h.01M12 11h.01M15.5 11h.01" />
  </Svg>
);

/** 辩论：两个相向的气泡，代表多角色交锋 */
export const IconDebate = (p) => (
  <Svg {...p}>
    <path d="M8 4H4a1 1 0 0 0-1 1v6a1 1 0 0 0 1 1h2v3l3-3h1" />
    <path d="M16 20h4a1 1 0 0 0 1-1v-6a1 1 0 0 0-1-1h-2v-3l-3 3h-1" />
    <path d="M10 12h4M12 10v4" />
  </Svg>
);

/** 租户：多窗口楼宇，代表一个隔离域 */
export const IconTenant = (p) => (
  <Svg {...p}>
    <path d="M4 21V6.5a1.5 1.5 0 0 1 1-1.4l6-2.3a1 1 0 0 1 1.3.9V21" />
    <path d="M12 10h6.5a1.5 1.5 0 0 1 1.5 1.5V21" />
    <path d="M2.5 21h19" />
    <path d="M7 8.5h2M7 12h2M7 15.5h2M15 13.5h1.5M15 17h1.5" />
  </Svg>
);

/** 成员：单人剪影 */
export const IconUser = (p) => (
  <Svg {...p}>
    <circle cx="12" cy="8.5" r="3.6" />
    <path d="M4.8 20.2a7.4 7.4 0 0 1 14.4 0" />
  </Svg>
);
