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

// Native select text can retain pre-font layout metrics after document.fonts.ready.
// Observe creation, rather than only checking the fonts immediately before capture.
test('native gallery selects are created only after Inter is loaded', async ({ page }) => {
  // Keep a cold font response pending while the gallery module is evaluated.
  let releaseFonts!: () => void;
  const fontsReleased = new Promise<void>(resolve => { releaseFonts = resolve; });
  await page.route('**/*.woff2', async route => { await fontsReleased; await route.continue(); });
  const galleryModule = page.waitForResponse(response => /\/assets\/gallery-[^/]+\.js$/.test(response.url()));
  await page.addInitScript(() => {
    const observations: boolean[] = [];
    Object.assign(window, { gallerySelectFonts: observations });
    new MutationObserver(records => {
      for (const record of records) for (const node of record.addedNodes) {
        if (!(node instanceof Element)) continue;
        const selects = [...node.querySelectorAll('select'), ...(node.matches('select') ? [node] : [])];
        for (const select of selects) {
          if (select.closest('[data-testid="gallery"]')) {
            observations.push(document.fonts.check('400 16px Inter', 'Default Selected'));
          }
        }
      }
    }).observe(document, { childList: true, subtree: true });
  });
  await page.goto('/_gallery', { waitUntil: 'domcontentloaded' });
  await (await galleryModule).finished();
  await page.evaluate(() => new Promise<void>(resolve => {
    requestAnimationFrame(() => requestAnimationFrame(() => resolve()));
  }));
  releaseFonts();
  await expect(page.getByTestId('gallery')).toBeVisible();
  await page.evaluate(async () => { await document.fonts.ready; });
  const observations = await page.evaluate(() =>
    (window as unknown as { gallerySelectFonts: boolean[] }).gallerySelectFonts);
  expect(observations).toHaveLength(4);
  expect(observations).toEqual([true, true, true, true]);
});

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

test('every keyboard focus ring fits on all four screenshot edges', async ({ page }, testInfo) => {
  await catalog(page);
  const cases = [
    ...variants.map(variant => [`button-${variant}-md-default`, 'button'] as const),
    ['input-default', 'textbox'], ['select-default', 'combobox'], ['checkbox-unchecked', 'checkbox'],
    ['field-default', 'textbox'], ['table-populated', 'region']
  ] as const;
  const evidence = [];
  for (const [id, role] of cases) {
    const wrapper = page.getByTestId(`case-${id}`);
    const control = wrapper.getByRole(role);
    await control.focus();
    await page.keyboard.press('Tab');
    await page.keyboard.press('Shift+Tab');
    await expect(control).toBeFocused();
    expect(await control.evaluate(element => element.matches(':focus-visible'))).toBe(true);
    await focusFits(control, wrapper);
    const padding = await wrapper.locator('.gallery-example').evaluate(element => {
      const style = getComputedStyle(element);
      return [style.paddingTop, style.paddingRight, style.paddingBottom, style.paddingLeft].map(parseFloat);
    });
    const extent = await control.evaluate(element => {
      const style = getComputedStyle(element);
      return parseFloat(style.outlineWidth) + parseFloat(style.outlineOffset);
    });
    // Capture padding tracks the outline tokens, with clear space beyond every painted edge.
    for (const edge of padding) expect(edge, `${id}: all four capture insets`).toBeGreaterThan(extent);
    evidence.push({ id, extent, padding, control: await control.boundingBox(), screenshot: await wrapper.boundingBox() });
    await control.blur();
  }
  await testInfo.attach('focus-ring-bounds', { body: JSON.stringify(evidence, null, 2), contentType: 'application/json' });
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
  const mixed = page.getByTestId('case-checkbox-indeterminate').getByRole('checkbox');
  await expect(mixed).toBeChecked({ indeterminate: true });
  await expect(mixed).toMatchAriaSnapshot('- checkbox "Checkbox example" [checked=mixed]');
  await expect(mixed).not.toHaveAttribute('aria-checked');
  const field = page.getByTestId('case-field-error').getByRole('textbox', { name: 'Field example' });
  await expect(field).toHaveAttribute('aria-invalid', 'true');
  await expect(field).toHaveAttribute('aria-describedby', 'field-error-error');
  await expect(page.getByTestId('case-field-help').getByRole('textbox', { name: 'Field example' })).toHaveAttribute('aria-describedby', 'field-help-help');
  await page.getByTestId('case-input-default').getByRole('textbox').fill('Controlled input');
  await expect(page.getByTestId('case-input-default').getByRole('textbox')).toHaveValue('Controlled input');
});

test('production route excludes the component gallery', async ({ page }) => {
  await page.route('**/api/v1/auth/session',route=>route.fulfill({status:401,contentType:'application/problem+json',body:JSON.stringify({type:'about:blank',title:'Unauthenticated',status:401,code:'unauthenticated'})}));
  await page.goto('http://127.0.0.1:4174/_gallery');
  await expect(page.getByTestId('gallery')).toHaveCount(0);
  await expect(page.getByText('OpenSite admin')).toBeVisible();
});

// Temporary CI fault probes: each missing required font must stop gallery capture.
for (const weight of ['400', '500']) {
  test(`fault probe: unavailable Inter ${weight} cannot reach a gallery capture`, async ({ page }) => {
    await page.route(new RegExp(`inter-latin-${weight}-normal-.*\\.woff2$`), route => route.abort('failed'));
    await catalog(page);
    await expect(page).toHaveScreenshot('gallery.png', { fullPage: true });
  });
}
