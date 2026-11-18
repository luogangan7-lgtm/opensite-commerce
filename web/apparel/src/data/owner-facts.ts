/** Owner MOQ facts; consumed by PDP, About ways and distribution gate. No price/lead-time claims.
 * Authority: docs/apparel-b2b/about-copy.md:22,58 and owner-input.md;
 * root authorization msg_b06d4d9e313e, copy approval msg_8cd0a266743a (2026-11-15);
 * About ways-only extension msg_d3aed81653ee (2026-11-15).
 * Wholesale may mix styles; blank/decorated mix colours/sizes within each style. */
import { dictionaries, type Locale } from '../i18n';
export const ownerMoq = { ready: 100, custom: 1000 } as const;
export const pdpMoq: Record<Locale, string> = {
  en: `MOQ ${ownerMoq.ready} pcs · mix colours & sizes`,
  zh: `${ownerMoq.ready} 件起订 · 同款可混色混码`,
  es: `Pedido mínimo: ${ownerMoq.ready} piezas · combina colores y tallas`,
  fr: `Minimum de commande : ${ownerMoq.ready} pièces · couleurs et tailles panachées`,
  de: `Mindestbestellmenge: ${ownerMoq.ready} Stück · Farben und Größen kombinierbar`,
  pt: `Pedido mínimo: ${ownerMoq.ready} peças · combine cores e tamanhos`,
  ru: `Минимальный заказ: ${ownerMoq.ready} шт. · можно сочетать цвета и размеры`,
  ar: `الحد الأدنى للطلب ${ownerMoq.ready} قطعة · يمكن مزج الألوان والمقاسات`,
};
/** Spec-row value: the row's own label already says MOQ, so only the quantity and mixing rule appear. */
export const pdpMoqValue: Record<Locale, string> = {
  en: `${ownerMoq.ready} pcs · mix colours & sizes`,
  zh: `${ownerMoq.ready} 件 · 同款可混色混码`,
  es: `${ownerMoq.ready} piezas · combina colores y tallas`,
  fr: `${ownerMoq.ready} pièces · couleurs et tailles panachées`,
  de: `${ownerMoq.ready} Stück · Farben und Größen kombinierbar`,
  pt: `${ownerMoq.ready} peças · combine cores e tamanhos`,
  ru: `${ownerMoq.ready} шт. · можно сочетать цвета и размеры`,
  ar: `${ownerMoq.ready} قطعة · يمكن مزج الألوان والمقاسات`,
};
export const aboutWays = ['01', '02', '03', '04'] as const;
/** Cards and the order band use their own approved wording for the owner MOQ. */
export const shopCardMoq = (locale: Locale): string =>
  dictionaries[locale]['cp-shop/figma-0004'].replace('{moq}', String(ownerMoq.ready));
export const shopReadyMoq = (locale: Locale): string => pdpMoq[locale].split(' · ')[0];
const grouping: Record<Locale, string> = { en: ',', zh: ',', es: '.', fr: ' ', de: '.', pt: '.', ru: ' ', ar: ',' };
export function aboutMoqCopy(locale: Locale, way: typeof aboutWays[number]): string {
  const quantity = way === '04' ? ownerMoq.custom : ownerMoq.ready;
  const amount = quantity.toLocaleString('en-US').replaceAll(',', grouping[locale]);
  return dictionaries[locale][`about.ways-body-${way}`].replace('{moq}', amount);
}

/** Listing quantities use the same explicit grouping as About across Node/Bun ICU builds. */
export function shopCustomMoq(locale: Locale): string {
  const amount = ownerMoq.custom.toLocaleString('en-US').replaceAll(',', grouping[locale]);
  return dictionaries[locale]['cp-shop/custom'].replace('{moq}', amount);
}
