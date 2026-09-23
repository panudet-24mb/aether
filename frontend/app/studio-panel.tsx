"use client";
import {useRef,useState,useEffect,type ReactNode,type PointerEvent} from "react";
type Gesture={kind:"move"|"resize";x:number;y:number;dx:number;dy:number;width:number;height:number;column:number;row:number};
export default function StudioPanel({id,width,height,disabled,children,onResize,onMove}:{id:string;width:number;height:number;disabled:boolean;children:ReactNode;onResize:(width:number,height:number)=>void;onMove:(target:string)=>void}){
 const root=useRef<HTMLElement>(null),gesture=useRef<Gesture|null>(null);
 const [visual,setVisual]=useState<Gesture|null>(null),[columns,setColumns]=useState(4);
 useEffect(()=>{const grid=root.current?.parentElement;if(!grid)return;const observer=new ResizeObserver(()=>setColumns(getComputedStyle(grid).gridTemplateColumns.split(" ").length));observer.observe(grid);return()=>observer.disconnect();},[]);
 function start(e:PointerEvent<HTMLElement>,kind:"move"|"resize"){
  if(disabled||e.button!==0)return;
  if(kind==="move"&&(!(e.target instanceof Element)||!e.target.closest("header")||e.target.closest("input,select,button")))return;
  const node=root.current!,grid=node.parentElement!,rect=node.getBoundingClientRect(),style=getComputedStyle(grid),gap=parseFloat(style.columnGap)||16;
  const state:Gesture={kind,x:e.clientX,y:e.clientY,dx:0,dy:0,width:rect.width,height:rect.height,column:(grid.getBoundingClientRect().width+gap)/columns,row:parseFloat(style.gridAutoRows)+(parseFloat(style.rowGap)||16)};
  gesture.current=state;setVisual(state);node.setPointerCapture(e.pointerId);e.preventDefault();
 }
 function move(e:PointerEvent<HTMLElement>){if(!gesture.current)return;const next={...gesture.current,dx:e.clientX-gesture.current.x,dy:e.clientY-gesture.current.y};gesture.current=next;setVisual(next);}
 function finish(e:PointerEvent<HTMLElement>,cancel=false){const g=gesture.current;if(!g)return;gesture.current=null;setVisual(null);if(root.current?.hasPointerCapture(e.pointerId))root.current.releasePointerCapture(e.pointerId);if(cancel)return;
  if(g.kind==="resize"){onResize(Math.max(1,Math.min(columns,Math.min(width,columns)+Math.round(g.dx/g.column))),Math.max(1,Math.min(4,height+Math.round(g.dy/g.row))));return;}
  if(Math.hypot(g.dx,g.dy)<8)return;
  const candidates=Array.from(root.current!.parentElement!.children).filter((n):n is HTMLElement=>n instanceof HTMLElement&&n!==root.current);
  const target=candidates.find(n=>{const r=n.getBoundingClientRect();return e.clientX>=r.left&&e.clientX<=r.right&&e.clientY>=r.top&&e.clientY<=r.bottom;});if(target?.dataset.panelId)onMove(target.dataset.panelId);
 }
 return <article ref={root} data-panel-id={id} className={`studio-panel ${visual?"studio-manipulating":""}`} style={{gridColumn:`span ${Math.min(width,columns)}`,gridRow:`span ${height}`,transform:visual?.kind==="move"?`translate(${visual.dx}px,${visual.dy}px)`:undefined,width:visual?.kind==="resize"?Math.max(100,visual.width+visual.dx):undefined,height:visual?.kind==="resize"?Math.max(140,visual.height+visual.dy):undefined,zIndex:visual?20:undefined}} onPointerDown={e=>start(e,"move")} onPointerMove={move} onPointerUp={e=>finish(e)} onPointerCancel={e=>finish(e,true)} onLostPointerCapture={e=>finish(e,true)}>
 {children}{!disabled&&<button className="studio-resize-handle" aria-label="ลากมุมเพื่อปรับขนาด widget" title="ลากเพื่อปรับขนาด · ปุ่มลูกศรเพื่อปรับด้วยคีย์บอร์ด" onPointerDown={e=>{e.stopPropagation();start(e,"resize");}} onKeyDown={e=>{const keys:Record<string,[number,number]>={ArrowRight:[1,0],ArrowLeft:[-1,0],ArrowUp:[0,-1],ArrowDown:[0,1]};const d=keys[e.key];if(d){e.preventDefault();onResize(Math.max(1,Math.min(columns,width+d[0])),Math.max(1,Math.min(4,height+d[1])));}}}/>}
 </article>;
}
