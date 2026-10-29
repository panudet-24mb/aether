package studio

import "encoding/json"

// Official widgets for Milesight LoRaWAN sensors behind a UG65 (docs/platform/milesight-lorawan.md). Like the kit
// widgets they have no bundled decoder: render(input) reads Aether's canonical reading, where a Milesight uplink
// carries every value in input.data.metrics (temperature, humidity, nh3, h2s, motion, daylight, distance, leak,
// battery, and rssi / snr when the gateway sent its envelope) and states in input.data.values (*_sensor_status,
// calibration_status). These sensors report every few minutes to hours, so each widget judges staleness by its
// model's default reporting interval, not by the BLE minute.

// milesightHelpers are repeated in each widget because every render runs in a fresh, isolated context.
const milesightHelpers = `function esc(s){return String(s==null?'':s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;').replace(/'/g,'&#39;');}
function ago(iso,now){var t=Date.parse(iso),n=Date.parse(now);if(!isFinite(t)||!isFinite(n))return '—';var s=Math.max(0,Math.round((n-t)/1000));if(s<60)return s+' วินาทีที่แล้ว';if(s<3600)return Math.round(s/60)+' นาทีที่แล้ว';if(s<86400)return Math.round(s/3600)+' ชั่วโมงที่แล้ว';return Math.round(s/86400)+' วันที่แล้ว';}
function span(ms){var m=Math.max(0,Math.round(ms/60000));return m<60?m+' นาที':Math.floor(m/60)+' ชม. '+(m%60)+' นาที';}
function stale(d,now,min){return !d||!d.received_at||Date.parse(now)-Date.parse(d.received_at)>min*60000;}
function foot(d,src,now,min){return '<div class="muted">'+(src==='simulated'?'SIMULATED · ':'')+(d&&d.received_at?'อัปเดต '+ago(d.received_at,now):'รอข้อมูล')+(d&&d.received_at&&stale(d,now,min)?' · ข้อมูลเก่า (เกินรอบส่ง '+min+' นาที)':'')+'</div>';}
function meta(d){var m=d.metrics||{},b=m.battery!=null?m.battery:d.battery,r=m.rssi!=null?m.rssi:d.rssi,o='';if(b!=null&&b>=0)o+='<div><strong>'+Math.round(b)+'%</strong>แบตเตอรี่</div>';if(r!=null)o+='<div><strong>'+r+' dBm</strong>RSSI</div>';if(m.snr!=null)o+='<div><strong>'+Number(m.snr).toFixed(1)+' dB</strong>SNR</div>';return o;}
function rows(h){return (h||[]).filter(function(x){return x&&x.received_at;}).sort(function(a,b){return Date.parse(a.received_at)-Date.parse(b.received_at);});}
var TZ=25200000,MON=['ม.ค.','ก.พ.','มี.ค.','เม.ย.','พ.ค.','มิ.ย.','ก.ค.','ส.ค.','ก.ย.','ต.ค.','พ.ย.','ธ.ค.'];
function pad(n){return (n<10?'0':'')+n;}
function clock(t){var d=new Date(t+TZ);return pad(d.getUTCHours())+':'+pad(d.getUTCMinutes());}
function day(t){var d=new Date(t+TZ);return d.getUTCDate()+' '+MON[d.getUTCMonth()];}
function axis(t0,t1){var w=Math.max(1,t1-t0),M=60000,S=[5*M,10*M,15*M,30*M,60*M,120*M,180*M,360*M,720*M,1440*M],st=S[S.length-1],x=[],o='';for(var i=0;i<S.length;i++){if(w/S[i]<=7){st=S[i];break;}}
 for(var t=Math.ceil((t0+TZ)/st)*st-TZ;t<=t1;t+=st){var p=(t-t0)/w*100;x.push(p);o+='<span style="left:'+p.toFixed(2)+'%;transform:translateX('+(p<6?'0':p>94?'-100%':'-50%')+')">'+(st>=1440*M||(t+TZ)%86400000===0?day(t):clock(t))+'</span>';}
 return {x:x,html:'<div class="axis">'+o+'</div>'};}
function line(h,key,color,digits,now,unit){var r=rows(h).filter(function(x){return x.metrics&&typeof x.metrics[key]==='number';}).slice(-200);if(r.length<2)return '';var v=r.map(function(x){return x.metrics[key];}),lo=Math.min.apply(null,v),hi=Math.max.apply(null,v),top=Math.max(hi,lo+Math.pow(10,-digits));var t0=Date.parse(r[0].received_at),w=Math.max(1,Date.parse(now)-t0),a=axis(t0,Date.parse(now));
 return '<div class="stats">ล่าสุด <b>'+v[v.length-1].toFixed(digits)+'</b> · สูงสุด <b>'+hi.toFixed(digits)+'</b> · ต่ำสุด <b>'+lo.toFixed(digits)+'</b> '+unit+'</div><svg class="plot" viewBox="0 0 600 60">'+a.x.map(function(p){return '<line x1="'+(p*6).toFixed(1)+'" x2="'+(p*6).toFixed(1)+'" y1="0" y2="60" stroke="#26302d" stroke-width="1"/>';}).join('')+'<polyline fill="none" stroke="'+color+'" stroke-width="2" points="'+r.map(function(x,i){return ((Date.parse(x.received_at)-t0)/w*600).toFixed(1)+','+(56-(v[i]-lo)/(top-lo)*52).toFixed(1);}).join(' ')+'"/></svg>'+a.html;}
function timeline(h,key,now,on,onWord,offWord){var r=rows(h).filter(function(x){return x.metrics&&x.metrics[key]!=null;});if(!r.length)return '';var t0=Date.parse(r[0].received_at),t1=Date.parse(now),w=Math.max(1,t1-t0),a=axis(t0,t1),o='',busy=0;
 r.forEach(function(x,i){var s=Date.parse(x.received_at),e=i+1<r.length?Date.parse(r[i+1].received_at):t1;if(x.metrics[key]!==1)return;busy+=e-s;o+='<span class="seg" style="left:'+((s-t0)/w*100).toFixed(2)+'%;width:'+Math.max(0.3,(e-s)/w*100).toFixed(2)+'%;background:'+on+'" title="'+onWord+' '+clock(s)+'–'+clock(e)+'"></span>';});
 return '<div class="bar">'+o+a.x.map(function(p){return '<span class="tick" style="left:'+p.toFixed(2)+'%"></span>';}).join('')+'</div>'+a.html+'<div class="legend"><span><i style="background:'+on+'"></i>'+onWord+' '+span(busy)+'</span><span><i></i>'+offWord+'</span><span>'+day(t0)+' '+clock(t0)+' – ตอนนี้</span></div>';}
function since(ev,types,now){var e=(ev||[]).filter(function(x){return types.indexOf(x.event_type)>=0;})[0];return e?span(Date.parse(now)-Date.parse(e.occurred_at)):null;}
function today(ev,type,now){var d=new Date(now),start=Date.UTC(d.getUTCFullYear(),d.getUTCMonth(),d.getUTCDate())-7*3600000;if(Date.parse(now)-start>=86400000)start+=86400000;var n=(ev||[]).filter(function(x){return x.event_type===type&&Date.parse(x.occurred_at)>=start;}).length;return (n>=(ev||[]).length&&n>0?'≥ ':'')+n;}
`

const milesightCSS = kitCSS + `.chart{margin-top:12px}.chart>b{font-size:11px;font-weight:500;color:#939e9c}
.chart svg.plot{display:block;height:auto;margin-top:4px;background:#121a18;border-radius:4px}
.bar{position:relative;height:18px;margin-top:6px;border-radius:4px;background:#26302d;overflow:hidden}
.bar .seg{position:absolute;top:0;bottom:0}.bar .tick{position:absolute;top:0;bottom:0;width:1px;background:rgba(14,19,18,.55)}
.axis{position:relative;height:15px;margin-top:3px;font-size:10.5px;color:#6f7a78}.axis span{position:absolute;top:0;white-space:nowrap}
.legend{display:flex;flex-wrap:wrap;gap:6px 14px;margin-top:6px;font-size:11px;color:#939e9c}.legend i{display:inline-block;width:10px;height:10px;border-radius:2px;margin-right:5px;vertical-align:-1px;background:#26302d}.legend span:last-child{margin-left:auto}
.stats{font-size:11px;color:#939e9c;margin-top:2px}.stats b{color:#e6edea;font-weight:500}
.level{display:inline-block;margin-left:10px;padding:2px 10px;border-radius:12px;font-size:13px;vertical-align:middle;border:1px solid currentColor}`

func milesightWidgets() []Item {
	make := func(id, name, model string, kinds []string, code string) Item {
		b, _ := json.Marshal(Definition{HTML: `<div class="eyebrow">AETHER</div>`, CSS: milesightCSS, Code: milesightHelpers + code, Kinds: kinds})
		return Item{ID: id, Kind: "widget", Name: name, Brand: "Milesight", Model: model, Version: 1, Visibility: "community", Official: true, Definition: b, Revision: 1}
	}
	return []Item{
		// Odour in a restroom. The limits are a starting point for a restroom, not a health standard: NH3 becomes
		// noticeable from a few ppm, H2S (rotten egg) from a few hundredths of a ppm. Edit LIMIT in a copy to tune.
		make("official-milesight-gs301-v1", "Restroom odour (NH₃ / H₂S)", "GS301", []string{"environment"}, `var LIMIT={nh3:[1,5],h2s:[0.02,0.1]},REPORT=10;
function level(v,l){return v==null?0:v>=l[1]?2:v>=l[0]?1:0;}
function render(input){var d=input.data||{},m=d.metrics||{},s=d.values||{},now=input.now;
 if(m.nh3==null&&m.h2s==null&&!s.nh3_sensor_status&&!s.h2s_sensor_status)return {html:'<div class="eyebrow">ODOUR · GS301</div><div class="state warn">รอข้อมูลกลิ่น</div><div class="muted">widget นี้ใช้กับ Milesight GS301 ที่ส่งค่า NH₃ / H₂S</div>'+foot(d,input.source,now,REPORT*2+10)};
 var fault=s.nh3_sensor_status==='device error'||s.h2s_sensor_status==='device error',warm=s.nh3_sensor_status==='polarizing'||s.h2s_sensor_status==='polarizing';
 var lv=Math.max(level(m.nh3,LIMIT.nh3),level(m.h2s,LIMIT.h2s));
 var head=fault?'<div class="state bad">เซนเซอร์ผิดพลาด</div>':warm?'<div class="state warn">กำลังอุ่นเครื่อง</div><div class="muted">เซนเซอร์แก๊สต้องอุ่นเครื่องหลังเปิด ค่า NH₃ / H₂S จะมาเมื่อพร้อม</div>':'<div class="state '+['ok','warn','bad'][lv]+'">'+['สะอาด','เริ่มมีกลิ่น','กลิ่นแรง'][lv]+'</div>';
 var gas=function(k,label,dg){var st=s[k+'_sensor_status'];return '<div><strong>'+(st==='polarizing'?'อุ่นเครื่อง':st==='device error'?'ผิดพลาด':m[k]==null?'—':Number(m[k]).toFixed(dg)+' ppm')+'</strong>'+label+'</div>';};
 var row=gas('nh3','NH₃ แอมโมเนีย',2)+gas('h2s','H₂S ไข่เน่า',3)+(m.temperature!=null?'<div><strong>'+Number(m.temperature).toFixed(1)+' °C</strong>อุณหภูมิ</div>':'')+(m.humidity!=null?'<div><strong>'+Math.round(m.humidity)+' %</strong>ความชื้น</div>':'')+meta(d);
 var c1=line(input.history,'nh3','#c4a7ff',2,now,'ppm'),c2=line(input.history,'h2s','#f8b74f',3,now,'ppm');
 return {html:'<div class="eyebrow">ODOUR · GS301'+(d.model&&d.model!=='GS301'?' · '+esc(d.model):'')+'</div>'+head+'<div class="row">'+row+'</div>'+(c1?'<div class="chart"><b>NH₃</b>'+c1+'</div>':'')+(c2?'<div class="chart"><b>H₂S</b>'+c2+'</div>':'')+'<div class="muted">ระดับกลิ่นเทียบกับ NH₃ ≥ '+LIMIT.nh3[0]+' / '+LIMIT.nh3[1]+' ppm และ H₂S ≥ '+LIMIT.h2s[0]+' / '+LIMIT.h2s[1]+' ppm · ปรับได้ในโค้ด</div>'+foot(d,input.source,now,REPORT*2+10)};}`),
		make("official-milesight-ws202-v1", "PIR & light", "WS202", []string{"occupancy", "motion"}, `var REPORT=20;
function render(input){var d=input.data||{},m=d.metrics||{},now=input.now;
 if(m.motion==null&&m.daylight==null)return {html:'<div class="eyebrow">PIR · WS202</div><div class="state warn">รอข้อมูล PIR</div><div class="muted">widget นี้ใช้กับ Milesight WS202 (PIR และแสง)</div>'+foot(d,input.source,now,REPORT*2+10)};
 var on=m.motion===1,last=(input.events||[]).filter(function(e){return e.event_type==='occupied'||e.event_type==='motion';})[0];
 var row=(m.daylight!=null?'<div><strong>'+(m.daylight===1?'สว่าง':'มืด')+'</strong>แสง</div>':'')+'<div><strong>'+(last?ago(last.occurred_at,now):on?'ตอนนี้':'—')+'</strong>เคลื่อนไหวล่าสุด</div><div><strong>'+today(input.events,'occupied',now)+' ครั้ง</strong>มีคนเข้าวันนี้</div>'+meta(d);
 return {html:'<div class="eyebrow">PIR &amp; LIGHT · WS202</div><div class="state '+(on?'warn':'ok')+'">'+(on?'มีการเคลื่อนไหว':'ว่าง')+'</div><div class="row">'+row+'</div><div class="chart"><b>การเคลื่อนไหวในช่วงที่เลือก</b>'+timeline(input.history,'motion',now,'#f8b74f','มีการเคลื่อนไหว','ไม่มี')+'</div>'+foot(d,input.source,now,REPORT*2+10)};}`),
		make("official-milesight-vs330-v1", "Restroom stall occupancy", "VS330", []string{"occupancy", "motion"}, `var REPORT=10;
function render(input){var d=input.data||{},m=d.metrics||{},s=d.values||{},now=input.now;
 if(m.motion==null)return {html:'<div class="eyebrow">STALL · VS330</div><div class="state warn">รอสถานะห้อง</div><div class="muted">widget นี้ใช้กับ Milesight VS330 (ห้องน้ำมีคน / ว่าง)</div>'+foot(d,input.source,now,REPORT*2+10)};
 var on=m.motion===1,dur=since(input.events,on?['occupied']:['vacant'],now);
 var row='<div><strong>'+(dur||'—')+'</strong>'+(on?'มีคนมาแล้ว':'ว่างมาแล้ว')+'</div><div><strong>'+today(input.events,'occupied',now)+' ครั้ง</strong>ใช้งานวันนี้</div>'+(m.distance!=null?'<div><strong>'+Math.round(m.distance)+' mm</strong>ระยะถึงพื้น</div>':'')+(s.calibration_status?'<div><strong>'+(s.calibration_status==='success'?'สำเร็จ':esc(s.calibration_status))+'</strong>การปรับเทียบ</div>':'')+meta(d);
 return {html:'<div class="eyebrow">STALL · VS330</div><div class="state '+(on?'bad':'ok')+'">'+(on?'มีคน':'ว่าง')+'</div><div class="row">'+row+'</div><div class="chart"><b>สถานะห้องในช่วงที่เลือก</b>'+timeline(input.history,'motion',now,'#ff8164','มีคน','ว่าง')+'</div>'+foot(d,input.source,now,REPORT*2+10)};}`),
		make("official-milesight-ws303-v1", "Water leak", "WS303", []string{"leak"}, `var REPORT=1080;
function render(input){var d=input.data||{},m=d.metrics||{},now=input.now;
 if(m.leak==null)return {html:'<div class="eyebrow">LEAK · WS303</div><div class="state warn">รอสถานะน้ำรั่ว</div><div class="muted">widget นี้ใช้กับ Milesight WS303 (น้ำรั่ว)</div>'+foot(d,input.source,now,REPORT*2+10)};
 var wet=m.leak===1,last=(input.events||[]).filter(function(e){return e.event_type==='leak';})[0];
 var row='<div><strong>'+(last?ago(last.occurred_at,now):'ไม่เคย')+'</strong>พบน้ำรั่วล่าสุด</div>'+meta(d);
 return {html:'<div class="eyebrow">WATER LEAK · WS303</div><div class="state '+(wet?'bad':'ok')+'">'+(wet?'พบน้ำรั่ว':'แห้ง · ปกติ')+'</div><div class="row">'+row+'</div><div class="muted">WS303 ส่งทันทีเมื่อพบน้ำ และส่งสถานะปกติทุก '+Math.round(REPORT/60)+' ชั่วโมง</div>'+foot(d,input.source,now,REPORT*2+10)};}`),
	}
}
