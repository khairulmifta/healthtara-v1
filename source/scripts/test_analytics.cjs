const assert=require('node:assert/strict');const fs=require('fs'),zlib=require('zlib');const A=require('../web/analytics.js');
const core=JSON.parse(zlib.gunzipSync(fs.readFileSync(require('node:path').join(__dirname,'../web/data/core.json.gz'))));
assert.equal(core.hospitals.length,3323);assert.equal(new Set(core.hospitals.map(h=>h.code)).size,3323);
assert.deepEqual(A.percentile([1,2,2,4]),[25,62.5,62.5,100]);
const scored=A.scores(core.scores,[40,30,30]);for(const r of scored){const old=core.scores.find(o=>o.provinsi===r.provinsi);if(r.skor!=null)assert.ok(Math.abs(r.skor-old.skor)<1e-7);else assert.equal(r.provinsi,'DKI Jakarta');}
assert.throws(()=>A.scores(core.scores,[0,0,0]));assert.equal(A.sum([{v:null}],'v'),null);assert.equal(A.ratio([{v:10,penduduk:200000}],'v'),5);
const g={type:'Polygon',coordinates:[[[0,0],[2,0],[2,2],[0,2],[0,0]]]};assert.equal(A.coordinate(1,1,g).valid,true);assert.equal(A.coordinate(4,4,g).valid,false);assert.equal(A.coordinate(null,null,g).status,'Belum ada koordinat');
for(const [k,v] of Object.entries(core.clusters)){assert.equal(v.rows.length,37);assert.ok(v.silhouette>=-1&&v.silhouette<=1);assert.equal(new Set(v.rows.map(r=>r.klaster)).size,+k);}
console.log('PASS — snapshot IDs, score parity, missing values, ratios, geometry and K-means snapshots');
