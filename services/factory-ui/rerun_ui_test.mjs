import test from "node:test";
import assert from "node:assert/strict";
import vm from "node:vm";
import fs from "node:fs";
const source=fs.readFileSync(new URL("web/runs.js",import.meta.url),"utf8");
const code=source.slice(source.indexOf("  const rerunning"),source.indexOf("  // clearRuns removes"));
test("re-run uses original input/current workflow and prevents duplicate pending starts",async()=>{
 let resolve;const calls=[];const ctx={api:(...args)=>{calls.push(args);return new Promise(r=>resolve=r)},creatorHeaders:()=>({"X-Creator":"ada"}),runIndex:{refresh:async()=>{}},location:{hash:"#runs/old"},setSaveState(){},setStatus(){}};
 vm.runInNewContext(code,ctx);
 const run={trace_id:"old",workflow:"a workflow",input_artifact:"runs/old/input"};
 const pending=ctx.rerun(run);await ctx.rerun(run);assert.equal(calls.length,1);
 assert.equal(calls[0][1],"/api/orchestrator/workflows/a%20workflow/runs?input=runs%2Fold%2Finput");
 assert.equal(calls[0][2],undefined);resolve({trace_id:"new"});await pending;assert.equal(ctx.location.hash,"#runs/new");
});
test("failed re-run preserves selection and permits retry",async()=>{
 const errors=[];let calls=0;const ctx={api:async()=>{calls++;throw new Error("input missing")},creatorHeaders:()=>({}),location:{hash:"#runs/old"},setStatus:(m)=>errors.push(m)};
 vm.runInNewContext(code,ctx);await ctx.rerun({trace_id:"old",workflow:"test"});await ctx.rerun({trace_id:"old",workflow:"test"});
 assert.equal(calls,2);assert.equal(ctx.location.hash,"#runs/old");assert.match(errors[0],/Not started: input missing/);
});
test("re-run uses the version the run recorded, else the newest",async()=>{
 const calls=[];const ctx={api:async(...args)=>{calls.push(args);return{trace_id:"new"}},creatorHeaders:()=>({}),runIndex:{refresh:async()=>{}},location:{hash:""},setSaveState(){},setStatus(){}};
 vm.runInNewContext(code,ctx);
 await ctx.rerun({trace_id:"a",workflow:"demo",workflow_version:2,input_artifact:"runs/a/input"});
 assert.equal(calls[0][1],"/api/orchestrator/workflows/demo/runs?input=runs%2Fa%2Finput&version=2");
 await ctx.rerun({trace_id:"b",workflow:"demo",input_artifact:"runs/b/input"});
 assert.equal(calls[1][1],"/api/orchestrator/workflows/demo/runs?input=runs%2Fb%2Finput","a run without a recorded version runs the newest");
 await ctx.rerun({trace_id:"c",workflow:"ecc/ECC-plan",workflow_version:1,input_artifact:"runs/c/input"});
 assert.equal(calls[2][1],"/api/orchestrator/workflows/ecc%2FECC-plan/runs?input=runs%2Fc%2Finput&version=1","a grouped name is one part of the path");
 const runVersion=vm.runInContext("runVersion",ctx);
 assert.equal(runVersion({workflow_version:2},{workflow:{name:"demo",version:3}}),3,"the metadata names the version that ran");
 assert.equal(runVersion({workflow_version:2},null),2);assert.equal(runVersion({},null),0);assert.equal(runVersion(undefined,{}),0);
});
test("the run detail reads the metadata only when the run has it and shows the workflow version",async()=>{
 const at=(mark)=>{const i=source.indexOf(mark);assert.ok(i>0,mark);return i};
 const block=source.slice(at("  const metadataCache"),at("  const stateOf"));
 let loads=0;const ctx={loadBlob:async(id)=>{loads++;assert.equal(id,"runs/t1/metadata.json");return{text:JSON.stringify({workflow:{name:"demo",version:3},workflows:[{name:"inner",version:1}]})}}};
 vm.runInNewContext(`${block}\nglobalThis.runMetadata=runMetadata;`,ctx);
 assert.equal(await ctx.runMetadata("t0",{}),null);assert.equal(loads,0,"a run without metadata asks for nothing");
 const value=await ctx.runMetadata("t1",{metadata:true});assert.equal(value.workflow.version,3);
 await ctx.runMetadata("t1",{metadata:true});assert.equal(loads,1,"read once");
 const broken={loadBlob:async()=>{throw new Error("gone")}};vm.runInNewContext(`${block}\nglobalThis.runMetadata=runMetadata;`,broken);
 assert.equal(await broken.runMetadata("t2",{metadata:true}),null,"a run whose metadata cannot be read still shows");
 assert.match(source,/`Workflow v\$\{version\}`/);
});
