// The twin's immersive style (docs/platform/digital-twin.md, "Immersive view"): image-based lighting, a dark
// reflective floor with a holographic grid in the accent colour, glass with glowing edges and devices as status
// beacons. Every colour comes from the app's tokens (theme.ts). The classic style never calls into this file.

import * as THREE from "three";
import { RoomEnvironment } from "three/examples/jsm/environments/RoomEnvironment.js";
import type { TwinTheme } from "../theme";

/** A room-light environment map (PMREM): the source scene is disposed as soon as it is baked; the caller keeps the
 *  render target and disposes it with the engine. */
export function makeEnvironment(renderer: THREE.WebGLRenderer): THREE.WebGLRenderTarget {
  const pmrem = new THREE.PMREMGenerator(renderer);
  const room = new RoomEnvironment();
  const rt = pmrem.fromScene(room, 0.04);
  room.traverse((o) => {
    const m = o as THREE.Mesh;
    m.geometry?.dispose();
    for (const mat of Array.isArray(m.material) ? m.material : m.material ? [m.material] : []) mat.dispose();
  });
  pmrem.dispose();
  return rt;
}

const GRID_VERTEX = /* glsl */ `
varying vec3 vWorld;
void main() {
  vec4 w = modelMatrix * vec4(position, 1.0);
  vWorld = w.xyz;
  gl_Position = projectionMatrix * viewMatrix * w;
}`;

const GRID_FRAGMENT = /* glsl */ `
uniform vec3 uColor;
uniform vec3 uCenter;
uniform float uRadius;
uniform float uTime;
uniform float uScan;
varying vec3 vWorld;
float line(vec2 p, float cell, float width) {
  vec2 q = p / cell;
  vec2 g = abs(fract(q - 0.5) - 0.5) / max(fwidth(q), vec2(1e-4));
  return 1.0 - min(min(g.x, g.y) / width, 1.0);
}
void main() {
  vec2 p = vWorld.xz - uCenter.xz;
  float d = length(p);
  float fade = 1.0 - smoothstep(uRadius * 0.22, uRadius, d);
  float minor = line(p, 1.0, 1.0) * 0.16;
  float major = line(p, 5.0, 1.3) * 0.42;
  float scan = 0.0;
  if (uScan > 0.0) {
    float r = mod(uTime * 5.0, uRadius * 1.1);
    scan = smoothstep(3.0, 0.0, abs(d - r)) * 0.28 * uScan * (1.0 - r / (uRadius * 1.1));
  }
  float a = (max(minor, major) + scan) * fade;
  gl_FragColor = vec4(uColor * a, a);
  #include <colorspace_fragment>
}`;

export type Ground = { group: THREE.Group; uniforms: { uTime: { value: number }; uScan: { value: number } }; dispose: () => void };

/**
 * The stage under the building: a dark, slightly metallic disc (it picks up the environment as a soft sheen) and
 * an additive grid that fades with distance. `scan` (ambient motion) sweeps a faint ring outwards.
 */
export function makeGround(theme: TwinTheme, centre: THREE.Vector3, radius: number, y: number): Ground {
  const group = new THREE.Group();
  group.name = "immersive-ground";
  const floorColor = new THREE.Color(theme.paper).lerp(new THREE.Color(0x000000), 0.45);
  // The disc fades out towards its rim, so the floor melts into the stage instead of ending in a hard horizon.
  const fade = document.createElement("canvas");
  fade.width = fade.height = 256;
  const ctx = fade.getContext("2d")!;
  const grad = ctx.createRadialGradient(128, 128, 0, 128, 128, 128);
  grad.addColorStop(0, "#fff");
  grad.addColorStop(0.45, "#fff");
  grad.addColorStop(1, "#000");
  ctx.fillStyle = grad;
  ctx.fillRect(0, 0, 256, 256);
  const alphaMap = new THREE.CanvasTexture(fade);
  const disc = new THREE.Mesh(
    new THREE.CircleGeometry(radius, 96),
    new THREE.MeshStandardMaterial({ color: floorColor, metalness: 0.55, roughness: 0.5, envMapIntensity: 0.3, alphaMap, transparent: true, depthWrite: false }),
  );
  disc.rotation.x = -Math.PI / 2;
  disc.position.set(centre.x, y - 0.02, centre.z);
  disc.renderOrder = -2;
  group.add(disc);
  const uniforms = {
    uColor: { value: new THREE.Color(theme.accent) },
    uCenter: { value: centre.clone() },
    uRadius: { value: radius },
    uTime: { value: 0 },
    uScan: { value: 0 },
  };
  const grid = new THREE.Mesh(
    new THREE.PlaneGeometry(radius * 2, radius * 2),
    new THREE.ShaderMaterial({ uniforms, vertexShader: GRID_VERTEX, fragmentShader: GRID_FRAGMENT, transparent: true, depthWrite: false, blending: THREE.AdditiveBlending, toneMapped: false }),
  );
  grid.rotation.x = -Math.PI / 2;
  grid.position.set(centre.x, y, centre.z);
  grid.renderOrder = -1;
  group.add(grid);
  // disposeTree releases geometries, materials and colour maps; the alpha map is released here.
  return { group, uniforms, dispose: () => alphaMap.dispose() };
}

/** Glass walls with a cool sheen and accent edges bright enough to bloom; slabs a touch of metal. */
export function styleBuilding(building: THREE.Group, theme: TwinTheme) {
  const accent = new THREE.Color(theme.accent);
  // Frosted rather than clear: wall fixtures (switches, sockets, units) need a wall to sit on.
  const glassTint = new THREE.Color(theme.blue).lerp(new THREE.Color(theme.ink), 0.62);
  building.traverse((o) => {
    const mat = (o as THREE.Mesh).material as THREE.Material | THREE.Material[] | undefined;
    for (const m of Array.isArray(mat) ? mat : mat ? [mat] : []) {
      if ((m as THREE.MeshPhysicalMaterial).isMeshPhysicalMaterial) {
        const g = m as THREE.MeshPhysicalMaterial;
        g.color.copy(glassTint);
        g.metalness = 0.15;
        g.envMapIntensity = 1.1;
        g.clearcoat = 0.6;
        g.opacity = Math.min(0.3, g.opacity * 1.25);
        g.roughness = 0.35;
        g.needsUpdate = true;
      } else if ((m as THREE.LineBasicMaterial).isLineBasicMaterial) {
        const l = m as THREE.LineBasicMaterial;
        // Accent edges on the active floor glow; dimmed floors keep their dim edges.
        if (l.opacity > 0.2) { l.color.copy(accent); l.opacity = Math.min(0.95, l.opacity * 1.7); }
        l.toneMapped = false;
      } else if ((m as THREE.MeshStandardMaterial).isMeshStandardMaterial) {
        const s = m as THREE.MeshStandardMaterial;
        s.metalness = Math.max(s.metalness, 0.25);
        s.roughness = Math.min(s.roughness, 0.7);
        s.envMapIntensity = 0.6;
      }
    }
  });
}

/** Status colours of a beacon (tokens): online accent, stale or door open amber, alert coral, offline dim grey. */
export type BeaconTone = "ok" | "idle" | "warn" | "alert" | "off";

export function beaconColor(theme: TwinTheme, tone: BeaconTone, out = new THREE.Color()): THREE.Color {
  switch (tone) {
    case "alert": return out.setHex(theme.danger).multiplyScalar(1.25);
    case "warn": return out.setHex(theme.warning).multiplyScalar(1.15);
    case "off": return out.setHex(theme.dim).multiplyScalar(0.55);
    case "idle": return out.setHex(theme.accent).multiplyScalar(0.42);
    default: return out.setHex(theme.accent).multiplyScalar(1.2);
  }
}

/** A hexagonal hub tower for gateways, standing on its own base (origin at the bottom). */
export function towerGeometry(): THREE.BufferGeometry {
  const g = new THREE.CylinderGeometry(0.14, 0.26, 0.9, 6, 1);
  g.translate(0, 0.45, 0);
  const cap = new THREE.OctahedronGeometry(0.17, 0);
  cap.translate(0, 1.05, 0);
  const merged = new THREE.BufferGeometry();
  // Two simple parts merged by hand (same attributes: position, normal, uv).
  const parts = [g, cap].map((p) => (p.index ? p.toNonIndexed() : p.clone()));
  for (const name of ["position", "normal", "uv"] as const) {
    const arrays = parts.map((p) => p.getAttribute(name).array as Float32Array);
    const size = parts[0].getAttribute(name).itemSize;
    const all = new Float32Array(arrays.reduce((n, a) => n + a.length, 0));
    let at = 0;
    for (const a of arrays) { all.set(a, at); at += a.length; }
    merged.setAttribute(name, new THREE.BufferAttribute(all, size));
  }
  g.dispose(); cap.dispose(); parts.forEach((p) => p.dispose());
  return merged;
}
