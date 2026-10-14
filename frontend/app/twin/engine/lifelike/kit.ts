// A tiny instanced "parts kit": furniture and fixtures are made of a few primitives (box, cylinder, ball, glowing
// box), so a whole furnished floor is one instanced mesh per primitive, a handful of draw calls whatever the count.

import * as THREE from "three";

export type Shape = "box" | "cyl" | "ball" | "glow";

type Part = { m: THREE.Matrix4; c: THREE.Color };

const UP = new THREE.Vector3(0, 1, 0);

/** A local frame: an item's origin on the floor and its heading; parts are given in the item's own metres. */
export class Frame {
  private q: THREE.Quaternion;
  constructor(private kit: Kit, private origin: THREE.Vector3, rotY: number) {
    this.q = new THREE.Quaternion().setFromAxisAngle(UP, rotY);
  }

  /** A part centred at local (x, y, z) (y up from the floor), size w × h × d, optional extra tilt about local x. */
  add(shape: Shape, x: number, y: number, z: number, w: number, h: number, d: number, color: number | THREE.Color, tiltX = 0) {
    const pos = new THREE.Vector3(x, y, z).applyQuaternion(this.q).add(this.origin);
    const q = tiltX ? this.q.clone().multiply(new THREE.Quaternion().setFromAxisAngle(new THREE.Vector3(1, 0, 0), tiltX)) : this.q;
    this.kit.push(shape, new THREE.Matrix4().compose(pos, q, new THREE.Vector3(w, h, d)), color);
  }
}

export class Kit {
  private parts: Record<Shape, Part[]> = { box: [], cyl: [], ball: [], glow: [] };
  triangles = 0;

  frame(origin: THREE.Vector3, rotY: number) { return new Frame(this, origin, rotY); }

  push(shape: Shape, m: THREE.Matrix4, color: number | THREE.Color) {
    this.parts[shape].push({ m, c: color instanceof THREE.Color ? color.clone() : new THREE.Color(color) });
  }

  get count() { return this.parts.box.length + this.parts.cyl.length + this.parts.ball.length + this.parts.glow.length; }

  /** One instanced mesh per primitive that has parts. `low` uses fewer segments. */
  build(low: boolean, name: string): THREE.Group {
    const group = new THREE.Group();
    group.name = name;
    const geos: Record<Shape, () => THREE.BufferGeometry> = {
      box: () => new THREE.BoxGeometry(1, 1, 1),
      glow: () => new THREE.BoxGeometry(1, 1, 1),
      cyl: () => new THREE.CylinderGeometry(0.5, 0.5, 1, low ? 6 : 12, 1),
      ball: () => new THREE.IcosahedronGeometry(0.5, low ? 0 : 1),
    };
    this.triangles = 0;
    for (const shape of Object.keys(this.parts) as Shape[]) {
      const list = this.parts[shape];
      if (!list.length) continue;
      const geo = geos[shape]();
      const mat = shape === "glow"
        ? new THREE.MeshBasicMaterial({ color: 0xffffff, toneMapped: false })
        : new THREE.MeshStandardMaterial({ color: 0xffffff, roughness: 0.72, metalness: 0.08 });
      const mesh = new THREE.InstancedMesh(geo, mat, list.length);
      list.forEach((p, i) => { mesh.setMatrixAt(i, p.m); mesh.setColorAt(i, p.c); });
      mesh.instanceMatrix.needsUpdate = true;
      if (mesh.instanceColor) mesh.instanceColor.needsUpdate = true;
      mesh.computeBoundingSphere();
      mesh.name = `${name}:${shape}`;
      group.add(mesh);
      const idx = geo.index ? geo.index.count : geo.getAttribute("position").count;
      this.triangles += (idx / 3) * list.length;
    }
    return group;
  }
}
