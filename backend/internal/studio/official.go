package studio

import "encoding/json"

const MinewDecoder = `function decodeUplink(input) {
 const b=input.bytes;
 for(let i=0;i<b.length;) {
  const n=b[i]; if(!n) break; if(i+n>=b.length) throw Error("Truncated advertisement");
  const a=b.slice(i+1,i+n+1); i+=n+1;
  if(a.length===16 && a[0]===22 && a[1]===225 && a[2]===255 && a[3]===161 && a[4]===1){
   const signed=(h,l)=>{let v=h*256+l;return (v>=32768?v-65536:v)/256};
   const temperature=signed(a[6],a[7]),humidity=signed(a[8],a[9]);
   if(a[5]>100||humidity<0||humidity>100||temperature< -100||temperature>125) throw Error("Invalid measurement");
   return {data:{temperature,humidity,battery:a[5]}};
  }
 }
 throw Error("Unsupported FFE1 frame");
}`

func Official() []Item {
	makeItem := func(id, kind, name string, d Definition) Item {
		if kind == "widget" {
			d.DecodeCode = MinewDecoder
			d.Kinds = []string{"environment"}
		}
		b, _ := json.Marshal(d)
		return Item{ID: id, Kind: kind, Name: name, Brand: "Minew", Model: "S1 / FFE1 A1-01", Version: 1, Visibility: "community", Official: true, Definition: b, Revision: 1}
	}
	return append(kitWidgets(), []Item{
		makeItem("official-minew-ffe1-v1", "decoder", "FFE1 environmental decoder", Definition{Code: MinewDecoder}),
		makeItem("official-environment-v1", "widget", "Environment / dual metric", Definition{HTML: `<div class="eyebrow">ENVIRONMENT</div><div class="metrics"><div><strong>{{temperature}}</strong><span>°C · Temperature</span></div><div><strong>{{humidity}}</strong><span>% · Humidity</span></div></div><footer>Battery {{battery}}% · {{source}}</footer>`, CSS: `body{margin:0;padding:22px;background:#111a20;color:#eef9f4;font-family:system-ui}.eyebrow{font-size:11px;letter-spacing:3px;color:#9dafb2}.metrics{display:flex;gap:32px;margin:24px 0}strong{display:block;font-size:46px;font-weight:500;color:#a7f3d0}span,footer{font-size:12px;color:#a5b5bd}`, Code: `function render(input){const d=input.data;return {temperature:Number(d.temperature).toFixed(1),humidity:Number(d.humidity).toFixed(1),battery:d.battery,source:input.source};}`}),
		makeItem("official-temperature-v1", "widget", "Temperature / history", Definition{HTML: `<h3>Temperature trend</h3>{{chart}}`, CSS: `body{margin:0;padding:20px;background:#10191f;color:#b5f9da;font-family:system-ui}h3{font-size:13px;font-weight:400;color:#9aacb4}svg{width:100%;height:140px}strong{font-size:44px;font-weight:500}`, Code: `function render(input){const values=input.history.map(x=>Number(x.temperature)).filter(Number.isFinite);const lo=Math.min(...values)-1,hi=Math.max(...values)+1;const points=values.map((v,i)=>(i/Math.max(1,values.length-1)*600)+','+(100-(v-lo)/(hi-lo)*90)).join(' ');return {html:'<h3>Temperature · '+(input.source==='simulated'?'SIMULATED':'DEVICE')+'</h3><strong>'+Number(input.data.temperature).toFixed(1)+' °C</strong><svg viewBox="0 0 600 110"><polyline points="'+points+'" fill="none" stroke="#a7f3d0" stroke-width="2"/></svg>'};}`}),
	}...)
}
