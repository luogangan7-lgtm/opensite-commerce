// Serial source-bound image jobs; production profile remains closed.
// Depends on source-guard and canonical table. Called by legacy entry/tests; one send, no retry.
// Every attempted ID is ledgered before UI work; all ledger statuses suppress resending.
const fs = require('node:fs'), path = require('node:path'), crypto = require('node:crypto');
const CONSUMED = path.resolve(__dirname, '../../wash-consumed-exact-ids.json');
const STOP = /^(CAP|LOGOUT|CAPTCHA)$/;
const hash = b => crypto.createHash('sha256').update(b).digest('hex');
const normalize = s => s.replace(/\s+/g, ' ').trim();
// Only prompt_send is sendable; internal columns and legacy job.prompt never supply text.
function cleanPromptSend(value) {
  if (typeof value !== 'string' || !value.trim()) throw Error('PROMPT_SEND_EMPTY');
  if (/hold|slot|\/Volumes/i.test(value)) throw Error('PROMPT_SEND_INTERNAL');
  return value.trim();
}
function prepareJobs(jobs, slotsFile = path.join(__dirname, '../slots.tsv')) {
  if (!Array.isArray(jobs)) throw Error('JOBS_NOT_ARRAY');
  // This project table has one record per line; reject ambiguous/malformed TSV.
  const lines = fs.readFileSync(slotsFile, 'utf8').replace(/[\r\n]+$/, '').split(/\r?\n/);
  const header = lines.shift().split('\t'), id = header.indexOf('slot_id'), prompt = header.indexOf('prompt_send');
  if (id < 0 || prompt < 0) throw Error('PROMPT_SEND_COLUMN_MISSING');
  const rows = new Map();
  for (const line of lines) {
    const cells = line.split('\t');
    if (cells.length !== header.length || rows.has(cells[id])) throw Error('INVALID_SLOTS_TSV');
    rows.set(cells[id], cells[prompt]);
  }
  return jobs.map(job => {
    const id = (job.id || '').replace(/-supplement[\w-]*$/, '');
    const prompt_send = cleanPromptSend(rows.get(id));
    return {...job, prompt_send, prompt: prompt_send}; // retain existing card prompt-hash binding
  });
}
function slotTimeoutMs(env = process.env) {
  const seconds = env.SLOT_TIMEOUT_SECONDS === undefined ? 360 : Number(env.SLOT_TIMEOUT_SECONDS);
  if (!Number.isFinite(seconds) || seconds <= 0) throw Error('INVALID_SLOT_TIMEOUT');
  return seconds * 1000;
}
async function guard(page) {
  const reason = await page.evaluate(() => {
    const t = document.body.innerText;
    if (/usage cap|reached (the|your) .{0,40}limit|too many (requests|images)|limit (resets|will reset)|you.ve hit.{0,30}limit|come back (later|in|at|after)|(已达到|达到了).{0,20}(上限|限额|限制)|使用上限|稍后再试|请在.{0,30}后(再试|重试)/i.test(t)) return 'CAP';
    if (document.querySelector('iframe[src*="captcha"], iframe[src*="challenges.cloudflare"], .g-recaptcha, #challenge-running') || /verify you are human|checking your browser|complete the captcha|验证您是人类|人机验证|验证码/i.test(t)) return 'CAPTCHA';
    if ([...document.querySelectorAll('button,a')].some(e => /^(log in|sign in|登录|登入)$/i.test(e.innerText.trim()))) return 'LOGOUT';
    return null;
  });
  if (reason) throw Error(reason);
}
// Real profiles remain closed; only an internally minted loopback fixture capability is supported.
async function one(page, job, options = {}) {
  const guard = require('./source-guard.cjs');
  guard.assertProfile(options.profile);
  cleanPromptSend(job.prompt_send);
  const handle = guard.preflight(job, options.profile);
  return guard.execute(page, handle, options.profile);
}
function readSeen(ledger, consumed) {
  const ids = new Set();
  if (fs.existsSync(ledger)) for (const line of fs.readFileSync(ledger, 'utf8').split('\n').filter(Boolean)) {
    const row = JSON.parse(line); if (typeof row.id !== 'string') throw Error('INVALID_LEDGER'); ids.add(row.id);
  }
  const data = JSON.parse(fs.readFileSync(consumed, 'utf8'));
  if (!Array.isArray(data.exact_ids) || data.exact_ids.some(id => typeof id !== 'string')) throw Error('INVALID_CONSUMED');
  for (const id of data.exact_ids) ids.add(id);
  return ids;
}
async function run(jobs, options = {}) {
  jobs = prepareJobs(jobs, options.slotsFile);
  const sourceGuard = require('./source-guard.cjs');
  sourceGuard.assertProfile(options.profile);
  for (const job of jobs) sourceGuard.preflight(job, options.profile);
  const dir = options.dir || __dirname, ledger = path.join(dir, 'ledger.jsonl');
  const seen = readSeen(ledger, options.consumed || CONSUMED);
  if (!Array.isArray(jobs)) throw Error('JOBS_NOT_ARRAY');
  for (const j of jobs) {
    if (!j || typeof j.id !== 'string' || !j.id || !Array.isArray(j.refs) || j.refs.length !== 2 || typeof j.prompt !== 'string' || !j.prompt.trim()) throw Error('INVALID_JOB');
    if (!seen.has(j.id)) for (const ref of j.refs) if (typeof ref !== 'string' || !fs.statSync(ref).isFile()) throw Error('INVALID_REFERENCE');
  }
  fs.mkdirSync(path.join(dir, 'out'), {recursive: true});
  const log = row => { const fd = fs.openSync(ledger, 'a'); try {fs.writeSync(fd, JSON.stringify(row) + '\n'); fs.fsyncSync(fd);} finally {fs.closeSync(fd);} };
  let code = 0;
  for (const job of jobs) {
    if (seen.has(job.id)) continue;
    try { options.checkSlot(job); } catch(e) { log({id:job.id,status:'REFUSED',error:e.message,time:new Date().toISOString()}); code = code || 2; continue; }
    seen.add(job.id);
    const row = {id: job.id, time: new Date().toISOString(), url: null, sha256: null, status: 'started'};
    log(row); // crash/ambiguous send remains consumed, including process death
    let page;
    try {
      options.reserve(job); // consume attempts before any browser work, including failed/unknown starts
      page = await options.newPage();
      const result = await one(page, job, {profile: options.profile});
      const ext = /png/i.test(result.type) ? 'png' : /webp/i.test(result.type) ? 'webp' : /jpeg/i.test(result.type) ? 'jpg' : 'img';
      row.output = path.join(dir, 'out', hash(job.id) + '.' + ext);
      fs.writeFileSync(row.output, result.bytes, {flag: 'wx'});
      row.sha256 = hash(fs.readFileSync(row.output));
      if (row.sha256 !== result.sha256) throw Error('PERSISTED_BYTES_MISMATCH');
      row.status = 'ok';
    } catch (e) {
      row.status = e.outcome === 'UNKNOWN' ? 'UNKNOWN' : STOP.test(e.message) ? e.message.toLowerCase() : 'failed'; row.error = e.message;
      if (e.quarantine?.length) {
        row.quarantine = path.join(dir, 'out', hash(job.id) + '.quarantine');
        fs.writeFileSync(row.quarantine, e.quarantine, {flag: 'wx'});
      }
      code = STOP.test(e.message) ? 3 : code || 2;
    } finally {
      row.url = page ? page.url() : null; row.time = new Date().toISOString(); log(row);
      if (page) await page.close().catch(() => {});
    }
    if (code === 3) break;
  }
  return code;
}
async function main() { throw Error('USE_CARD_BOUND_RUN_ENTRY'); }
module.exports = {one, run, guard, readSeen, prepareJobs, slotTimeoutMs};
if (require.main === module) main().catch(e => { console.error(e.message); process.exitCode = 1; });
