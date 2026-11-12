import { useEffect, useMemo, useRef, useState } from "react";
import type { KeyboardEvent, ReactNode } from "react";
import { highlight, type Lang } from "./highlight";

export function Spinner({ small }: { small?: boolean }) {
  return <span className={small ? "spinner small" : "spinner"} role="status" aria-label="Loading" />;
}

export function Empty({ title, children }: { title: string; children?: ReactNode }) {
  return (
    <div className="empty">
      <h3>{title}</h3>
      {children}
    </div>
  );
}

export function ErrorBox({ errors, title }: { errors: string[] | string; title?: string }) {
  const list = (Array.isArray(errors) ? errors : [errors]).filter(Boolean);
  if (list.length === 0) return null;
  return (
    <div className="errorbox" role="alert">
      {title && <strong>{title}</strong>}
      {list.length === 1 ? (
        <p>{list[0]}</p>
      ) : (
        <ul>
          {list.map((e, i) => (
            <li key={i}>{e}</li>
          ))}
        </ul>
      )}
    </div>
  );
}

export function CopyButton({ text, label = "Copy" }: { text: string; label?: string }) {
  const [done, setDone] = useState(false);
  async function copy() {
    try {
      await navigator.clipboard.writeText(text);
      setDone(true);
      setTimeout(() => setDone(false), 1500);
    } catch {
      // Clipboard access can be denied; the text stays selectable.
    }
  }
  return (
    <button className="btn small" onClick={copy}>
      {done ? "Copied" : label}
    </button>
  );
}

export function download(name: string, text: string, type = "text/plain") {
  const url = URL.createObjectURL(new Blob([text], { type }));
  const a = document.createElement("a");
  a.href = url;
  a.download = name;
  a.click();
  setTimeout(() => URL.revokeObjectURL(url), 1000);
}

export function Code({ text, lang, copy, downloadAs }: { text: string; lang: Lang; copy?: boolean; downloadAs?: string }) {
  const nodes = useMemo(() => highlight(text, lang), [text, lang]);
  return (
    <div className="code">
      {(copy || downloadAs) && (
        <div className="code-tools">
          {copy && <CopyButton text={text} />}
          {downloadAs && (
            <button className="btn small" onClick={() => download(downloadAs, text)}>
              Download
            </button>
          )}
        </div>
      )}
      <pre className={"code-pre lang-" + lang}>{nodes}</pre>
    </div>
  );
}

export interface SegOption<T extends string> {
  value: T;
  label: string;
  disabled?: string; // reason, shown as a tooltip
}

export function Segmented<T extends string>({
  value,
  options,
  onChange,
}: {
  value: T;
  options: SegOption<T>[];
  onChange: (v: T) => void;
}) {
  return (
    <div className="segmented" role="radiogroup">
      {options.map((o) => (
        <button
          key={o.value}
          role="radio"
          aria-checked={value === o.value}
          className={value === o.value ? "on" : ""}
          disabled={!!o.disabled}
          title={o.disabled}
          onClick={() => onChange(o.value)}
        >
          {o.label}
        </button>
      ))}
    </div>
  );
}

export function Modal({
  title,
  children,
  footer,
  onClose,
  wide,
}: {
  title: string;
  children: ReactNode;
  footer?: ReactNode;
  onClose: () => void;
  wide?: boolean;
}) {
  useEffect(() => {
    const h = (e: globalThis.KeyboardEvent) => e.key === "Escape" && onClose();
    window.addEventListener("keydown", h);
    return () => window.removeEventListener("keydown", h);
  }, [onClose]);
  return (
    <div className="modal-backdrop" onMouseDown={(e) => e.target === e.currentTarget && onClose()}>
      <div className={"modal" + (wide ? " wide" : "")} role="dialog" aria-modal="true" aria-label={title}>
        <header>
          <h2>{title}</h2>
          <button className="icon-btn" onClick={onClose} aria-label="Close">
            ×
          </button>
        </header>
        <div className="modal-body">{children}</div>
        {footer && <footer>{footer}</footer>}
      </div>
    </div>
  );
}

export interface Option {
  value: string;
  label?: string;
  hint?: string;
  disabled?: string; // reason the option cannot be chosen
}

/** A searchable select for long lists (tables, columns). */
export function Combobox({
  value,
  options,
  onChange,
  placeholder,
  autoFocus,
  ariaLabel,
}: {
  value: string;
  options: Option[];
  onChange: (v: string) => void;
  placeholder?: string;
  autoFocus?: boolean;
  ariaLabel?: string;
}) {
  const [open, setOpen] = useState(false);
  const [q, setQ] = useState("");
  const [active, setActive] = useState(0);
  const listRef = useRef<HTMLUListElement>(null);
  const filtered = useMemo(() => {
    const f = q.trim().toLowerCase();
    const m = f ? options.filter((o) => (o.label ?? o.value).toLowerCase().includes(f)) : options;
    return m.slice(0, 300);
  }, [q, options]);

  useEffect(() => {
    listRef.current?.querySelector(".active")?.scrollIntoView({ block: "nearest" });
  }, [active, open]);

  function choose(o: Option) {
    if (o.disabled) return;
    onChange(o.value);
    setOpen(false);
    setQ("");
  }
  function onKey(e: KeyboardEvent<HTMLInputElement>) {
    if (e.key === "ArrowDown") {
      e.preventDefault();
      setOpen(true);
      setActive((a) => Math.min(a + 1, filtered.length - 1));
    } else if (e.key === "ArrowUp") {
      e.preventDefault();
      setActive((a) => Math.max(a - 1, 0));
    } else if (e.key === "Enter") {
      e.preventDefault();
      const o = filtered[active];
      if (o) choose(o);
    } else if (e.key === "Escape") {
      setOpen(false);
      setQ("");
    }
  }
  const current = options.find((o) => o.value === value);
  return (
    <div className="combo">
      <input
        className="input"
        role="combobox"
        aria-label={ariaLabel}
        aria-expanded={open}
        autoFocus={autoFocus}
        value={open ? q : (current?.label ?? value)}
        placeholder={open ? (current?.label ?? value) || placeholder : placeholder}
        onFocus={() => {
          setOpen(true);
          setQ("");
          setActive(0);
        }}
        onBlur={() => setTimeout(() => setOpen(false), 150)}
        onChange={(e) => {
          setQ(e.target.value);
          setActive(0);
          setOpen(true);
        }}
        onKeyDown={onKey}
      />
      {open && (
        <ul className="combo-list" ref={listRef} role="listbox">
          {filtered.map((o, i) => (
            <li
              key={o.value}
              role="option"
              aria-selected={i === active}
              aria-disabled={!!o.disabled}
              className={(i === active ? "active " : "") + (o.disabled ? "disabled" : "")}
              onMouseDown={(e) => {
                e.preventDefault();
                choose(o);
              }}
              onMouseEnter={() => setActive(i)}
              title={o.disabled}
            >
              <span className="mono">{o.label ?? o.value}</span>
              {(o.disabled || o.hint) && <span className="hint">{o.disabled ?? o.hint}</span>}
            </li>
          ))}
          {filtered.length === 0 && <li className="disabled">No matches</li>}
        </ul>
      )}
    </div>
  );
}

export function Logo() {
  return (
    <svg className="logo" viewBox="0 0 32 32" aria-hidden="true">
      <rect x="3" y="3" width="17" height="17" rx="4" fill="var(--accent)" />
      <rect x="12" y="12" width="17" height="17" rx="4" fill="var(--accent)" fillOpacity=".45" />
    </svg>
  );
}
