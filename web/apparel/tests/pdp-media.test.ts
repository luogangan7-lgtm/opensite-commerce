/** Frozen FD1 whole/G contract: all 115 PDPs use their own accepted primary media. */
import { test, expect } from 'bun:test';
import { readFileSync } from 'node:fs';
import { pdpMediaPair } from '../src/data/gallery';
import products from '../src/data/products.json';
import digital from '../digital-sources.json';
import details from '../detail-sources.json';
import { dictionaries } from '../src/i18n';
test('every PDP pairs its own accepted W1 front with same-colour G structure', () => {
 expect(products).toHaveLength(115);
 for (const product of products) {
  const { whole, detail } = pdpMediaPair(product.images);
  expect(whole.role).toBe('front'); expect(detail.role).toBe('structure');
  expect(whole.colour).toBe(detail.colour);
  expect(digital.some(row => row.slug === product.slug && row.wave === 'W1' && row.src === whole.src)).toBe(true);
  expect(details.some(row => row.slug === product.slug && row.colour === detail.colour && row.src === detail.src)).toBe(true);
 }
});
test('missing same-colour media fails rather than substituting another SKU or colour', () => {
 expect(() => pdpMediaPair([{src:'front',role:'front',colour:'white'},{src:'detail',role:'structure',colour:'black'}])).toThrow();
});
// Figma Locale fd3/18, with FD5 ZH correction (2026-11-14).
const frozenCaptions = {
  "en": "AI-generated illustration; actual details are subject to the confirmed sample.",
  "ar": "رسم توضيحي مولّد بالذكاء الاصطناعي؛ التفاصيل الفعلية وفق العينة المعتمدة.",
  "zh": "AI 生成示意图，实际细节以确认样品为准",
  "es": "Ilustración generada por IA; los detalles reales se ajustarán a la muestra confirmada.",
  "fr": "Illustration générée par IA ; les détails réels dépendent de l’échantillon confirmé.",
  "de": "KI-generierte Illustration; die tatsächlichen Details richten sich nach dem bestätigten Muster.",
  "pt": "Ilustração gerada por IA; os detalhes reais seguem a amostra confirmada.",
  "ru": "Иллюстрация создана ИИ; фактические детали определяются утверждённым образцом."
};
test('all eight dictionaries own the caption and frozen FD5 Chinese wording', () => {
 for (const [locale, caption] of Object.entries(frozenCaptions)) expect(dictionaries[locale as keyof typeof dictionaries].pdpDetailCaption).toBe(caption);
 expect(dictionaries.zh.pdpDetailCaption).toBe('AI 生成示意图，实际细节以确认样品为准');
});
test('PDP replaces generic craft illustrations with FIT pair', () => {
 const view=readFileSync(new URL('../src/views/Product.astro',import.meta.url),'utf8');
 expect(view).not.toContain('pdp/craft_'); expect(view).toContain('data-pdp-media');
 expect(view).toContain('object-fit: contain');
});
