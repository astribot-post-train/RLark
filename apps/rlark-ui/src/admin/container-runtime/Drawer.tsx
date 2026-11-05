import { useEffect, useRef, type ReactNode } from "react";
import { Hint } from "./Charts";

// Side drawer for forms. Children stay mounted while closed so in-flight
// requests and retained intents survive toggling; only visibility changes.
export function Drawer({
  open,
  title,
  hint,
  closeLabel,
  onClose,
  children,
}: {
  open: boolean;
  title: string;
  hint?: string;
  closeLabel: string;
  onClose: () => void;
  children: ReactNode;
}) {
  const panel = useRef<HTMLElement>(null);
  useEffect(() => {
    if (!open) return;
    panel.current?.focus();
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") onClose();
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [open, onClose]);
  return (
    <div className="drawer-layer" hidden={!open}>
      <div className="drawer-backdrop" onClick={onClose} />
      <aside
        ref={panel}
        className="drawer"
        role="dialog"
        aria-modal="true"
        aria-label={title}
        tabIndex={-1}
      >
        <header className="drawer-head">
          <h2>
            {title}
            {hint && <Hint text={hint} />}
          </h2>
          <button type="button" aria-label={closeLabel} onClick={onClose}>
            ×
          </button>
        </header>
        <div className="drawer-body">{children}</div>
      </aside>
    </div>
  );
}
