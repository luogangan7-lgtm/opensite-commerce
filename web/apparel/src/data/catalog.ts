/** Published catalog view; generated products are sole product facts; pages never read source material. */
import products from './products.json';
export { products };
export type Product = (typeof products)[number];
export const quote = 'Confirmed in quote';
export const categories: Record<string, string> = { 'crew-fleece': 'Fleece sweatshirts', apparel: 'Cotton T-shirts', 'cotton430-hoodie': 'Cotton fleece hoodies', 'hoodie-set': 'Hoodies & sets', 'pullover-hoodie': 'Pullover hoodies' };
