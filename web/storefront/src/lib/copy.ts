/** Responsibility: typed UI copy; dependencies: none; callers: storefront templates/islands.
 * Published merchant content stays in the release; this dictionary only owns interface labels.
 */
const english = {
  storefront: 'Storefront', home: 'Home', products: 'Products', navigation: 'Main navigation',
  skip: 'Skip to content', productDetails: 'Product details', backProducts: 'Back to products',
  emptyProducts: 'No products available', emptyDescription: 'Check back for new products.',
  notFound: 'Page not found', notFoundDescription: 'This page is unavailable. Return home to continue browsing.',
  unavailable: 'Store temporarily unavailable', unavailableDescription: 'We could not load this store. Try again in a moment.',
  retry: 'Try again', returnHome: 'Return home',
} as const;
export type CopyKey = keyof typeof english;
export type StorefrontDictionary = Record<CopyKey, string>;
const dictionaries: Record<'en', StorefrontDictionary> = { en: english };
export function copy(_locale = 'en'): StorefrontDictionary { return dictionaries.en; }
