/** C20 DATA audit regression; consumes the three C19 catalog colour cases. */
import {test,expect} from 'bun:test';
import {catalogColours} from '../catalog-colours';
for(const slug of ['hoodie-set-k24-tz2803-5','hoodie-set-k25-tz2803-6']) {
 test(`${slug}: slash-separated Chinese colour names remain whole`,()=>{
  expect(catalogColours('灰/黑/藏青')).toEqual(['灰','黑','藏青']);
 });
}
test('hoodie-set-k21-70046: rear-view annotation is not a front colour',()=>{
 expect(catalogColours(['light grey-green/sage','black','navy','khaki/sand','light grey (back view, img 5)'])).toEqual(['light grey-green/sage','black','navy','khaki/sand']);
});
