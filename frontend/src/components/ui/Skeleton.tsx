import React from 'react';

export type SkeletonVariant = 'text' | 'rect' | 'circle' | 'table-row' | 'card';

export interface SkeletonProps {
  variant?: SkeletonVariant;
  /** Overrides the fluid width. Applied as a style property / CSS variable, never string-mixed. */
  width?: string | number;
  height?: string | number;
  /** Row count for the `text` / `card` variants. Capped at 4 for `text`; ignored by others. */
  lines?: number;
  /** Overrides the sr-only announcement text (default `'Loading'`). */
  ariaLabel?: string;
  /**
   * Whether this skeleton carries its own live-region announcement. Set false on pages that
   * coordinate a single shared `role="status"` region so multiple sections never announce
   * simultaneously (UX §8.3 single-live-region policy).
   */
  announceOnLoad?: boolean;
  className?: string;
}

let styleInjected = false;
const STYLE_ID = 'fms-skeleton-styles';

// Single, deduplicated style block. Lives outside the component so a strict CSP never needs
// inline `style-src 'unsafe-inline'` for the shimmer keyframes (Security §9.1).
const FMS_SKELETON_CSS = `
:root {
  --fms-skeleton-base:#E6E8EB;                      /* neutral gray, light theme */
  --fms-skeleton-highlight:rgba(255,255,255,0.65);  /* shimmer streak overlay   */
  --fms-skeleton-radius-text:var(--fms-radius-sm);
  --fms-skeleton-radius-rect:var(--fms-radius-md);
  --fms-skeleton-radius-card:var(--fms-radius-lg);
  --fms-skeleton-radius-circle:var(--fms-radius-full);
  --fms-skeleton-shimmer-duration:1800ms;
  --fms-skeleton-pulse-duration:1200ms;
}
.fms-skeleton {
  position:relative;
  overflow:hidden;
  background:var(--fms-skeleton-base);
  border-radius:var(--fms-radius-md);
  user-select:none;
  pointer-events:none;
}
.fms-skeleton--text,.fms-skeleton--table-row,.fms-skeleton--card{border-radius:var(--fms-radius-sm);}
.fms-skeleton--rect{border-radius:var(--fms-radius-md);}
.fms-skeleton--card{border-radius:var(--fms-radius-lg);}
.fms-skeleton--circle{border-radius:var(--fms-radius-full);}
.fms-skeleton__line{background:var(--fms-skeleton-base);border-radius:var(--fms-radius-sm);}
.fms-skeleton__cell{background:var(--fms-skeleton-base);border-radius:var(--fms-radius-sm);}
.fms-skeleton__group{background:var(--fms-skeleton-base);border-radius:var(--fms-radius-sm);}
.fms-skeleton--text{display:flex;flex-direction:column;gap:var(--fms-space-2);}
.fms-skeleton--table-row{display:flex;align-items:center;gap:var(--fms-space-4);}
.fms-skeleton--card{display:flex;flex-direction:column;gap:var(--fms-space-3);}
.fms-skeleton__cell{height:24px;}
.fms-skeleton__cell--date{width:15%;}
.fms-skeleton__cell--desc{width:30%;}
.fms-skeleton__cell--cat{width:20%;}
.fms-skeleton__cell--acc{width:18%;}
.fms-skeleton__cell--amt{width:17%;}
@media (max-width:767px){
  .fms-skeleton__cell--cat,.fms-skeleton__cell--acc{display:none;}
}
.fms-skeleton--card{padding:var(--fms-space-6);}
.fms-skeleton__title{width:40%;height:20px;}
.fms-skeleton__stat{width:60%;height:32px;}
.fms-skeleton__body{height:14px;width:100%;}
/* shimmer: transform-only overlay, zero layout/paint reflow */
.fms-skeleton::after{
  content:"";
  position:absolute;inset:0;
  transform:translateX(-100%);
  background:linear-gradient(90deg,transparent,var(--fms-skeleton-highlight),transparent);
  animation:fms-skeleton-shimmer var(--fms-skeleton-shimmer-duration) linear infinite;
}
@keyframes fms-skeleton-shimmer{100%{transform:translateX(100%);}}
/* reduced motion: static placeholder + bounded opacity pulse, never a 0→1 flash */
@media (prefers-reduced-motion:reduce){
  .fms-skeleton::after{animation:none;opacity:0;}
  .fms-skeleton{animation:fms-skeleton-pulse var(--fms-skeleton-pulse-duration) ease-in-out infinite alternate;}
}
@keyframes fms-skeleton-pulse{from{opacity:0.55;}to{opacity:1;}}
.fms-skeleton__sr{
  position:absolute;width:1px;height:1px;padding:0;margin:-1px;overflow:hidden;
  clip:rect(0 0 0 0);clip-path:inset(50%);white-space:nowrap;border:0;
}
`;

const SKELETON_TEXT_LINES = [100, 100, 85, 70];
const SKELETON_TABLE_CELLS = ['date', 'desc', 'cat', 'acc', 'amt'] as const;

/**
 * Pure, presentational loading placeholder. Reserves the exact space of real content
 * (CLS ≤ 0.1) with a transform-only shimmer. The visual skeleton is `aria-hidden`
 * decorative; the status announcement lives in a separate `role="status"` live region.
 */
export function Skeleton({
  variant = 'text',
  width,
  height,
  lines = 1,
  ariaLabel = 'Loading',
  announceOnLoad = true,
  className = '',
}: SkeletonProps): JSX.Element {
  if (typeof document !== 'undefined' && !styleInjected) {
    styleInjected = true;
    const existing = document.getElementById(STYLE_ID);
    if (!existing) {
      const style = document.createElement('style');
      style.id = STYLE_ID;
      style.textContent = FMS_SKELETON_CSS;
      document.head.appendChild(style);
    }
  }

  const variantClass = `fms-skeleton fms-skeleton--${variant}${className ? ` ${className}` : ''}`;
  const dimensionStyle: React.CSSProperties = {};
  if (width !== undefined) dimensionStyle.width = width;
  if (height !== undefined) dimensionStyle.height = height;
  if (variant === 'circle' && width === undefined && height === undefined) {
    dimensionStyle.width = 40;
    dimensionStyle.height = 40;
    dimensionStyle.flex = 'none';
  }
  if (variant === 'rect' && width === undefined && height === undefined) {
    dimensionStyle.aspectRatio = '16 / 9';
  }

  let body: JSX.Element;
  if (variant === 'text') {
    const count = Math.min(Math.max(lines, 1), 4);
    body = (
      <>
        {Array.from({ length: count }).map((_, i) => (
          <span
            key={i}
            className="fms-skeleton__line"
            style={{
              height: 24,
              width: i === count - 1 && count > 1 ? `${SKELETON_TEXT_LINES[i] ?? 65}%` : '100%',
            }}
          />
        ))}
      </>
    );
  } else if (variant === 'table-row') {
    body = (
      <>
        {SKELETON_TABLE_CELLS.map((cell) => (
          <span key={cell} className={`fms-skeleton__cell fms-skeleton__cell--${cell}`} />
        ))}
      </>
    );
  } else if (variant === 'card') {
    const bodyCount = Math.min(Math.max(lines, 1), 3);
    body = (
      <>
        <span className="fms-skeleton__group fms-skeleton__title" />
        <span className="fms-skeleton__group fms-skeleton__stat" />
        {Array.from({ length: bodyCount }).map((_, i) => (
          <span
            key={i}
            className="fms-skeleton__group fms-skeleton__body"
            style={{ width: `${SKELETON_TEXT_LINES[i] ?? 100}%` }}
          />
        ))}
      </>
    );
  } else {
    // rect / circle: single block, sized by the dimension style above.
    body = <span className="fms-skeleton__group fms-skeleton--bare" style={{ display: 'none' }} />;
  }

  const decorative = (
    <div aria-hidden="true" className={variantClass} style={dimensionStyle}>
      {body}
    </div>
  );

  // The live region must sit OUTSIDE every aria-hidden ancestor, otherwise the
  // announcement is dropped (Security §check 2). When announceOnLoad is false the
  // consumer owns a shared status region, so render the skeleton standalone as hidden.
  if (!announceOnLoad || !ariaLabel) {
    return <div aria-hidden="true" className={variantClass} style={dimensionStyle}>{body}</div>;
  }

  return (
    <div role="status">
      <span className="fms-skeleton__sr">{ariaLabel}</span>
      {decorative}
    </div>
  );
}

export default Skeleton;