import { useEffect, type RefObject } from 'react';

/**
 * useDismiss closes a popover on Escape or a press outside its host (the
 * element holding both the trigger and the popover).
 */
export function useDismiss(open: boolean, host: RefObject<HTMLElement | null>, close: () => void) {
  useEffect(() => {
    if (!open) return;
    const key = (e: KeyboardEvent) => {
      if (e.key === 'Escape') close();
    };
    const outside = (e: PointerEvent) => {
      if (host.current && e.target instanceof Node && !host.current.contains(e.target)) close();
    };
    document.addEventListener('keydown', key);
    document.addEventListener('pointerdown', outside);
    return () => {
      document.removeEventListener('keydown', key);
      document.removeEventListener('pointerdown', outside);
    };
  }, [open, host, close]);
}
