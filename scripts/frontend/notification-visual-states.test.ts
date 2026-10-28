/** CI-only negative coverage for #366's required-state manifest; no renderer or product writes. */
import {expect,test} from 'bun:test';
import {existsSync} from 'node:fs';
import {missingNotificationSnapshots,requiredNotificationSnapshots} from '../../tests/frontend/notification-visual-states';
test('notification evidence requires six states including three long-field views in both viewports',()=>{
 expect(new Set(requiredNotificationSnapshots).size).toBe(16);
 expect(missingNotificationSnapshots(requiredNotificationSnapshots)).toEqual([]);
});
for(const omitted of requiredNotificationSnapshots){
 test(`required notification PNG exists: ${omitted}`,()=>{
  expect(existsSync(`tests/frontend/baselines/${omitted}`)).toBe(true);
 });
 test(`missing required notification PNG is RED: ${omitted}`,()=>{
  expect(missingNotificationSnapshots(requiredNotificationSnapshots.filter(path=>path!==omitted))).toEqual([omitted]);
 });
}
