/** Pins owner-approved C-22b copy through the shared typed message API; CI runs with admin tests. */
import {expect,test} from 'bun:test';
import {t} from '@opensite/component-contracts';

test('deletion support English preserves the deadline and export commitment',()=>{
 expect(t('privacy.deletion-support')).toBe('To delete shop or customer data, contact support. We will process it within 30 days and provide an export copy.');
});
test('deletion support Chinese preserves the owner verbatim wording',()=>{
 expect(t('privacy.deletion-support-zh')).toBe('如需删除店铺或客户数据，请联系支持，我们会在 30 天内处理并提供导出副本。');
});
