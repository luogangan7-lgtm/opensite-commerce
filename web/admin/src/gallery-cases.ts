/** Defines stable gallery state IDs; depends on no runtime; used by gallery and gates. IDs never imply business data. */
export const variants = ['primary', 'secondary', 'danger', 'ghost'] as const;
export const sizes = ['sm', 'md', 'lg'] as const;
export const buttonStates = ['default', 'loading', 'disabled'] as const;
export const tones = ['neutral', 'success', 'warning', 'danger', 'info'] as const;
export const inputStates = ['default', 'filled', 'disabled', 'error'] as const;
export const selectStates = ['default', 'selected', 'disabled', 'error'] as const;
export const checkboxStates = ['unchecked', 'checked', 'disabled', 'indeterminate'] as const;
export const fieldStates = ['default', 'help', 'error', 'disabled'] as const;

export const CASE_IDS = [
  ...variants.flatMap(variant => sizes.flatMap(size => buttonStates.map(state => `button-${variant}-${size}-${state}`))),
  ...inputStates.map(state => `input-${state}`),
  ...selectStates.map(state => `select-${state}`),
  ...checkboxStates.map(state => `checkbox-${state}`),
  ...fieldStates.map(state => `field-${state}`),
  ...tones.map(tone => `badge-${tone}`), ...tones.map(tone => `status-${tone}`),
  'card-default', 'table-populated', 'table-empty', 'table-loading', 'empty-basic', 'empty-action', 'dialog-closed', 'dialog-open',
] as const;

