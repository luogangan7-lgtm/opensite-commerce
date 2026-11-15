/** Owner MOQ facts; consumed by PDP and distribution gate. No price/lead-time claims.
 * Authority: docs/apparel-b2b/about-copy.md:22,58 and owner-input.md;
 * root authorization msg_b06d4d9e313e, copy approval msg_8cd0a266743a (2026-11-15).
 * Wholesale may mix styles; blank/decorated mix colours/sizes within each style. */
import type { Locale } from '../i18n';
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
