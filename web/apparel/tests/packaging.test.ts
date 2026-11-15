/** Provenance negative controls use disposable bytes, never real deploy inputs. */
import { test, expect } from 'bun:test';
import { mkdtempSync, writeFileSync, rmSync, mkdirSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { buildReceipt, verifyBuildReceipt, receiptDigest } from '../build-receipt';
import { assertSingleRelease } from '../image-plan';
test('build provenance rejects stale head, missing receipt, forged SHA and changed HTML/Worker', () => {
 const root = mkdtempSync(join(tmpdir(), 'apparel-receipt-'));
 try {
  mkdirSync(join(root, '_worker.js')); writeFileSync(join(root, 'index.html'), 'original'); writeFileSync(join(root, '_worker.js/index.js'), 'worker');
  const sha = 'a'.repeat(40), r = buildReceipt(root, sha, '123');
  writeFileSync(join(root, 'build-receipt.json'), JSON.stringify(r));
  const trusted = receiptDigest(r);
  expect(() => verifyBuildReceipt(root, sha, trusted)).not.toThrow();
  expect(() => verifyBuildReceipt(root, 'b'.repeat(40), trusted)).toThrow();
  expect(() => verifyBuildReceipt(root, sha, '')).toThrow();
  for (const file of ['index.html', '_worker.js/index.js']) {
   writeFileSync(join(root,file), 'changed'); expect(() => verifyBuildReceipt(root, sha, trusted)).toThrow();
   writeFileSync(join(root,file), file === 'index.html' ? 'original' : 'worker');
  }
  writeFileSync(join(root,'build-receipt.json'), JSON.stringify({...r, sha:'b'.repeat(40)}));
  expect(() => verifyBuildReceipt(root, 'b'.repeat(40), trusted)).toThrow();
  rmSync(join(root,'build-receipt.json')); expect(() => verifyBuildReceipt(root,sha,trusted)).toThrow();
 } finally { rmSync(root,{recursive:true,force:true}); }
});
test('single release binds PASS, exact record and final digest, never mere text inclusion', () => {
 const key='crew-fleece-8900-46__model-03', sha='a'.repeat(64);
 const report=`结论：PASS。\nrecord：${key}；任务 test。\n/path/final.png\nSHA256：${sha}\n`;
 expect(() => assertSingleRelease(report,key,sha)).not.toThrow();
 for(const bad of [report.replace('结论：PASS','结论：HOLD'),report.replace(key,'other'),report.replace(sha,'b'.repeat(64)),report.replace('/path/final.png','/path/review.png')]) expect(() => assertSingleRelease(bad,key,sha)).toThrow();
});

test('CI receipt CLI binds checkout HEAD even when PR GITHUB_SHA names a merge commit', () => {
 const root=mkdtempSync(join(tmpdir(),'apparel-cli-'));
 try {
  mkdirSync(join(root,'_worker.js'));writeFileSync(join(root,'index.html'),'html');writeFileSync(join(root,'_worker.js/index.js'),'worker');
  const result=Bun.spawnSync(['bun','web/apparel/build-receipt.ts',root],{env:{...process.env,GITHUB_SHA:'b'.repeat(40),GITHUB_RUN_ID:'123'}});
  expect(result.exitCode).toBe(0);
  const expected=Bun.spawnSync(['git','rev-parse','HEAD']).stdout.toString().trim();
  expect(JSON.parse(require('node:fs').readFileSync(join(root,'build-receipt.json'),'utf8')).sha).toBe(expected);
 } finally {rmSync(root,{recursive:true,force:true});}
});
