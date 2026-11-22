// Uses only a newly-created uniquely named test stream; never touches factory logs/traces.
import assert from 'node:assert/strict';
import {readFile} from 'node:fs/promises';
import {randomUUID} from 'node:crypto';
import path from 'node:path';
import {fileURLToPath} from 'node:url';
const root=path.resolve(path.dirname(fileURLToPath(import.meta.url)),'..');
const config=Object.fromEntries((await readFile(process.env.OPENOBSERVE_CONFIG||path.join(root,'services/openobserve/config/openobserve.env'),'utf8')).split(/\r?\n/).filter(l=>l&&!l.startsWith('#')&&l.includes('=')).map(l=>[l.slice(0,l.indexOf('=')),l.slice(l.indexOf('=')+1)]));
assert.ok(config.ZO_ROOT_USER_EMAIL&&config.ZO_ROOT_USER_PASSWORD,'local credentials required');
const base=process.env.OPENOBSERVE_TEST_URL||'http://127.0.0.1:5080';
const stream='factory_measurements_test_'+randomUUID().replaceAll('-','');
const headers={Authorization:'Basic '+Buffer.from(config.ZO_ROOT_USER_EMAIL+':'+config.ZO_ROOT_USER_PASSWORD).toString('base64'),'Content-Type':'application/json'};
const hour=Math.floor(Date.now()/3600000)*3600000,previous=hour-3600000;
async function call(method,route,body){const r=await fetch(base+route,{method,headers,body:body===undefined?undefined:JSON.stringify(body),signal:AbortSignal.timeout(15000)});const text=await r.text();assert.ok(r.ok,`${method} ${route}: HTTP ${r.status} ${text.slice(0,500)}`);return text?JSON.parse(text):{};}
async function records(){const data=await call('POST','/api/default/_search?use_cache=false',{query:{sql:`SELECT id, amount FROM "${stream}" ORDER BY id`,start_time:previous*1000,end_time:(hour+3600000)*1000,from:0,size:100}});assert.equal(data.is_partial||false,false);return data.hits||[];}
async function wait(fn,label,attempts=90){for(let i=0;i<attempts;i++){if(await fn())return;await new Promise(r=>setTimeout(r,1000));}throw new Error(label+' timed out');}
console.log('Disposable verification stream: '+stream);
let created=false;
try{
 const inserted=await call('POST',`/api/default/${stream}/_json`,[{_timestamp:(previous+1000)*1000,id:'replace',amount:10},{_timestamp:(hour+1000)*1000,id:'neighbor',amount:7}]);created=true;
 assert.ok(inserted.status?.every(s=>s.failed===0),'ingestion reported failed records');
 await wait(async()=> (await records()).length===2,'initial visibility');
 console.log('Waiting for normal file persistence; no node flush requested.');
 await wait(async()=>{const partition=await call('POST','/api/default/_search_partition?type=logs',{sql:`SELECT id FROM "${stream}" ORDER BY _timestamp`,start_time:previous*1000,end_time:(hour+3600000)*1000});return partition.records===2;},'file-list persistence',900);
 const deletion=await call('DELETE',`/api/default/streams/${stream}/data_by_time_range?type=logs&start=${previous*1000}&end=${hour*1000}`);
 assert.ok(deletion.id,'delete response needs a job identity');
 await wait(async()=> (await call('GET',`/api/default/streams/${stream}/data_by_time_range/status/${deletion.id}`)).status==='Completed','deletion');
 await wait(async()=>{const rows=await records();return rows.length===1&&rows[0].id==='neighbor';},'deleted range visibility');
 const remaining=await records();assert.deepEqual(remaining.map(r=>r.id),['neighbor'],'hour deletion must remove target and preserve adjacent hour');
 const replacement=await call('POST',`/api/default/${stream}/_json`,[{_timestamp:(previous+1000)*1000,id:'replace',amount:3}]);assert.ok(replacement.status?.every(s=>s.failed===0));
 await wait(async()=>{const rows=await records();return rows.length===2&&rows.find(r=>r.id==='replace')?.amount===3;},'replacement');
 console.log('PASS: actual pinned OpenObserve hourly delete/status/reingest preserves adjacent-hour records and replaces value.');
}finally{
 if(created){try{await call('DELETE',`/api/default/streams/${stream}?type=logs&delete_all=true`);console.log('Disposable stream deletion requested.');}catch(e){console.error('Disposable stream cleanup pending: '+e.message);}}
}
