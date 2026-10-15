'use strict';
importScripts('/vendor/xlsx.full.min.js','/analytics.js','/importer.js');
self.onmessage=async e=>{try{const p=await HTImport.parse(e.data.buffer,e.data.geo,message=>postMessage({progress:message}));p.origin=e.data.name;postMessage({result:p});}catch(error){postMessage({error:error.message});}};
