/** Published catalog view; generated products are sole product facts; pages never read source material. */
import products from './products.json';
export { products };
export type Product = Omit<(typeof products)[number], 'images'> & { images: (import('./gallery').GalleryImage & { sha256_16?: string; web_sha256?: string })[] };
export const quote = 'Confirmed in quote';
export const categories: Record<string, string> = { apparel: 'Cotton T-shirts', 'cotton430-hoodie': 'Cotton fleece hoodies', 'crew-fleece': 'Fleece sweatshirts', 'hoodie-set': 'Hoodies & sets', 'pullover-hoodie': 'Pullover hoodies', 'zip-ups-jackets': 'Zip-ups & Jackets' };
