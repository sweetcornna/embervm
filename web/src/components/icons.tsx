// 16px stroke icons (1.5px, currentColor) — one visual voice for the whole
// console; replaces ad-hoc text glyphs. Keep additions in this style.

import type { SVGProps } from "react";

function icon(path: React.ReactNode) {
  return function Icon(props: SVGProps<SVGSVGElement> & { size?: number }) {
    const { size = 16, ...rest } = props;
    return (
      <svg
        width={size}
        height={size}
        viewBox="0 0 16 16"
        fill="none"
        stroke="currentColor"
        strokeWidth="1.5"
        strokeLinecap="round"
        strokeLinejoin="round"
        aria-hidden
        {...rest}
      >
        {path}
      </svg>
    );
  };
}

export const IconPlay = icon(<path d="M4.5 2.8 12.6 8l-8.1 5.2z" />);
export const IconPause = icon(
  <>
    <path d="M5.5 3v10" />
    <path d="M10.5 3v10" />
  </>,
);
export const IconCamera = icon(
  <>
    <rect x="1.8" y="4.5" width="12.4" height="8.7" rx="1.5" />
    <path d="M5.5 4.5 6.6 2.6h2.8l1.1 1.9" />
    <circle cx="8" cy="8.7" r="2.3" />
  </>,
);
export const IconBranch = icon(
  <>
    <circle cx="4.5" cy="3.5" r="1.7" />
    <circle cx="4.5" cy="12.5" r="1.7" />
    <circle cx="11.5" cy="6" r="1.7" />
    <path d="M4.5 5.2v5.6" />
    <path d="M11.5 7.7c0 2.6-4 2.2-5.4 3.6" />
  </>,
);
export const IconUndo = icon(
  <>
    <path d="M2.8 6.5h7a3.7 3.7 0 0 1 0 7.4H6" />
    <path d="m5.6 3.7-2.8 2.8 2.8 2.8" />
  </>,
);
export const IconTerminal = icon(
  <>
    <rect x="1.8" y="2.8" width="12.4" height="10.4" rx="1.5" />
    <path d="m4.5 6 2.2 2-2.2 2" />
    <path d="M8.5 10.2h3" />
  </>,
);
export const IconFolder = icon(
  <path d="M1.8 4.2c0-.8.6-1.4 1.4-1.4h3l1.5 1.7h5.1c.8 0 1.4.6 1.4 1.4v6.4c0 .8-.6 1.4-1.4 1.4H3.2c-.8 0-1.4-.6-1.4-1.4z" />,
);
export const IconFolderOpen = icon(
  <>
    <path d="M1.8 4.2c0-.8.6-1.4 1.4-1.4h3l1.5 1.7h4.6c.8 0 1.4.6 1.4 1.4v.8" />
    <path d="M3.4 13.7h8.9c.6 0 1.2-.4 1.4-1l1.3-4.2c.2-.7-.3-1.4-1-1.4H4.7c-.6 0-1.2.4-1.4 1l-1.5 4.7c.1.5.6.9 1.6.9z" />
  </>,
);
export const IconFile = icon(
  <>
    <path d="M3.5 2.8c0-.6.4-1 1-1H9l3.5 3.5v7.9c0 .6-.4 1-1 1h-7c-.6 0-1-.4-1-1z" />
    <path d="M9 1.8v3.7h3.5" />
  </>,
);
export const IconGlobe = icon(
  <>
    <circle cx="8" cy="8" r="6.2" />
    <path d="M1.8 8h12.4" />
    <path d="M8 1.8c1.8 1.7 2.7 3.9 2.7 6.2S9.8 12.5 8 14.2C6.2 12.5 5.3 10.3 5.3 8S6.2 3.5 8 1.8z" />
  </>,
);
export const IconGauge = icon(
  <>
    <path d="M2 11.8a6.5 6.5 0 1 1 12 0" />
    <path d="M8 9.8 11 5.6" />
    <circle cx="8" cy="10.5" r="1" fill="currentColor" stroke="none" />
  </>,
);
export const IconClock = icon(
  <>
    <circle cx="8" cy="8" r="6.2" />
    <path d="M8 4.5V8l2.4 1.6" />
  </>,
);
export const IconNode = icon(
  <>
    <rect x="2" y="2.5" width="12" height="4.6" rx="1.2" />
    <rect x="2" y="8.9" width="12" height="4.6" rx="1.2" />
    <path d="M4.4 4.8h.01M4.4 11.2h.01" strokeWidth="2" />
  </>,
);
export const IconTrash = icon(
  <>
    <path d="M2.5 4.2h11" />
    <path d="M5.5 4.2V3c0-.6.4-1 1-1h3c.6 0 1 .4 1 1v1.2" />
    <path d="M4 4.2l.7 9c0 .6.5 1 1 1h4.6c.5 0 1-.4 1-1l.7-9" />
  </>,
);
export const IconCopy = icon(
  <>
    <rect x="5.5" y="5.5" width="8" height="8" rx="1.2" />
    <path d="M10.5 5.5V3.7c0-.7-.5-1.2-1.2-1.2H3.7c-.7 0-1.2.5-1.2 1.2v5.6c0 .7.5 1.2 1.2 1.2h1.8" />
  </>,
);
export const IconExternal = icon(
  <>
    <path d="M12.8 8.7v4c0 .6-.4 1-1 1H3.2c-.6 0-1-.4-1-1V4.2c0-.6.4-1 1-1h4" />
    <path d="M9.8 2.2h4v4" />
    <path d="M13.6 2.4 7.7 8.3" />
  </>,
);
export const IconPlus = icon(<path d="M8 3v10M3 8h10" />);
export const IconClose = icon(<path d="m3.8 3.8 8.4 8.4M12.2 3.8l-8.4 8.4" />);
export const IconRefresh = icon(
  <>
    <path d="M13.6 6.6A6 6 0 0 0 3.4 4.4L2 6" />
    <path d="M2 2.5V6h3.5" />
    <path d="M2.4 9.4a6 6 0 0 0 10.2 2.2l1.4-1.6" />
    <path d="M14 13.5V10h-3.5" />
  </>,
);
export const IconChevronRight = icon(<path d="m6 3.5 4.5 4.5L6 12.5" />);
export const IconChevronDown = icon(<path d="m3.5 6 4.5 4.5L12.5 6" />);
export const IconDots = icon(
  <path
    d="M3.2 8h.01M8 8h.01M12.8 8h.01"
    strokeWidth="2.4"
  />,
);
export const IconUpload = icon(
  <>
    <path d="M8 10.5v-8" />
    <path d="m4.8 5.4 3.2-3.2 3.2 3.2" />
    <path d="M2.5 13.5h11" />
  </>,
);
export const IconDownload = icon(
  <>
    <path d="M8 2.5v8" />
    <path d="m4.8 7.4 3.2 3.2 3.2-3.2" />
    <path d="M2.5 13.5h11" />
  </>,
);
export const IconArrowRight = icon(
  <>
    <path d="M2.5 8h11" />
    <path d="m9.5 4 4 4-4 4" />
  </>,
);
export const IconWarn = icon(
  <>
    <path d="M8 2.2 14.6 13c.3.5 0 1.2-.7 1.2H2.1c-.7 0-1-.7-.7-1.2z" />
    <path d="M8 6.2v3.4" />
    <path d="M8 12h.01" strokeWidth="2" />
  </>,
);
export const IconCheck = icon(<path d="m2.8 8.6 3.4 3.4 7-7.4" />);
export const IconInfo = icon(
  <>
    <circle cx="8" cy="8" r="6.2" />
    <path d="M8 7.4v3.4" />
    <path d="M8 4.8h.01" strokeWidth="2" />
  </>,
);
