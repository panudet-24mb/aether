"use client";
import { useState } from "react";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
type Template = { id: string; name: string; version: number; definition: { temperature_high: number | null; humidity_high: number | null } };
const API = import.meta.env.VITE_AETHER_API_ORIGIN ?? "";
export default function TemplateSettings({ gateway, external, name, currentTemplate, getToken }: { gateway: string; external: string; name: string; currentTemplate?: string | null; getToken: () => string }) {
 const [open,setOpen]=useState(false),[items,setItems]=useState<Template[]>([]),[selected,setSelected]=useState("");
 const [deviceName,setDeviceName]=useState(name),[templateName,setTemplateName]=useState(""),[version,setVersion]=useState("1");
 const [temperature,setTemperature]=useState("30"),[humidity,setHumidity]=useState("75"),[busy,setBusy]=useState(false),[message,setMessage]=useState("");
 async function request(path:string,data?:object) {
  const r=await fetch(`${API}/api/v1${path}`,{method:data ? "POST":"GET",headers:{Authorization:`Bearer ${getToken()}`,"Content-Type":"application/json"},body:data ? JSON.stringify(data):undefined,signal:AbortSignal.timeout(10000)});
  if(!r.ok)throw new Error(r.status===409?"ชื่อและ version นี้มีแล้ว หรือเกินจำนวน template ที่อนุญาต":r.status===401?"เซสชันหมดอายุ ลองอีกครั้งหลังหน้า Live เชื่อมต่อใหม่":"บันทึกไม่สำเร็จ ตรวจข้อมูลและสิทธิ์ของบัญชี");
  return r;
 }
 async function show() {
  setOpen(true);setDeviceName(name);setMessage("");setBusy(true);
  try{const r=await request("/templates");const data=await r.json() as {items:Template[]};setItems(data.items);setSelected(currentTemplate ?? data.items[0]?.id ?? "");}catch(e){setMessage(e instanceof Error?e.message:"โหลดไม่ได้");}finally{setBusy(false);}
 }
 async function create(e:React.FormEvent) {
  e.preventDefault();setBusy(true);setMessage("");
  try{const r=await request("/templates",{name:templateName,version:Number(version),decoder_id:"minew-ffe1-a101@1",definition:{temperature_high:temperature===""?null:Number(temperature),humidity_high:humidity===""?null:Number(humidity)}});const t=await r.json() as Template;setItems(v=>[t,...v]);setSelected(t.id);setMessage("สร้าง version แล้ว เลือกใช้กับอุปกรณ์ด้านบนเพื่อบันทึก");}catch(e){setMessage(e instanceof Error?e.message:"บันทึกไม่ได้");}finally{setBusy(false);}
 }
 async function assign() {
  setBusy(true);setMessage("");
  try{await request(`/gateways/${gateway}/streams/${external}/template`,{name:deviceName,template_id:selected});setOpen(false);}catch(e){setMessage(e instanceof Error?e.message:"บันทึกไม่ได้");}finally{setBusy(false);}
 }
 return <><Button variant="outline" onClick={show}>ตั้งชื่อ / เกณฑ์อุปกรณ์</Button><Dialog open={open} onOpenChange={v=>{if(!busy)setOpen(v);}}><DialogContent className="template-dialog"><DialogHeader><DialogTitle>ตั้งค่าอุปกรณ์และ เกณฑ์อุปกรณ์</DialogTitle><DialogDescription>เกณฑ์อุปกรณ์ แต่ละ version ใช้ซ้ำกับหลายอุปกรณ์ได้ โดยประวัติเดิมคง version ที่ใช้ตอนรับข้อมูล</DialogDescription></DialogHeader>
 <label htmlFor="stream-name">ชื่ออุปกรณ์</label><Input id="stream-name" value={deviceName} maxLength={128} onChange={e=>setDeviceName(e.target.value)} />
 <label>เกณฑ์อุปกรณ์ ที่ใช้กับอุปกรณ์</label><Select value={selected} onValueChange={setSelected}><SelectTrigger aria-label="เลือกเกณฑ์"><SelectValue placeholder="สร้าง เกณฑ์ก่อน" /></SelectTrigger><SelectContent>{items.map(t=><SelectItem key={t.id} value={t.id}>{t.name} · v{t.version}</SelectItem>)}</SelectContent></Select>
 <Button disabled={busy||!selected||!deviceName.trim()} onClick={assign}>บันทึกชื่อและเลือกใช้ เกณฑ์อุปกรณ์</Button>
 <form onSubmit={create}><h3>สร้าง เกณฑ์อุปกรณ์ version ใหม่</h3><p className="live-note">รองรับ Minew อุณหภูมิ / ความชื้น · เกณฑ์ด้านล่างใช้แสดงสถานะบนหน้า Live</p><label htmlFor="template-name">ชื่อ เกณฑ์อุปกรณ์</label><Input id="template-name" required maxLength={128} value={templateName} onChange={e=>setTemplateName(e.target.value)} /><label htmlFor="template-version">Version</label><Input id="template-version" type="number" required min={1} max={100000} value={version} onChange={e=>setVersion(e.target.value)} /><div className="template-fields"><div><label htmlFor="temp-high">อุณหภูมิสูงกว่า (°C)</label><Input id="temp-high" type="number" step="any" min={-100} max={125} value={temperature} onChange={e=>setTemperature(e.target.value)} /></div><div><label htmlFor="humidity-high">ความชื้นสูงกว่า (%RH)</label><Input id="humidity-high" type="number" step="any" min={0} max={100} value={humidity} onChange={e=>setHumidity(e.target.value)} /></div></div><p className="live-note">เว้นว่างเพื่อไม่กำหนดเกณฑ์</p><Button type="submit" variant="outline" disabled={busy||!templateName.trim()}>สร้าง version</Button></form>{message&&<p role="status" className="live-note">{message}</p>}
 </DialogContent></Dialog></>;
}
