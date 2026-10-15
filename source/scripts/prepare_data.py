"""Build embedded Healthtara snapshots from provided workbooks; no web scraping.

Usage: python prepare_data.py --v3 PATH --rekap PATH --output PATH
Requires openpyxl, pandas and scikit-learn for the build only.
End users run the compiled EXE and do not need these dependencies.
"""
import argparse, collections, datetime, gzip, hashlib, json, sys, zipfile, shutil
from pathlib import Path
import openpyxl
def inside_ring(x,y,ring):
    inside=False
    for a,b in zip(ring,ring[1:]+ring[:1]):
        x1,y1=a[:2];x2,y2=b[:2]
        if (y1>y)!=(y2>y) and x < (x2-x1)*(y-y1)/(y2-y1)+x1: inside=not inside
    return inside

def in_geometry(x,y,geom):
    polygons=geom['coordinates'] if geom['type']=='MultiPolygon' else [geom['coordinates']]
    return any(inside_ring(x,y,p[0]) and not any(inside_ring(x,y,r) for r in p[1:]) for p in polygons)

def edge_distance(x,y,geom):
    polygons=geom['coordinates'] if geom['type']=='MultiPolygon' else [geom['coordinates']]
    best=float('inf')
    for p in polygons:
        for ring in p:
            for a,b in zip(ring,ring[1:]+ring[:1]):
                dx,dy=b[0]-a[0],b[1]-a[1];length=dx*dx+dy*dy
                t=max(0,min(1,((x-a[0])*dx+(y-a[1])*dy)/length)) if length else 0
                best=min(best,((x-a[0]-t*dx)**2+(y-a[1]-t*dy)**2)**0.5)
    return best

def clean(v):
    if isinstance(v, (datetime.date,datetime.datetime)): return v.isoformat()
    if isinstance(v,float) and (v!=v or abs(v)==float('inf')): return None
    return v

def read_sheet(book,name):
    rows=book[name].iter_rows(values_only=True)
    header=None
    for row in rows:
        if row and row[0] in ('Kode RS','Tahun','Topik/field','Dimensi/instrumen','Kelompok/label','Provinsi'):
            header=[str(c or '') for c in row]; break
    if header is None: return []
    return [{h:clean(v) for h,v in zip(header,row) if h} for row in rows if any(v is not None for v in row)]

def canonical(s):
    return {'Bangka Belitung':'Kepulauan Bangka Belitung','Kep. Bangka Belitung':'Kepulauan Bangka Belitung','Daerah Istimewa Yogyakarta':'DI Yogyakarta','Yogyakarta':'DI Yogyakarta'}.get(str(s).strip(),str(s).strip())

def number(v):
    if isinstance(v,(int,float)):return clean(v)
    return None

def dump(path,data,gz=False):
    raw=json.dumps(data,ensure_ascii=False,separators=(',',':'),allow_nan=False).encode()
    path.parent.mkdir(parents=True,exist_ok=True)
    if gz: path.write_bytes(gzip.compress(raw,mtime=0))
    else:path.write_bytes(raw)

def main():
    a=argparse.ArgumentParser();a.add_argument('--v3',required=True);a.add_argument('--rekap',required=True);a.add_argument('--output',required=True);args=a.parse_args()
    v3=Path(args.v3).resolve();out=Path(args.output).resolve();sys.path.insert(0,str(v3))
    from data_logic import load_sources
    from analysis_logic import analysis_frame,cluster_frame,sensitivity
    d=load_sources()
    def records(df): return json.loads(df.to_json(orient='records',force_ascii=False))
    score=analysis_frame(d['province'],d['ski']);clusters={}
    for k in range(2,7):
        frame,sil=cluster_frame(score,k);clusters[str(k)]={'rows':records(frame),'silhouette':sil}
    print('Reading hospital workbook',flush=True)
    w=openpyxl.load_workbook(args.rekap,read_only=True,data_only=True)
    names=['Profil RS','Kapasitas RS','Pendalaman RS','Bukti Web','Pelayanan','SDM','Tempat Tidur','Kategori Data','Klasifikasi dan Skor','Sumber Web','Perubahan Master','Perlu Verifikasi','Metode dan Kamus']
    sheets={}
    for name in names:
        sheets[name]=read_sheet(w,name);print(name,len(sheets[name]),flush=True)
    by={}
    for name,rows in sheets.items():
        dic=collections.defaultdict(list)
        for r in rows:
            if r.get('Kode RS'):dic[str(r['Kode RS'])].append(r)
        by[name]=dic
    geo=json.loads((out/'indonesia.geojson').read_text())
    shapes={canonical(f['properties']['PROVINSI']):f['geometry'] for f in geo['features']}
    points=collections.Counter((r.get('Latitude terpakai'),r.get('Longitude terpakai')) for r in sheets['Profil RS'] if number(r.get('Latitude terpakai')) is not None)
    hospitals=[];coordinate_counts=collections.Counter()
    for p in sheets['Profil RS']:
        code=str(p['Kode RS']);caps=by['Kapasitas RS'].get(code,[{}])[0]
        services=by['Pelayanan'][code];flags=by['Perlu Verifikasi'][code].copy()
        lat=number(p.get('Latitude terpakai'));lon=number(p.get('Longitude terpakai'));province=canonical(p.get('Provinsi'))
        coord='Belum ada koordinat';validated=False
        if lat is not None and lon is not None:
            poly=shapes.get(province)
            if poly is not None and in_geometry(lon,lat,poly):coord='Sesuai geometri provinsi';validated=True
            elif poly is not None and edge_distance(lon,lat,poly)<=0.15:coord='Dekat batas provinsi perlu verifikasi'
            else:coord='Titik tidak cocok dengan provinsi'
            if not validated:flags.append({'Kelompok data':'Lokasi Healthtara','Temuan':coord,'Nilai sumber':f'{lat}, {lon}','Tindakan lanjut':'Konfirmasi titik bangunan. Titik tidak digeser secara otomatis.','URL sumber':p.get('URL data koordinat')})
        coordinate_counts[coord]+=1
        names_services=[s.get('Pelayanan sesuai sumber','') for s in services]
        tags=sorted({s.get('Kategori analitis','') for s in services if s.get('Kategori analitis')})
        h=dict(code=code,name=p.get('Nama profil SIRS saat diakses') or p.get('Nama dalam daftar dasar'),originalName=p.get('Nama dalam daftar dasar'),province=province,district=p.get('Kabupaten/kota'),type=p.get('Jenis tercatat'),klass=p.get('Kelas daftar dasar'),profileClass=p.get('Kelas profil SIRS'),owner=p.get('Kepemilikan'),address=p.get('Alamat'),phone=p.get('Telepon'),website=p.get('Website RS ditelusuri'),websiteSource=p.get('Sumber identitas website'),latitude=lat,longitude=lon,coordinateStatus=coord,coordinateValidated=validated,coordinateShared=points.get((lat,lon),0),maps=p.get('Google Maps'),source=p.get('URL profil SIRS'),accessed=p.get('Diakses UTC'),sourceUpdated=p.get('Last Update pada profil'),flagship=p.get('Unggulan dengan bukti eksplisit'),capability=p.get('Klasifikasi kemampuan PMK 6/2026'),email=p.get('Email publik pada situs RS'),scope=p.get('Status pendalaman website'),bedRowsTotal=number(caps.get('TT jumlah baris SIRS')),staffRowsTotal=number(caps.get('SDM jumlah baris (bukan unik)')),doctors=number(caps.get('Dokter umum dilaporkan')),specialists=number(caps.get('Spesialis/subspesialis dilaporkan')),nurses=number(caps.get('Perawat/Ners nonasisten')),midwives=number(caps.get('Bidan nonasisten')),pharmacists=number(caps.get('Apoteker dilaporkan')),land=number(caps.get('Luas tanah standar (m²)')),building=number(caps.get('Luas bangunan standar (m²)')),serviceCount=len(services),completeness=number(caps.get('Kelengkapan data dasar')),serviceTags=tags,serviceSearch=' | '.join(names_services),serviceFlags={key:str(caps.get(field,'')).startswith('Terdaftar') for key,field in {'radiology':'Layanan radiologi','lab':'Layanan laboratorium','ct':'Layanan CT scan','mri':'Layanan MRI','xray':'Layanan X-ray/rontgen','dialysis':'Dialisis/CAPD'}.items()},verificationCount=len(flags),webEvidenceCount=len(by['Bukti Web'][code]))
        hospitals.append(h)
        detail={'code':code,'profile':p,'capacity':caps,'services':services,'staff':by['SDM'][code],'beds':by['Tempat Tidur'][code],'web':by['Bukti Web'][code],'webSources':by['Sumber Web'][code],'verification':flags,'deepDive':by['Pendalaman RS'][code],'changes':by['Perubahan Master'][code]}
        dump(out/'details'/f'{code}.json.gz',detail,True)
    hospitals.sort(key=lambda h:(h['province'],h['name'] or ''))
    sources=records(d['sources'])
    core={'schema':'healthtara-v1','version':'1.0.0','dataDate':'2026-10-12','origin':'Healthcare_Intelligence_Rekap_Fasilitas_2026-10-12.xlsx','originSHA256':hashlib.sha256(Path(args.rekap).read_bytes()).hexdigest(),'hospitals':hospitals,'province':records(d['province']),'facility':records(d['facility']),'kmk':records(d['kmk']),'rifaskes':records(d['rifaskes']),'ski':records(d['ski']),'sources':sources,'scores':records(score),'clusters':clusters,'sensitivity':records(sensitivity(score)),'sirsOriginal':records(__import__('pandas').read_csv(v3/'data/sirs_rs_snapshot.csv',dtype=str)),'registry':json.loads((v3/'data/source_registry.json').read_text()),'coordinateCounts':dict(coordinate_counts),'snapshotNote':'Rekap 12 Oktober 2026 berisi 3.323 kode RS. Data tahunan tetap mengikuti tahun sumber. Bukti web didalami secara purposif pada 70 RS.'}
    dump(out/'core.json.gz',core,True)
    ref={name:sheets[name] for name in ['Kategori Data','Klasifikasi dan Skor','Metode dan Kamus','Pendalaman RS','Perubahan Master']}
    dump(out/'reference.json.gz',ref,True)
    for key in ['province','facility','kmk','rifaskes','ski']: d[key].to_csv(out/f'{key}.csv',index=False,encoding='utf-8-sig')
    dump(out/'build_report.json',{'hospitalCount':len(hospitals),'sheets':{k:len(v) for k,v in sheets.items()},'coordinateCounts':dict(coordinate_counts),'historicalRows':len(core['province']),'clusters':{k:v['silhouette'] for k,v in clusters.items()}})
    with zipfile.ZipFile(out/'hospital_details.zip','w',compression=zipfile.ZIP_STORED) as z:
        for f in sorted((out/'details').glob('*.json.gz')):z.write(f,f.name)
    shutil.rmtree(out/'details')
    print('Completed',len(hospitals),dict(coordinate_counts),flush=True)
if __name__=='__main__':main()
