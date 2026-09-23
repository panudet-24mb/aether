package studio

import "encoding/json"

// Official widgets for the non-environment devices of the Minew MHS kit. They have no bundled decoder:
// render(input) consumes Aether's canonical reading (input.data / input.history with kind, metrics,
// beacon, battery, rssi), the persisted device events (input.events), the workspace's open alerts
// (input.alerts) and the server clock (input.now). No DOM, timers or network exist in this runtime.

const kitCSS = `body{margin:0;padding:20px;background:#10191f;color:#e6f2ee;font-family:system-ui,sans-serif}
.eyebrow{font-size:11px;letter-spacing:3px;color:#8fa3a8;text-transform:uppercase}
.state{font-size:40px;font-weight:500;margin:14px 0 6px;line-height:1.1}
.state.zone{font-size:24px;line-height:1.3;overflow-wrap:anywhere}
.ok{color:#a7f3d0}.warn{color:#f6c177}.bad{color:#ff8f70}
.row{display:flex;gap:18px;flex-wrap:wrap;margin-top:12px;font-size:12px;color:#9fb1b6}
.row strong{display:block;font-size:18px;font-weight:500;color:#e6f2ee}
.muted{font-size:12px;color:#8fa3a8;margin-top:10px;line-height:1.6}
ul{list-style:none;margin:12px 0 0;padding:0;display:flex;flex-direction:column;gap:8px}
li{display:flex;justify-content:space-between;gap:12px;padding:9px 11px;border-radius:8px;background:#16232a;font-size:13px}
li small{color:#8fa3a8;white-space:nowrap}
li.bad{border-left:3px solid #ff8f70}li.warn{border-left:3px solid #f6c177}li.ok{border-left:3px solid #a7f3d0}
svg{width:100%;height:70px;margin-top:10px}code{font-family:ui-monospace,monospace;font-size:12px;color:#b5c7cc;overflow-wrap:anywhere}`

// Shared helpers are repeated in each widget because every render runs in a fresh, isolated context.
const kitHelpers = `function esc(s){return String(s==null?'':s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;').replace(/'/g,'&#39;');}
function ago(iso,now){var t=Date.parse(iso),n=Date.parse(now);if(!isFinite(t)||!isFinite(n))return '—';var s=Math.max(0,Math.round((n-t)/1000));if(s<60)return s+' วินาทีที่แล้ว';if(s<3600)return Math.round(s/60)+' นาทีที่แล้ว';if(s<86400)return Math.round(s/3600)+' ชั่วโมงที่แล้ว';return Math.round(s/86400)+' วันที่แล้ว';}
function stale(d,now){return !d||!d.received_at||Date.parse(now)-Date.parse(d.received_at)>60000;}
function foot(d,src,now){return '<div class="muted">'+(src==='simulated'?'SIMULATED · ':'')+(d&&d.received_at?'อัปเดต '+ago(d.received_at,now):'รอข้อมูล')+(stale(d,now)?' · ข้อมูลเก่า':'')+'</div>';}
function meta(d){var m='';if(d.battery>0)m+='<div><strong>'+d.battery+'%</strong>แบตเตอรี่</div>';if(d.rssi!=null)m+='<div><strong>'+d.rssi+' dBm</strong>RSSI</div>';return m;}
`

var eventLabelsJS = `var LABEL={tamper:'ป้ายถูกถอด',tamper_cleared:'tamper กลับสู่ปกติ',button:'กดปุ่ม',leak:'พบน้ำรั่ว',leak_cleared:'น้ำรั่วหาย',motion:'เริ่มเคลื่อนไหว',motion_stopped:'หยุดเคลื่อนไหว',offline:'ขาดการติดต่อ',online:'กลับมาออนไลน์',threshold:'ค่าเกินเกณฑ์',threshold_cleared:'ค่ากลับเข้าเกณฑ์',zone:'เข้าโซนใหม่',door_open:'ประตูเปิด',door_closed:'ประตูปิด',occupied:'มีคนในพื้นที่',vacant:'ไม่มีคนในพื้นที่'};
var TONE={tamper:'bad',button:'bad',leak:'bad',offline:'bad',threshold:'warn',motion:'warn'};
`

// Presence list shared by the wearable widgets: one row per gateway that hears the tag, with a signal bar.
var presenceJS = `function bar(r){if(r==null)return 0;return Math.max(4,Math.min(100,Math.round((r+100)/55*100)));}
function zones(s,now){if(!s.length)return '';return '<ul>'+s.map(function(x){return '<li class="'+(x.current?'ok':x.fresh?'warn':'')+'" style="display:block"><div style="display:flex;justify-content:space-between;gap:12px"><span>'+esc(x.gateway_name)+(x.current?' · อยู่ที่นี่':'')+'</span><small>'+(x.fresh?(x.rssi==null?'—':x.rssi+' dBm'):ago(x.last_seen,now))+'</small></div><div style="height:4px;border-radius:2px;background:#22323a;margin-top:7px"><div style="height:4px;border-radius:2px;width:'+(x.fresh?bar(x.rssi):0)+'%;background:'+(x.current?'#a7f3d0':'#f6c177')+'"></div></div></li>';}).join('')+'</ul>';}
`

func kitWidgets() []Item {
	make := func(id, name, model string, kinds []string, code string) Item {
		b, _ := json.Marshal(Definition{HTML: `<div class="eyebrow">AETHER</div>`, CSS: kitCSS, Code: kitHelpers + code, Kinds: kinds})
		return Item{ID: id, Kind: "widget", Name: name, Brand: "Minew", Model: model, Version: 1, Visibility: "community", Official: true, Definition: b, Revision: 1}
	}
	return []Item{
		make("official-motion-v1", "Motion / activity", "E8S · C10 · B7 / A1-03 · A1-18", []string{"motion"}, `function render(input){var d=input.data||{},m=d.metrics||{},now=input.now;
 if(m.accel_g==null&&m.vibration==null&&m.motion==null)return {html:'<div class="eyebrow">MOTION</div><div class="state warn">ไม่มีข้อมูลการเคลื่อนไหว</div><div class="muted">อุปกรณ์นี้ยังไม่ส่งเฟรม accelerometer / vibration</div>'};
 var moving=m.vibration===1||m.motion===1;
 var pts=(input.history||[]).map(function(h){return h.metrics&&h.metrics.accel_g;}).filter(function(v){return typeof v==='number';}).slice(-60);
 var chart='';if(pts.length>1){var lo=Math.min.apply(null,pts),hi=Math.max.apply(null,pts);if(hi-lo<0.05){hi=lo+0.05;}chart='<svg viewBox="0 0 600 70"><polyline fill="none" stroke="#a7f3d0" stroke-width="2" points="'+pts.map(function(v,i){return (i/(pts.length-1)*600).toFixed(1)+','+(64-(v-lo)/(hi-lo)*58).toFixed(1);}).join(' ')+'"/></svg>';}
 return {html:'<div class="eyebrow">MOTION</div><div class="state '+(moving?'warn':'ok')+'">'+(moving?'กำลังเคลื่อนไหว':'นิ่ง')+'</div><div class="row">'+(m.accel_g!=null?'<div><strong>'+Number(m.accel_g).toFixed(2)+' g</strong>แรงรวม |a|</div>':'')+meta(d)+'</div>'+chart+foot(d,input.source,now)};}`),
		make("official-tamper-v1", "Tamper status", "MBT01 / A1-20", []string{"tamper"}, `function render(input){var d=input.data||{},m=d.metrics||{},now=input.now;
 if(m.tamper==null)return {html:'<div class="eyebrow">TAMPER</div><div class="state warn">ไม่มีเฟรม tamper</div><div class="muted">อุปกรณ์นี้ยังไม่ส่งเฟรม A1-20</div>'};
 var last=(input.events||[]).filter(function(e){return e.event_type==='tamper';})[0];
 return {html:'<div class="eyebrow">ANTI-TAMPER</div><div class="state '+(m.tamper===1?'bad':'ok')+'">'+(m.tamper===1?'ป้ายถูกถอด':'ปกติ · ติดอยู่')+'</div><div class="row">'+meta(d)+'<div><strong>'+(last?ago(last.occurred_at,now):'ไม่เคย')+'</strong>ถูกถอดล่าสุด</div></div><div class="muted">เฟรม A1-20 ยังไม่ยืนยันกับเครื่องจริง</div>'+foot(d,input.source,now)};}`),
		make("official-button-v1", "Emergency button", "B10 / Eddystone UID", []string{"beacon"}, `function render(input){var d=input.data||{},b=d.beacon||{},now=input.now;
 if(!b.instance)return {html:'<div class="eyebrow">BUTTON</div><div class="state warn">ไม่มี Eddystone UID</div><div class="muted">widget นี้ใช้กับ tag ที่ส่ง Eddystone-UID เช่น B10</div>'};
 var counts={};(input.history||[]).forEach(function(h){var i=h.beacon&&h.beacon.instance;if(i)counts[i]=(counts[i]||0)+1;});
 var idle=b.instance,best=0;for(var k in counts){if(counts[k]>best){best=counts[k];idle=k;}}
 var pressed=b.instance!==idle;
 var last=(input.events||[]).filter(function(e){return e.event_type==='button';})[0];
 return {html:'<div class="eyebrow">EMERGENCY BUTTON</div><div class="state '+(pressed?'bad':'ok')+'">'+(pressed?'กำลังกดปุ่ม':'ปกติ')+'</div><div class="row"><div><strong>'+(last?ago(last.occurred_at,now):'ไม่เคย')+'</strong>กดล่าสุด</div>'+(b.voltage?'<div><strong>'+Number(b.voltage).toFixed(2)+' V</strong>แรงดัน (TLM)</div>':'')+meta(d)+'</div><div class="muted">instance <code>'+esc(b.instance)+'</code> · การกดตีความจากการเปลี่ยน instance ยังไม่ยืนยันกับเครื่องจริง</div>'+foot(d,input.source,now)};}`),
		make("official-beacon-v1", "Beacon identity", "iBeacon / Eddystone", []string{"beacon", "motion", "tamper"}, `function render(input){var d=input.data||{},b=d.beacon,now=input.now;
 if(!b)return {html:'<div class="eyebrow">BEACON</div><div class="state warn">ไม่มีเฟรม beacon</div>'};
 var id=b.type==='ibeacon'?'<div><strong>'+b.major+' / '+b.minor+'</strong>major / minor</div>':'<div><strong>…'+esc(String(b.instance||'').slice(-6))+'</strong>instance</div>';
 var full=b.type==='ibeacon'?'UUID '+esc(b.uuid):'namespace '+esc(b.namespace)+' · instance '+esc(b.instance);
 return {html:'<div class="eyebrow">'+(b.type==='ibeacon'?'IBEACON':'EDDYSTONE')+(d.model?' · '+esc(d.model):'')+'</div><div class="state ok">'+(stale(d,now)?'ไม่อยู่ในระยะ':'อยู่ในระยะ')+'</div><div class="row">'+id+meta(d)+(b.tx_power?'<div><strong>'+b.tx_power+' dBm</strong>Tx power</div>':'')+'</div><div class="muted"><code>'+full+'</code></div>'+foot(d,input.source,now)};}`),
		make("official-presence-v1", "Wearable location (multi-gateway)", "B7 · B10 · C10 / presence", []string{"beacon", "motion", "tamper"}, presenceJS+`function render(input){var p=input.presence||{},now=input.now,d=input.data||{};
 var s=(p.sightings||[]).slice(0,5);
 if(!s.length)return {html:'<div class="eyebrow">LOCATION</div><div class="state warn">ยังไม่มี gateway ใดได้ยิน</div>'};
 var cur=p.current;
 var head=cur?'<div class="state zone ok">'+esc(cur.gateway_name)+'</div><div class="muted">'+(cur.project?'โปรเจค '+esc(cur.project)+' · ':'')+'สัญญาณแรงสุด '+(cur.rssi==null?'—':cur.rssi+' dBm')+'</div>':'<div class="state bad">ไม่อยู่ในระยะ</div><div class="muted">ได้ยินล่าสุดที่ '+esc(s[0].gateway_name)+' · '+ago(s[0].last_seen,now)+'</div>';
 return {html:'<div class="eyebrow">LOCATION'+(d.model?' · '+esc(d.model):'')+'</div>'+head+zones(s,now)+(p.roaming?'':'<div class="muted">อุปกรณ์นี้ยังไม่ได้เปิดโหมดใช้หลาย gateway · ค่าที่แสดงมาจาก gateway ที่ลงทะเบียน</div>')+'<div class="muted">RSSI บอกความใกล้โดยประมาณ ไม่ใช่พิกัด</div>'};}`),
		make("official-wearable-v1", "Wearable card", "B7 · B10 · C10 / person", []string{"beacon", "motion"}, presenceJS+`function render(input){var d=input.data||{},m=d.metrics||{},b=d.beacon||{},p=input.presence||{},now=input.now;
 var cur=p.current,out=!cur||stale(d,now);
 var pts=(input.history||[]).map(function(h){return h.metrics&&h.metrics.accel_g;}).filter(function(v){return typeof v==='number';}).slice(-12);
 var active=null;if(pts.length>2){var lo=Math.min.apply(null,pts),hi=Math.max.apply(null,pts);active=hi-lo>0.06;}
 var press=(input.events||[]).filter(function(e){return e.event_type==='button';})[0];
 var recent=press&&Date.parse(now)-Date.parse(press.occurred_at)<120000;
 var tone=recent?'bad':out?'warn':'ok',text=recent?'กดปุ่มขอความช่วยเหลือ':out?'ไม่อยู่ในระยะ':'อยู่ที่ '+esc(cur.gateway_name);
 return {html:'<div class="eyebrow">WEARABLE'+(d.model?' · '+esc(d.model):'')+'</div><div class="state zone '+tone+'">'+text+'</div><div class="row">'+(active==null?'':'<div><strong>'+(active?'ขยับตัว':'นิ่ง')+'</strong>กิจกรรม</div>')+'<div><strong>'+(press?ago(press.occurred_at,now):'ไม่เคย')+'</strong>กดปุ่มล่าสุด</div>'+(b.voltage?'<div><strong>'+Number(b.voltage).toFixed(2)+' V</strong>แรงดัน</div>':'')+meta(d)+'</div>'+zones((p.sightings||[]).slice(0,3),now)+foot(d,input.source,now)};}`),
		make("official-events-v1", "Event timeline", "Any / event log", nil, eventLabelsJS+`function render(input){var ev=(input.events||[]).slice(0,8),now=input.now;
 if(!ev.length)return {html:'<div class="eyebrow">EVENTS</div><div class="state ok">ยังไม่มีเหตุการณ์</div><div class="muted">บันทึกเมื่ออุปกรณ์เปลี่ยนสถานะ เช่น tamper, กดปุ่ม, เคลื่อนไหว, offline</div>'};
 return {html:'<div class="eyebrow">EVENTS · '+esc(ev[0].device_name)+'</div><ul>'+ev.map(function(e){return '<li class="'+(TONE[e.event_type]||'ok')+'"><span>'+esc(LABEL[e.event_type]||e.event_type)+'</span><small>'+ago(e.occurred_at,now)+'</small></li>';}).join('')+'</ul>'};}`),
		make("official-alerts-v1", "Open alerts (workspace)", "Any / alerts", nil, `function render(input){var a=(input.alerts||[]).slice(0,8),now=input.now;
 if(!a.length)return {html:'<div class="eyebrow">ALERTS</div><div class="state ok">ไม่มีเรื่องเปิดอยู่</div><div class="muted">แสดงการแจ้งเตือนที่ยังไม่ปิดของทั้ง workspace</div>'};
 return {html:'<div class="eyebrow">OPEN ALERTS · '+a.length+'</div><ul>'+a.map(function(x){return '<li class="'+(x.severity==='critical'?'bad':x.severity==='warning'?'warn':'ok')+'"><span>'+esc(x.title)+'</span><small>'+ago(x.opened_at,now)+'</small></li>';}).join('')+'</ul>'};}`),
	}
}
