/** Verifies real shared component states; depends on Playwright/axe; CI snapshots are bound to fixed container inputs. */
import { expect, test, type Page, type Locator } from '@playwright/test';
import AxeBuilder from '@axe-core/playwright';
import { CASE_IDS, variants } from '../../web/admin/src/gallery-cases';

async function catalog(page: Page) {
  await page.goto('/_gallery');
  await expect(page.getByTestId('gallery')).toBeVisible();
  await page.evaluate(async () => { await document.fonts.ready; });
  expect(await page.evaluate(() => Array.from(document.fonts)
    .filter(face => face.family.replace(/['"]/g, '') === 'Inter' && face.status === 'loaded')
    .map(face => face.weight).sort())).toEqual(['400', '500', '600']);
}

async function focusFits(control: Locator, wrapper: Locator) {
  const controlBox = await control.boundingBox();
  const snapshotBox = await wrapper.boundingBox();
  if (!controlBox || !snapshotBox) throw new Error('Focused control and snapshot must have geometry');
  const outline = await control.evaluate(element => {
    const style = getComputedStyle(element);
    return { style: style.outlineStyle, width: parseFloat(style.outlineWidth), offset: parseFloat(style.outlineOffset) };
  });
  expect(outline.style).not.toBe('none');
  expect(outline.style).not.toBe('hidden');
  expect(outline.width).toBeGreaterThan(0);
  const extent = outline.width + outline.offset;
  expect(extent).toBeGreaterThan(0);
  expect(controlBox.x - extent).toBeGreaterThanOrEqual(snapshotBox.x);
  expect(controlBox.x + controlBox.width + extent).toBeLessThanOrEqual(snapshotBox.x + snapshotBox.width);
  expect(controlBox.y - extent).toBeGreaterThanOrEqual(snapshotBox.y);
  expect(controlBox.y + controlBox.height + extent).toBeLessThanOrEqual(snapshotBox.y + snapshotBox.height);
}

test('all component states have visual baselines', async ({ page }) => {
  await catalog(page);
  expect(await page.locator('[data-gallery-case]').evaluateAll(elements => elements.map(el => el.getAttribute('data-gallery-case')))).toEqual(CASE_IDS);
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
  await expect(page).toHaveScreenshot('gallery.png', { fullPage: true });
  for (const id of CASE_IDS.filter(id => id !== 'dialog-open')) {
    await page.mouse.move(0, 0);
    await expect(page.getByTestId(`case-${id}`)).toHaveScreenshot(`${id}.png`);
  }
  for (const variant of variants) {
    const control = page.getByTestId(`case-button-${variant}-md-default`).getByRole('button');
    await control.hover();
    await expect(control).toHaveScreenshot(`button-${variant}-hover.png`);
    await page.mouse.move(0, 0);
    await control.focus();
    await page.keyboard.press('Tab');
    await page.keyboard.press('Shift+Tab');
    await expect(control).toBeFocused();
    expect(await control.evaluate(element => element.matches(':focus-visible'))).toBe(true);
    await focusFits(control, page.getByTestId(`case-button-${variant}-md-default`));
    await expect(page.getByTestId(`case-button-${variant}-md-default`)).toHaveScreenshot(`button-${variant}-focus.png`);
    await control.blur();
  }
  for (const [id, role] of [['input-default', 'textbox'], ['select-default', 'combobox'], ['checkbox-unchecked', 'checkbox'], ['field-default', 'textbox'], ['table-populated', 'region']] as const) {
    const wrapper = page.getByTestId(`case-${id}`);
    const control = wrapper.getByRole(role);
    await control.hover();
    await expect(wrapper).toHaveScreenshot(`${id}-hover.png`);
    await page.mouse.move(0, 0);
    await control.focus();
    await page.keyboard.press('Tab');
    await page.keyboard.press('Shift+Tab');
    await expect(control).toBeFocused();
    expect(await control.evaluate(element => element.matches(':focus-visible'))).toBe(true);
    await focusFits(control, wrapper);
    await expect(wrapper).toHaveScreenshot(`${id}-focus.png`);
    await control.blur();
  }
  await page.getByTestId('dialog-trigger').click();
  await expect(page.getByRole('dialog')).toBeVisible();
  await expect(page.getByRole('dialog')).toHaveScreenshot('dialog-open.png');
});

test('keyboard focus rings fit inside snapshot cases', async ({ page }) => {
  await catalog(page);
  const wrapper = page.getByTestId('case-button-primary-md-default');
  const control = wrapper.getByRole('button');
  await control.focus();
  await page.keyboard.press('Tab');
  await page.keyboard.press('Shift+Tab');
  await expect(control).toBeFocused();
  expect(await control.evaluate(element => element.matches(':focus-visible'))).toBe(true);
  await focusFits(control, wrapper);
});

test('axe rejects serious or critical violations', async ({ page }) => {
  await catalog(page);
  const report = await new AxeBuilder({ page }).analyze();
  expect(report.violations.filter(item => ['serious', 'critical'].includes(item.impact ?? ''))).toEqual([]);
  await page.getByTestId('dialog-trigger').click();
  const opened = await new AxeBuilder({ page }).analyze();
  expect(opened.violations.filter(item => ['serious', 'critical'].includes(item.impact ?? ''))).toEqual([]);
});

test('keyboard dialog traps focus, closes with Escape and restores opener', async ({ page }) => {
  await catalog(page);
  const trigger = page.getByTestId('dialog-trigger');
  await trigger.focus();
  await page.keyboard.press('Enter');
  const dialog = page.getByRole('dialog');
  await expect(dialog).toBeVisible();
  expect(await dialog.evaluate(element => element instanceof HTMLDialogElement && element.open && element.matches(':modal'))).toBe(true);
  expect(await dialog.evaluate(element => element.scrollWidth <= element.clientWidth)).toBe(true);
  await expect(page.getByTestId('dialog-first')).toBeFocused();
  await expect(dialog).toHaveScreenshot('dialog-keyboard.png');
  const focusables = dialog.getByRole('button');
  await focusables.first().focus();
  await page.keyboard.press('Shift+Tab');
  await expect(focusables.last()).toBeFocused();
  await page.keyboard.press('Tab');
  await expect(focusables.first()).toBeFocused();
  await page.keyboard.press('Escape');
  await expect(dialog).not.toBeVisible();
  await expect(trigger).toBeFocused();
});

test('queued close event cannot dismiss a reopened native dialog', async ({ page }) => {
  await catalog(page);
  await page.getByTestId('dialog-trigger').click();
  const dialog = page.getByRole('dialog');
  await expect(dialog).toBeVisible();
  await dialog.evaluate(async element => {
    const native = element as HTMLDialogElement;
    const closed = new Promise<void>(resolve => native.addEventListener('close', () => resolve(), { once: true }));
    native.close();
    native.showModal();
    await closed;
  });
  await expect(dialog).toBeVisible();
  expect(await dialog.evaluate(element => element instanceof HTMLDialogElement && element.open && element.matches(':modal'))).toBe(true);
  await page.keyboard.press('Escape');
  await expect(dialog).not.toBeVisible();
});

test('native controls expose actual loading, checked, error and help states', async ({ page }) => {
  await catalog(page);
  await expect(page.getByTestId('case-button-primary-md-loading').getByRole('button')).toBeDisabled();
  await expect(page.getByTestId('case-button-primary-md-loading').getByRole('button')).toHaveAttribute('aria-busy', 'true');
  await expect(page.getByTestId('case-checkbox-checked').getByRole('checkbox')).toBeChecked();
  await expect(page.getByTestId('case-checkbox-indeterminate').getByRole('checkbox')).toHaveAttribute('aria-checked', 'mixed');
  const field = page.getByTestId('case-field-error').getByRole('textbox', { name: 'Field example' });
  await expect(field).toHaveAttribute('aria-invalid', 'true');
  await expect(field).toHaveAttribute('aria-describedby', 'field-error-error');
  await expect(page.getByTestId('case-field-help').getByRole('textbox', { name: 'Field example' })).toHaveAttribute('aria-describedby', 'field-help-help');
  await page.getByTestId('case-input-default').getByRole('textbox').fill('Controlled input');
  await expect(page.getByTestId('case-input-default').getByRole('textbox')).toHaveValue('Controlled input');
});

test('production route and bundle exclude the component gallery', async ({ page }) => {
  await page.goto('http://127.0.0.1:4174/_gallery');
  await expect(page.getByTestId('gallery')).toHaveCount(0);
  await expect(page.getByText('OpenSite admin')).toBeVisible();
});
