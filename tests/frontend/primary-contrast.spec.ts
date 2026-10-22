/** Browser counterpart to the token contract; production gallery CSS in pinned CI.
 * Samples actual computed states and seeks any CSS transitions, without timing sleeps. */
import { expect, test } from '@playwright/test';
import { contrastRatio } from '../../scripts/frontend/check-contrast';

for (const reducedMotion of ['no-preference', 'reduce'] as const) {
  test(`primary contrast across states and frames (${reducedMotion})`, async ({ page }) => {
    await page.emulateMedia({ reducedMotion });
    await page.goto('/_gallery');
    const button = page.getByTestId('case-button-primary-md-default').getByRole('button');
    await expect(button).toBeVisible();
    for (const state of ['idle', 'hover', 'focus', 'active', 'pending', 'disabled', 'focusable-disabled', 'idle-again']) {
      await button.evaluate((element, state) => {
        const button = element as HTMLButtonElement;
        button.disabled = state === 'pending' || state === 'disabled';
        button.setAttribute('aria-busy', String(state === 'pending'));
        button.setAttribute('aria-disabled', String(state === 'focusable-disabled'));
      }, state);
      if (state === 'hover' || state === 'active') await button.hover();
      else await page.mouse.move(0, 0);
      if (state === 'focus' || state === 'focusable-disabled') await button.focus();
      if (state === 'active') await page.mouse.down();
      const frames = await button.evaluate(element => {
        const frames = [];
        for (const fraction of [0, 0.25, 0.5, 0.75, 1]) {
          for (const animation of element.getAnimations()) {
            animation.pause();
            animation.currentTime = Number(animation.effect!.getTiming().duration) * fraction;
          }
          const style = getComputedStyle(element);
          frames.push({ color: style.color, background: style.backgroundColor,
            opacity: style.opacity, transition: style.transitionProperty });
        }
        return frames;
      });
      if (state === 'active') await page.mouse.up();
      for (const frame of frames) {
        const hex = (rgb: string) => {
          expect(rgb, state).toMatch(/^rgb\(\d+, \d+, \d+\)$/);
          return '#' + rgb.match(/\d+/g)!.map(channel => Number(channel).toString(16).padStart(2, '0')).join('');
        };
        expect(hex(frame.color), state).toBe('#ffffff');
        expect(contrastRatio(hex(frame.color), hex(frame.background)), state).toBeGreaterThanOrEqual(4.5);
        expect(frame.opacity, state).toBe('1');
        expect(frame.transition, state).toBe('none');
      }
    }
  });
}
