/** Owns typed shared English component copy; no framework dependency; UI/admin call t. Missing keys fail compilation. */
export const messages = {
  'app.ready': 'OpenSite admin',
  'dialog.close': 'Close dialog',
  'common.loading': 'Loading…',
  'table.empty': 'No component states to display',
  'status.neutral': 'Neutral',
  'status.success': 'Success',
  'status.warning': 'Warning',
  'status.danger': 'Danger',
  'status.info': 'Information'
} as const;

export type MessageKey = keyof typeof messages;
export function t(key: MessageKey): string { return messages[key]; }
