/** Demonstrates shared component states; depends only on UI/messages; called by the guarded gallery route. Examples are controlled, with no commerce data. */
import { useState, type ReactNode } from 'react';
import { Badge, Button, Card, Checkbox, DataTable, Dialog, EmptyState, FormField, Select, StatusBadge, TextInput } from '@opensite/ui';
import { t } from '@opensite/component-contracts';

import { variants, sizes, buttonStates, tones, inputStates, selectStates, checkboxStates, fieldStates } from './gallery-cases';

function Case({ id, label, children }: { id: string; label: ReactNode; children: ReactNode }) {
  return <div className="gallery-case" data-gallery-case={id} data-testid={`case-${id}`}>
    <h3>{label}</h3><div className="gallery-example">{children}</div><code>{id}</code>
  </div>;
}
const sections = ['buttons', 'inputs', 'selects', 'checkboxes', 'fields', 'badges', 'statuses', 'cards', 'tables', 'empty', 'dialog'] as const;
function Section({ name, children }: { name: typeof sections[number]; children: ReactNode }) {
  return <section id={name} className="gallery-section" aria-labelledby={`${name}-heading`}>
    <h2 id={`${name}-heading`}>{t(`gallery.${name}`)}</h2><div className="gallery-cases">{children}</div>
  </section>;
}
function InputCase({ state }: { state: typeof inputStates[number] }) {
  const [value, setValue] = useState(state === 'filled' ? t('label.input') : '');
  const control = <TextInput id={`input-${state}`} value={value} onChange={event => setValue(event.target.value)} disabled={state === 'disabled'} aria-label={t('label.input')} />;
  return <Case id={`input-${state}`} label={<>{t('gallery.inputs')} · {t(`state.${state}`)}</>}>
    {state === 'error' ? <FormField id={`input-${state}`} label={t('label.input')} error={t('error.field')}>{control}</FormField> : control}
  </Case>;
}
function SelectCase({ state }: { state: typeof selectStates[number] }) {
  const [value, setValue] = useState(state === 'selected' ? 'selected' : '');
  const control = <Select id={`select-${state}`} value={value} onChange={event => setValue(event.target.value)} disabled={state === 'disabled'} aria-label={t('label.select')}>
    <option value="">{t('state.default')}</option><option value="selected">{t('state.selected')}</option>
  </Select>;
  return <Case id={`select-${state}`} label={<>{t('gallery.selects')} · {t(`state.${state}`)}</>}>
    {state === 'error' ? <FormField id={`select-${state}`} label={t('label.select')} error={t('error.field')}>{control}</FormField> : control}
  </Case>;
}
function CheckboxCase({ state }: { state: typeof checkboxStates[number] }) {
  const [checked, setChecked] = useState(state === 'checked');
  const [indeterminate, setIndeterminate] = useState(state === 'indeterminate');
  return <Case id={`checkbox-${state}`} label={<>{t('gallery.checkboxes')} · {t(`state.${state}`)}</>}>
    <Checkbox label={t('label.checkbox')} checked={checked} indeterminate={indeterminate} disabled={state === 'disabled'} onChange={event => { setChecked(event.target.checked); setIndeterminate(false); }} />
  </Case>;
}
function FieldCase({ state }: { state: typeof fieldStates[number] }) {
  const [value, setValue] = useState('');
  return <Case id={`field-${state}`} label={<>{t('gallery.fields')} · {t(`state.${state}`)}</>}>
    <FormField id={`field-${state}`} label={t('label.field')} help={state === 'help' ? t('help.field') : undefined} error={state === 'error' ? t('error.field') : undefined}>
      <TextInput id={`field-${state}`} value={value} onChange={event => setValue(event.target.value)} disabled={state === 'disabled'} />
    </FormField>
  </Case>;
}
export default function Gallery() {
  const [dialogOpen, setDialogOpen] = useState(false);
  const columns = [{ key: 'component', label: t('label.tableColumnComponent') }, { key: 'state', label: t('label.tableColumnState') }] as const;
  const rows = [{ id: 'button-primary-md-default', component: t('gallery.buttons'), state: t('state.default') }, { id: 'input-filled', component: t('gallery.inputs'), state: t('state.filled') }];
  return <>
    <a className="skip-link" href="#main">{t('gallery.skip')}</a>
    <div className="gallery-layout">
      <nav className="gallery-navigation" aria-label={t('gallery.navigation')}><strong>{t('gallery.navigation')}</strong>{sections.map(name => <a key={name} href={`#${name}`}>{t(`gallery.${name}`)}</a>)}</nav>
      <main id="main" className="gallery" data-testid="gallery" tabIndex={-1}>
        <header className="gallery-header"><h1>{t('gallery.title')}</h1><p>{t('gallery.description')}</p></header>
        <Section name="buttons">{variants.flatMap(variant => sizes.flatMap(size => buttonStates.map(state => <Case key={`${variant}-${size}-${state}`} id={`button-${variant}-${size}-${state}`} label={<>{t('gallery.buttons')} · {t(`button.${variant}`)} · {t(`size.${size}`)} · {t(`state.${state}`)}</>}><Button variant={variant} size={size} loading={state === 'loading'} disabled={state === 'disabled'}>{t('action.example')}</Button></Case>)))}</Section>
        <Section name="inputs">{inputStates.map(state => <InputCase key={state} state={state} />)}</Section>
        <Section name="selects">{selectStates.map(state => <SelectCase key={state} state={state} />)}</Section>
        <Section name="checkboxes">{checkboxStates.map(state => <CheckboxCase key={state} state={state} />)}</Section>
        <Section name="fields">{fieldStates.map(state => <FieldCase key={state} state={state} />)}</Section>
        <Section name="badges">{tones.map(tone => <Case key={tone} id={`badge-${tone}`} label={<>{t('gallery.badges')} · {t(`status.${tone}`)}</>}><Badge tone={tone}>{t(`status.${tone}`)}</Badge></Case>)}</Section>
        <Section name="statuses">{tones.map(tone => <Case key={tone} id={`status-${tone}`} label={<>{t('gallery.statuses')} · {t(`status.${tone}`)}</>}><StatusBadge status={tone} /></Case>)}</Section>
        <Section name="cards"><Case id="card-default" label={<>{t('gallery.cards')} · {t('state.default')}</>}><Card title={t('card.title')}><p>{t('gallery.description')}</p></Card></Case></Section>
        <Section name="tables">{(['populated', 'empty', 'loading'] as const).map(state => <Case key={state} id={`table-${state}`} label={<>{t('gallery.tables')} · {t(`state.${state}`)}</>}><DataTable columns={columns} rows={state === 'populated' ? rows : []} caption={t('label.tableCaption')} loading={state === 'loading'} emptyLabel={t('table.empty')} /></Case>)}</Section>
        <Section name="empty"><Case id="empty-basic" label={<>{t('gallery.empty')} · {t('state.empty')}</>}><EmptyState title={t('empty.title')} description={t('empty.description')} /></Case><Case id="empty-action" label={<>{t('gallery.empty')} · {t('action.example')}</>}><EmptyState title={t('empty.title')} description={t('empty.description')} action={<Button onClick={() => document.getElementById('buttons')?.scrollIntoView()}>{t('action.example')}</Button>} /></Case></Section>
        <Section name="dialog"><Case id="dialog-closed" label={<>{t('gallery.dialog')} · {t('state.closed')}</>}><Button data-testid="dialog-trigger" onClick={() => setDialogOpen(true)}>{t('action.openDialog')}</Button></Case><Case id="dialog-open" label={<>{t('gallery.dialog')} · {t('state.open')}</>}><Dialog open={dialogOpen} onOpenChange={setDialogOpen} title={t('dialog.title')} description={t('dialog.description')}><div className="gallery-dialog-actions"><Button data-testid="dialog-first" onClick={() => setDialogOpen(false)}>{t('action.confirm')}</Button><Button data-testid="dialog-last" variant="secondary" onClick={() => setDialogOpen(false)}>{t('action.cancel')}</Button></div></Dialog></Case></Section>
      </main>
    </div>
  </>;
}
