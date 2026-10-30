/** Build-time dictionaries only; pages/forms consume keys, product facts stay in the catalog. */
import en from './en.json';
import es from './es.json';
import fr from './fr.json';
import de from './de.json';
import pt from './pt.json';
import ru from './ru.json';
import ar from './ar.json';
import productsEn from './products.en.json';
import productsEs from './products.es.json';
import productsFr from './products.fr.json';
import productsDe from './products.de.json';
import productsPt from './products.pt.json';
import productsRu from './products.ru.json';
import productsAr from './products.ar.json';
export const locales = ['en', 'es', 'fr', 'de', 'pt', 'ru', 'ar'] as const;
export type Locale = typeof locales[number];
export type Key = keyof typeof en;
export const dictionaries: Record<Locale, Record<Key, string>> = { en, es, fr, de, pt, ru, ar };
export const languageNames: Record<Locale, string> = { en: 'English', es: 'Español', fr: 'Français', de: 'Deutsch', pt: 'Português', ru: 'Русский', ar: 'العربية' };
type Translation = { name: string; fabric_weight_fit: string; colourway_note: string };
export const productText: Record<Locale, Record<string, Translation>> = { en: productsEn, es: productsEs, fr: productsFr, de: productsDe, pt: productsPt, ru: productsRu, ar: productsAr };
export function pageLocale(url: URL): Locale {
  const prefix = url.pathname.split('/')[1];
  return locales.find(locale => locale === prefix) || 'en';
}
export function pagePath(url: URL): string {
  return url.pathname.replace(/^\/(?:es|fr|de|pt|ru|ar)(?=\/|$)/, '') || '/';
}
