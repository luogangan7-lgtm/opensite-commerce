/**
 * Responsibility: reusable native React controls and accessible presentation primitives.
 * Dependencies: React and the typed component-contracts dictionary; styles use design tokens.
 * Callers: admin/storefront surfaces and the component gallery.
 * Invariants: native attributes/ref remain usable; no business state, transport, or literal UI copy.
 */
import {
  cloneElement,
  forwardRef,
  useEffect,
  useId,
  useImperativeHandle,
  useRef,
  type ButtonHTMLAttributes,
  type HTMLAttributes,
  type InputHTMLAttributes,
  type ReactElement,
  type ReactNode,
  type SelectHTMLAttributes,
} from 'react';
import { t } from '@opensite/component-contracts';

function classes(...values: (string | undefined | false)[]) {
  return values.filter(Boolean).join(' ');
}

export type ButtonProps = ButtonHTMLAttributes<HTMLButtonElement> & {
  variant?: 'primary' | 'secondary' | 'danger' | 'ghost';
  size?: 'sm' | 'md' | 'lg';
  loading?: boolean;
};

export const Button = forwardRef<HTMLButtonElement, ButtonProps>(function Button(
  { variant = 'primary', size = 'md', loading = false, disabled, className, children, type = 'button', ...props }, ref,
) {
  return (
    <button {...props} ref={ref} type={type} disabled={disabled || loading}
      aria-busy={loading || props['aria-busy']}
      className={classes('os-button', `os-button--${variant}`, `os-button--${size}`, className)}>
      {loading ? <span role="status" aria-live="polite">{t('common.loading')}</span> : children}
    </button>
  );
});

export const TextInput = forwardRef<HTMLInputElement, InputHTMLAttributes<HTMLInputElement>>(function TextInput(
  { className, ...props }, ref,
) {
  return <input {...props} ref={ref} className={classes('os-input', className)} />;
});

export const Select = forwardRef<HTMLSelectElement, SelectHTMLAttributes<HTMLSelectElement>>(function Select(
  { className, children, ...props }, ref,
) {
  return <select {...props} ref={ref} className={classes('os-select', className)}>{children}</select>;
});

export type CheckboxProps = InputHTMLAttributes<HTMLInputElement> & { label: string; indeterminate?: boolean };

export const Checkbox = forwardRef<HTMLInputElement, CheckboxProps>(function Checkbox(
  { label, indeterminate = false, id, className, type: _type, ...props }, ref,
) {
  const generatedId = useId();
  const controlId = id ?? generatedId;
  const inputRef = useRef<HTMLInputElement | null>(null);
  useImperativeHandle(ref, () => inputRef.current!, []);
  useEffect(() => {
    if (inputRef.current) inputRef.current.indeterminate = indeterminate;
  }, [indeterminate]);
  return (
    <label htmlFor={controlId} className={classes('os-checkbox', className)}>
      <input {...props} ref={inputRef} type="checkbox" id={controlId} />
      <span>{label}</span>
    </label>
  );
});

export type FormFieldProps = {
  id: string;
  label: string;
  help?: string;
  error?: string;
  children: ReactElement;
};

export function FormField({ id, label, help, error, children }: FormFieldProps) {
  const child = children as ReactElement<Record<string, unknown>>;
  const helpId = `${id}-help`;
  const errorId = `${id}-error`;
  const describedBy = [child.props['aria-describedby'], help && helpId, error && errorId].filter(Boolean).join(' ');
  return (
    <div className="os-field">
      <label htmlFor={id} className="os-field__label">{label}</label>
      {cloneElement(child, {
        id,
        'aria-describedby': describedBy || undefined,
        'aria-invalid': error ? true : child.props['aria-invalid'],
      })}
      {help && <p id={helpId} className="os-field__help">{help}</p>}
      {error && <p id={errorId} className="os-field__error" role="alert">{error}</p>}
    </div>
  );
}

export type BadgeTone = 'neutral' | 'success' | 'warning' | 'danger' | 'info';

/** A single mapping owns both presentation and translated status labels. */
const toneMap = {
  neutral: { className: 'os-badge--neutral', key: 'status.neutral' },
  success: { className: 'os-badge--success', key: 'status.success' },
  warning: { className: 'os-badge--warning', key: 'status.warning' },
  danger: { className: 'os-badge--danger', key: 'status.danger' },
  info: { className: 'os-badge--info', key: 'status.info' },
} as const;

export type BadgeProps = HTMLAttributes<HTMLSpanElement> & { tone: BadgeTone };
export const Badge = forwardRef<HTMLSpanElement, BadgeProps>(function Badge({ tone, children, className, ...props }, ref) {
  return <span {...props} ref={ref} className={classes('os-badge', toneMap[tone].className, className)}>{children}</span>;
});

export function StatusBadge({ status }: { status: BadgeTone }) {
  return <Badge tone={status}>{t(toneMap[status].key)}</Badge>;
}

export type CardProps = Omit<HTMLAttributes<HTMLDivElement>, 'title'> & { title?: string };
export const Card = forwardRef<HTMLDivElement, CardProps>(function Card({ title, children, className, ...props }, ref) {
  return (
    <div {...props} ref={ref} className={classes('os-card', className)}>
      {title && <h3 className="os-card__title">{title}</h3>}
      {children}
    </div>
  );
});

export type DataTableProps = {
  columns: readonly { key: string; label: string }[];
  rows: readonly { id: string; [key: string]: ReactNode }[];
  caption: string;
  loading?: boolean;
  emptyLabel?: string;
};

export function DataTable({ columns, rows, caption, loading = false, emptyLabel }: DataTableProps) {
  return (
    <div className="os-table-scroll" role="region" aria-label={caption} tabIndex={0}>
      <table className="os-table" aria-busy={loading}>
        <caption>{caption}</caption>
        <thead><tr>{columns.map(column => <th key={column.key} scope="col">{column.label}</th>)}</tr></thead>
        <tbody>
          {loading || rows.length === 0 ? (
            <tr><td colSpan={Math.max(columns.length, 1)} className="os-table__state">
              <span role="status">{loading ? t('common.loading') : (emptyLabel ?? t('table.empty'))}</span>
            </td></tr>
          ) : rows.map(row => <tr key={row.id}>{columns.map(column => <td key={column.key}>{row[column.key]}</td>)}</tr>)}
        </tbody>
      </table>
    </div>
  );
}

export function EmptyState({ title, description, action }: { title: string; description?: string; action?: ReactNode }) {
  return (
    <div className="os-empty-state">
      <h3>{title}</h3>
      {description && <p>{description}</p>}
      {action && <div className="os-empty-state__action">{action}</div>}
    </div>
  );
}

export type DialogProps = {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  title: string;
  description?: string;
  children: ReactNode;
};

function focusableElements(container: HTMLElement) {
  return Array.from(container.querySelectorAll<HTMLElement>(
    'a[href], area[href], button, input, select, textarea, summary, [tabindex], [contenteditable="true"]',
  )).filter(element => element.tabIndex >= 0 && !element.matches(':disabled') &&
    !element.closest('[inert]') && element.getClientRects().length > 0 &&
    getComputedStyle(element).visibility !== 'hidden');
}

export function Dialog({ open, onOpenChange, title, description, children }: DialogProps) {
  const dialogRef = useRef<HTMLDialogElement>(null);
  const contentRef = useRef<HTMLDivElement>(null);
  const changeRef = useRef(onOpenChange);
  changeRef.current = onOpenChange;
  const titleId = useId();
  const descriptionId = useId();

  useEffect(() => {
    const dialog = dialogRef.current;
    if (!open || !dialog) return;
    const opener = dialog.ownerDocument.activeElement;
    const handleCancel = (event: Event) => {
      event.preventDefault();
      changeRef.current(false);
    };
    // Native close events are queued: an earlier cleanup must not dismiss a reopened modal.
    const handleClose = () => { if (!dialog.open) changeRef.current(false); };
    const handleKeyDown = (event: KeyboardEvent) => {
      if (event.key !== 'Tab') return;
      const elements = focusableElements(dialog);
      const first = elements[0];
      const last = elements.at(-1);
      const active = dialog.ownerDocument.activeElement;
      if (!first || !last) {
        event.preventDefault();
        dialog.focus();
      } else if (event.shiftKey && (active === first || active === dialog || !dialog.contains(active))) {
        event.preventDefault();
        last.focus();
      } else if (!event.shiftKey && (active === last || active === dialog || !dialog.contains(active))) {
        event.preventDefault();
        first.focus();
      }
    };
    dialog.addEventListener('cancel', handleCancel);
    dialog.addEventListener('close', handleClose);
    dialog.addEventListener('keydown', handleKeyDown);
    if (!dialog.open) dialog.showModal();
    // Content receives initial focus; an empty/non-interactive body focuses the dialog.
    (contentRef.current && focusableElements(contentRef.current)[0] || dialog).focus();
    return () => {
      dialog.removeEventListener('cancel', handleCancel);
      dialog.removeEventListener('close', handleClose);
      dialog.removeEventListener('keydown', handleKeyDown);
      if (dialog.open) dialog.close();
      if (opener instanceof HTMLElement && opener.isConnected) opener.focus();
    };
  }, [open]);

  return (
    <dialog ref={dialogRef} className="os-dialog" aria-modal="true" aria-labelledby={titleId}
      aria-describedby={description ? descriptionId : undefined} tabIndex={-1}>
      <div className="os-dialog__header">
        <h2 id={titleId}>{title}</h2>
        <Button variant="ghost" aria-label={t('dialog.close')} onClick={() => onOpenChange(false)}>
          {t('dialog.close')}
        </Button>
      </div>
      {description && <p id={descriptionId} className="os-dialog__description">{description}</p>}
      <div ref={contentRef} className="os-dialog__content">{children}</div>
    </dialog>
  );
}
