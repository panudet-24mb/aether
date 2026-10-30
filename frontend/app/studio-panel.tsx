"use client";
import {useRef,useState,useEffect,type ReactNode,type PointerEvent} from "react";
type Gesture={kind:"move"|"resize";x:number;y:number;dx:number;dy:number;width:number;height:number;column:number};
/** Rows of the panel grid are this many pixels, so a panel of any height takes the rows it covers; the gap below a
 *  panel is part of its rows (the grid has no row gap). */
export const ROW=4,GAP=16;
export const MIN_HEIGHT=80,MAX_HEIGHT=2000;
/** A panel in a grid of `columns` columns (12 for the page, fewer inside a group): `cols` of the page's 12, and
 *  `height` pixels, or the height of its content when `auto`. A narrow screen's grid has a few columns (--cols) the
 *  panel spans by quarters of the page, as panels did before twelfths. onResize gets the new columns and height, or "keep" when the drag left the height alone. */
export default function StudioPanel({id,cols,columns,height,auto,disabled,children,onResize,onMove}:{id:string;cols:number;columns:number;height:number;auto:boolean;disabled:boolean;children:ReactNode;onResize:(cols:number,height:number|"keep")=>void;onMove:(target:string)=>void}){
 const root=useRef<HTMLElement>(null),gesture=useRef<Gesture|null>(null);
 const [visual,setVisual]=useState<Gesture|null>(null),[shown,setShown]=useState(columns),[natural,setNatural]=useState(height);
 useEffect(()=>{const grid=root.current?.parentElement;if(!grid)return;const observer=new ResizeObserver(()=>setShown(parseInt(getComputedStyle(grid).getPropertyValue("--cols"))||columns));observer.observe(grid);return()=>observer.disconnect();},[]);
 useEffect(()=>{const node=root.current;if(!auto||!node)return;const observer=new ResizeObserver(()=>setNatural(node.offsetHeight));observer.observe(node);return()=>observer.disconnect();},[auto]);
 const span=shown===columns?Math.min(cols,columns):Math.max(1,Math.min(shown,Math.round(cols/3)));
 const toCols=(n:number)=>shown===columns?n:n*3;
 const rows=Math.ceil(((auto?natural:height)+GAP)/ROW);
 function start(e:PointerEvent<HTMLElement>,kind:"move"|"resize"){
  if(disabled||e.button!==0)return;
  if(kind==="move"&&(!(e.target instanceof Element)||!e.target.closest("header")||e.target.closest("input,select,button")))return;
  const node=root.current!,grid=node.parentElement!,rect=node.getBoundingClientRect(),gap=parseFloat(getComputedStyle(grid).columnGap)||GAP;
  const state:Gesture={kind,x:e.clientX,y:e.clientY,dx:0,dy:0,width:rect.width,height:rect.height,column:(grid.getBoundingClientRect().width+gap)/shown};
  gesture.current=state;setVisual(state);node.setPointerCapture(e.pointerId);e.preventDefault();
 }
 function move(e:PointerEvent<HTMLElement>){if(!gesture.current)return;const next={...gesture.current,dx:e.clientX-gesture.current.x,dy:e.clientY-gesture.current.y};gesture.current=next;setVisual(next);}
 function finish(e:PointerEvent<HTMLElement>,cancel=false){const g=gesture.current;if(!g)return;gesture.current=null;setVisual(null);if(root.current?.hasPointerCapture(e.pointerId))root.current.releasePointerCapture(e.pointerId);if(cancel)return;
  if(g.kind==="resize"){const next=toCols(Math.max(1,Math.min(shown,span+Math.round(g.dx/g.column))));onResize(Math.min(next,columns),Math.abs(g.dy)<5?"keep":snap(g.height+g.dy));return;}
  if(Math.hypot(g.dx,g.dy)<8)return;
  const candidates=Array.from(root.current!.parentElement!.children).filter((n):n is HTMLElement=>n instanceof HTMLElement&&n!==root.current);
  const target=candidates.find(n=>{const r=n.getBoundingClientRect();return e.clientX>=r.left&&e.clientX<=r.right&&e.clientY>=r.top&&e.clientY<=r.bottom;});if(target?.dataset.panelId)onMove(target.dataset.panelId);
 }
 return <article ref={root} data-panel-id={id} className={`studio-panel ${auto?"studio-panel-auto":""} ${visual?"studio-manipulating":""}`} style={{gridColumn:`span ${span}`,gridRow:`span ${rows}`,height:visual?.kind==="resize"?Math.max(MIN_HEIGHT,visual.height+visual.dy):auto?undefined:height,transform:visual?.kind==="move"?`translate(${visual.dx}px,${visual.dy}px)`:undefined,width:visual?.kind==="resize"?Math.max(100,visual.width+visual.dx):undefined,zIndex:visual?20:undefined}} onPointerDown={e=>start(e,"move")} onPointerMove={move} onPointerUp={e=>finish(e)} onPointerCancel={e=>finish(e,true)} onLostPointerCapture={e=>finish(e,true)}>
 {children}{!disabled&&<button className="studio-resize-handle" aria-label="ลากมุมเพื่อปรับขนาด widget" title="ลากเพื่อปรับขนาด (ความสูงทีละ 10px) · ปุ่มลูกศรเพื่อปรับด้วยคีย์บอร์ด" onPointerDown={e=>{e.stopPropagation();start(e,"resize");}} onKeyDown={e=>{const keys:Record<string,[number,number]>={ArrowRight:[1,0],ArrowLeft:[-1,0],ArrowUp:[0,-10],ArrowDown:[0,10]};const d=keys[e.key];if(d){e.preventDefault();onResize(Math.min(columns,toCols(Math.max(1,Math.min(shown,span+d[0])))),d[1]?snap((auto?natural:height)+d[1]):"keep");}}}/>}
 </article>;
}
/** A dragged height, to 10px and within bounds. */
const snap=(px:number)=>Math.max(MIN_HEIGHT,Math.min(MAX_HEIGHT,Math.round(px/10)*10));
