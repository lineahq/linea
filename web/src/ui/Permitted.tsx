import type { CSSProperties, ReactElement, ReactNode } from 'react';
import type { Permission } from './access';
import { useDenial } from './session';

/**
 * Permitted renders its link when the caller holds perm. Otherwise the link
 * stays in view as a disabled button with the same look, titled with the
 * missing permission, since a link cannot be disabled.
 */
export function Permitted({ perm, children }: { perm: Permission; children: ReactElement<{ className?: string; style?: CSSProperties; children?: ReactNode }> }) {
  const denied = useDenial(perm);
  if (!denied) return children;
  const { className, style, children: content } = children.props;
  return (
    <button type="button" className={className} style={style} disabled title={denied}>
      {content}
    </button>
  );
}
