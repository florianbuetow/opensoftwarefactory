// Actual isolated service processes, not a substitute for live broker/OpenObserve checks.
import assert from 'node:assert/strict';
import {spawn, execFileSync} from 'node:child_process';
import {mkdtemp, rm, mkdir, readFile} from 'node:fs/promises';
import {tmpdir} from 'node:os';
import net from 'node:net';
import {randomUUID} from 'node:crypto';
import path from 'node:path';
import {fileURLToPath} from 'node:url';
const root=path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const directory=await mkdtemp(path.join(tmpdir(),'factory-measurements-'));
const children=[];
const abort=new AbortController();
process.once('SIGTERM',()=>abort.abort());
const realObserve=process.env.VERIFY_REAL_OPENOBSERVE==='1';
const observeURL=process.env.OPENOBSERVE_TEST_URL||'http://127.0.0.1:5080';
const observeConfig=process.env.OPENOBSERVE_CONFIG||path.join(root,'services/openobserve/config/openobserve.env');
const testStream='factory_measurements_test_'+randomUUID().replaceAll('-','');
let observeHeaders={};
if(realObserve){
 const values=Object.fromEntries((await readFile(observeConfig,'utf8')).split(/\r?\n/).filter(l=>l.includes('=')&&!l.startsWith('#')).map(l=>[l.slice(0,l.indexOf('=')),l.slice(l.indexOf('=')+1)]));
 observeHeaders={Authorization:'Basic '+Buffer.from(values.ZO_ROOT_USER_EMAIL+':'+values.ZO_ROOT_USER_PASSWORD).toString('base64')};
 console.log('Actual OpenObserve test stream: '+testStream);
}
const delay=ms=>new Promise(r=>setTimeout(r,ms));
async function port(){const s=net.createServer();await new Promise((r,j)=>{s.once('error',j);s.listen(0,'127.0.0.1',r);});const p=s.address().port;await new Promise(r=>s.close(r));return p;}
async function request(base,method,route,body,headers={},expected=200){
 const r=await fetch(base+route,{method,headers:{'Content-Type':'application/json',...headers},body:body===undefined?undefined:JSON.stringify(body),signal:AbortSignal.timeout(10000)});
 const text=await r.text();assert.equal(r.status,expected,`${method} ${route}: ${text}`);return {data:text?JSON.parse(text):null,etag:r.headers.get('etag')};
}
async function eventually(fn, label, attempts=100, interval=100) {
 let last;
 for (let i=0; i<attempts; i++) { if(abort.signal.aborted) throw new Error('verification cancelled'); try { const result=await fn(); if(result) return result; } catch(error) { last=error; } await delay(interval); }
 throw new Error(label+': '+(last?.message||'timed out'));
}
async function start(binary,args,environment,base){
 const c=spawn(binary,args,{env:{...process.env,OTEL_EXPORTER_OTLP_TRACES_ENDPOINT:'',...environment},stdio:['ignore','pipe','pipe']});let output='';
 for(const stream of [c.stdout,c.stderr])stream.on('data',b=>{output=(output+b).slice(-8000);});children.push(c);
 for(let i=0;i<100;i++){if(c.exitCode!==null)throw new Error(`Service exited: ${output}`);try{if((await fetch(base+'/healthz',{signal:AbortSignal.timeout(500)})).ok)return;}catch{}await delay(100);}
 throw new Error(`Service did not start: ${output}`);
}
try{
 for(const service of ['blobstore','evaluation'])execFileSync('go',['build','-o',path.join(directory,service),'.'],{cwd:path.join(root,'services',service),stdio:'inherit'});
 const bp=await port(),ep=await port(),blobs=`http://127.0.0.1:${bp}`,evaluation=`http://127.0.0.1:${ep}`;
 await mkdir(path.join(directory,'state'));
 await start(path.join(directory,'blobstore'),['-data',path.join(directory,'blobs'),'-addr',`127.0.0.1:${bp}`],{},blobs);
 await start(path.join(directory,'evaluation'),[],{BLOBSTORE_URL:blobs,EVALUATION_ADDR:`127.0.0.1:${ep}`,EVALUATION_LOCK_PATH:path.join(directory,'state/writer.lock'),OPENOBSERVE_CONFIG:realObserve?observeConfig:path.join(directory,'absent.env'),OPENOBSERVE_URL:realObserve?observeURL:'http://127.0.0.1:1',EVALUATION_PROJECTION_STREAM:realObserve?testStream:'factory_measurements',ORCHESTRATOR_URL:'http://127.0.0.1:1'},evaluation);
 async function graph(expected,currency='USD'){
  const params=new URLSearchParams({metric:'cost',group_by:'model',from:'2026-11-20T09:00:00Z',to:'2026-11-20T11:00:00Z',currency});
  return eventually(async()=>{const result=await request(evaluation,'GET','/api/series?'+params);const line=result.data.series?.find(s=>s.key==='fixture-model');return result.data.projection?.status==='complete'&&line?.exact_total?.decimal===expected?result:null;},'actual projection '+currency+' '+expected,600,500);
 }
 const create={'If-None-Match':'*'};
 const price={id:'test-price',vendor:'fixture',model:'fixture-model',effort:'not_applicable',modes:{},from:'2020-01-01T00:00:00Z',to:null,currency:'USD',unit_tokens:1000,input_uncached:'2',input_cached:'1',output:'4'};
 const sp=await request(evaluation,'PUT','/api/prices/test-price',price,create);
 await request(evaluation,'PUT','/api/conversions/usd-eur',{id:'usd-eur',from_currency:'USD',to_currency:'EUR',rate:'0.9'},create);
 const usage={schema_version:1,id:'usage-test',kind:'usage',run_id:'12345678-1234-1234-1234-123456789abc',execution_id:'execution-test',workflow:'group/workflow',agent:'group/agent',vendor:'fixture',model:'fixture-model',effort:'not_applicable',modes:{},occurred_at:'2026-11-20T10:00:00Z',input_tokens:100,output_tokens:10,cached_input_tokens:20,uncached_input_tokens:80,status:'reported',missing:[],raw_usage:{prompt_tokens:100,completion_tokens:10,prompt_tokens_details:{cached_tokens:20}}};
 const su=await request(evaluation,'PUT','/api/measurements/usage-test',usage,create);
 const cost=await eventually(async()=>{const value=await request(blobs,'GET','/blobs/measurement-costs/usage-test.json');return value.data.amount==='0.22'?value:null;},'initial cost');assert.equal(cost.data.amount,'0.22');if(realObserve){await graph('0.22');await graph('0.198','EUR');}
 await request(evaluation,'PUT','/api/measurements/usage-test',usage,create,412);
 await request(evaluation,'PUT','/api/prices/test-price',{...price,output:'8'},{'If-Match':sp.etag||`"${sp.data.revision}"`});
 await request(evaluation,'PUT','/api/measurements/usage-test',usage,{'If-Match':su.etag});
 const changed=await eventually(async()=>{const value=await request(blobs,'GET','/blobs/measurement-costs/usage-test.json');return value.data.amount==='0.26'?value:null;},'replacement cost');assert.equal(changed.data.amount,'0.26');if(realObserve){await graph('0.26');await graph('0.234','EUR');}
 const missing={...usage,id:'usage-missing',raw_usage:{prompt_tokens:100,completion_tokens:10}};delete missing.cached_input_tokens;delete missing.uncached_input_tokens;
 await request(evaluation,'PUT','/api/measurements/usage-missing',missing,create);
 const unknown=await eventually(()=>request(blobs,'GET','/blobs/measurement-costs/usage-missing.json'),'missing cost status');assert.equal(unknown.data.status,'unavailable');assert.ok(unknown.data.missing.length);
 const producer='22345678-1234-1234-1234-123456789abc';
 await request(blobs,'PUT','/blobs/workflow-executions/execution-test.json',{schema_version:1,execution_id:usage.execution_id,run_id:usage.run_id,artifact_prefix:`runs/${usage.run_id}`},{},204);
 await request(blobs,'PUT','/blobs/workflow-executions/judge-execution.json',{schema_version:1,execution_id:'judge-execution',run_id:producer,artifact_prefix:`runs/${producer}`},{},204);
 await request(blobs,'PUT',`/blobs/runs/${producer}/input`,{run_id:usage.run_id,execution_id:usage.execution_id},{},204);
 await request(blobs,'PUT',`/blobs/runs/${producer}/run.json`,{execution_id:'judge-execution',input_artifact:`runs/${producer}/input`},{},204);
 await request(blobs,'PUT',`/blobs/runs/${producer}/result.json`,{score:0,explanation:'Evidence checked'},{},204);
 const extraction={producer_run_id:producer,target_run_id:usage.run_id,target_execution_id:usage.execution_id,artifact_id:`runs/${producer}/result.json`,agent_ref:'judges/test@v1',mapping_version:'v1',mappings:[{path:'/score',name:'quality',type:'number'},{path:'/explanation',name:'finding',type:'string'}]};
 await request(evaluation,'POST','/api/extractions',extraction,{},201);await request(evaluation,'POST','/api/extractions',extraction);
 const results=await request(evaluation,'GET','/api/measurements?kind=judge');const items=results.data.items||results.data.measurements;
 assert.equal(items.length,2);assert.ok(items.some(m=>m.metric_name==='quality'&&Number(m.number_value)===0));
 if(realObserve){
  const params=new URLSearchParams({metric:'quality',group_by:'run',from:new Date(Date.now()-3600000).toISOString(),to:new Date(Date.now()+60000).toISOString()});
  await eventually(async()=>{const answer=await request(evaluation,'GET','/api/series?'+params);return answer.data.projection?.status==='complete'&&answer.data.series?.some(line=>line.points.some(point=>point.value===0))?answer:null;},'real histogram quality graph',600,500);
 }
 console.log('PASS: real blobstore/evaluation processes; CAS, pricing replacement, unknown cache usage, JSON extraction and deduplication.');
 console.log(realObserve?'PASS: actual OpenObserve initial projection, physical replacement, and EUR/USD graph conversion. Browser rendering not verified.':'NOT VERIFIED here: live broker/Temporal/OpenObserve, browser rendering, or cluster deployment.');
}finally{
 for(const c of children.reverse()){if(c.exitCode!==null)continue;c.kill('SIGTERM');await Promise.race([new Promise(r=>c.once('exit',r)),delay(5000)]);if(c.exitCode===null){c.kill('SIGKILL');await new Promise(r=>c.once('exit',r));}}
 if(realObserve){try{await request(observeURL,'DELETE',`/api/default/streams/${testStream}?type=logs&delete_all=true`,undefined,observeHeaders);console.log('Disposable projection stream deletion requested.');}catch(e){console.error('Test stream cleanup pending: '+e.message);}}
 await rm(directory,{recursive:true,force:true});
}
