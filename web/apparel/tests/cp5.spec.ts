/** CP5 browser gate; every send is intercepted, widths and locale assertions are independent. */
import { test, expect } from '@playwright/test';
import { readFileSync, mkdirSync } from 'node:fs';
import products from '../src/data/products.json' with {type:'json'};
const evidence=process.env.CP5_EVIDENCE;
if(evidence) mkdirSync(evidence,{recursive:true});
for (const locale of ['en','ar','zh']) for(const width of [390,768,1280]) {
 test(`CP5 states and prefill ${locale} ${width}`,async({page})=>{
  const t=JSON.parse(readFileSync(new URL(`../src/i18n/${locale}.json`,import.meta.url),'utf8'));
  const names=JSON.parse(readFileSync(new URL(`../src/i18n/products.${locale}.json`,import.meta.url),'utf8'));
  const prefix=locale==='en'?'':`/${locale}`;
  await page.setViewportSize({width,height:900});
  let calls=0,release:()=>void=()=>{};
  const waiting=new Promise<void>(r=>release=r);
  await page.route('**/api/rfq', async route=>{calls++; if(calls===1) await waiting; await route.fulfill({status:calls===1?503:204,contentType:'application/json',body:calls===1?'{}':''});});
  await page.goto(`${prefix}/contact/`);
  const form=page.locator('form.rfq'), button=form.locator('button[type=submit]');
  await expect(form).toHaveAttribute('data-state','Default');
  await expect(page.locator('[data-contact-media=pattern]')).toHaveCount(1);
  await expect(page.locator('[data-slot="contact/banner"]')).toHaveCount(0);
  expect(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth)).toBe(true);
  await expect(page.locator('html')).toHaveAttribute('dir',locale==='ar'?'rtl':'ltr');
  if(evidence) {
   const cookie=page.locator('[data-cookie-choice=essential]'); if(await cookie.isVisible()) await cookie.click();
   for(const img of await page.locator('main img').all()) { await img.scrollIntoViewIfNeeded(); await expect(img).toHaveJSProperty('complete',true); expect(await img.evaluate((node:HTMLImageElement)=>node.naturalWidth)).toBeGreaterThan(0); }
   await page.evaluate(async()=>{await document.fonts.ready; window.scrollTo(0,0);});
   await page.screenshot({path:`${evidence}/after-${locale}-${width}.png`,fullPage:true});
  }
  await button.click(); await expect(form).toHaveAttribute('data-state','Error');expect(calls).toBe(0);
  if(evidence) await form.screenshot({path:`${evidence}/error-${locale}-${width}.png`});
  for(const [field,value]of Object.entries({first_name:'CI',last_name:'Buyer',email:'ci@example.invalid',company:'Example',quantity_note:'100 pieces',message:'Synthetic RFQ'})) await form.locator(`[name=${field}]`).fill(value);
  await button.click();await expect(form).toHaveAttribute('data-state','Submitting');await expect(button).toBeDisabled();
  if(evidence) await form.screenshot({path:`${evidence}/submitting-${locale}-${width}.png`});
  release();await expect(form).toHaveAttribute('data-state','ServerError');await expect(form.locator('.form-status')).toHaveText(t.rfqServerError);
  if(evidence) await form.screenshot({path:`${evidence}/server-error-${locale}-${width}.png`});
  await button.click(); await expect(page).toHaveURL(`${prefix}/contact/?sent=1`);
  await expect(page.locator('[data-rfq-success]')).toBeVisible();
  if(evidence) await page.screenshot({path:`${evidence}/sent-${locale}-${width}.png`});
  const product=products.find(p=>p.slug==='hoodie-set-k02') || products[0], colour=product.images.find(i=>i.colour)?.colour || '';
  for(const type of ['quote','sample']) {
   await page.goto(`${prefix}/contact/?product=${product.slug}&type=${type}&colour=${encodeURIComponent(colour)}#rfq-form`);
   await expect(form.locator('[name=product_slug]')).toHaveValue(product.slug);
   await expect(form.locator('[data-prefill-context]')).toContainText(colour);
   await expect(form.locator('[name=quantity_note]')).toHaveValue('');
   // Prefill writes the textarea .value, not its textContent; assert the delivered value.
   const prefill = await form.locator('textarea').inputValue();
   expect(prefill).toContain(names[product.slug].name);
   expect(prefill).toContain(colour);
   if(type==='sample')expect(prefill).toContain(t.requestSample);
   expect(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth)).toBe(true);
   if(evidence)await form.screenshot({path:`${evidence}/prefill-${type}-${locale}-${width}.png`});
  }
 });
}
