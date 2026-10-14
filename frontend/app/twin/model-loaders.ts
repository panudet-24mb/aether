// three.js loaders for building models, imported only when a model is shown or imported (the twin's first paint
// does not pay for them). Draco and meshopt decoders are served from this app's own assets, never a CDN: three's
// DRACO_GLTF_CONFIG points at its bundled decoder through `new URL(…, import.meta.url)`, which the build emits.

import type * as THREE from "three";
import type { GLTFLoader } from "three/examples/jsm/loaders/GLTFLoader.js";

/** A GLTFLoader that also reads Draco- and meshopt-compressed files. */
export async function gltfLoader(manager?: THREE.LoadingManager): Promise<GLTFLoader> {
  const [{ GLTFLoader }, { DRACOLoader, DRACO_GLTF_CONFIG }, { MeshoptDecoder }] = await Promise.all([
    import("three/examples/jsm/loaders/GLTFLoader.js"),
    import("three/examples/jsm/loaders/DRACOLoader.js"),
    import("three/examples/jsm/libs/meshopt_decoder.module.js"),
  ]);
  const loader = new GLTFLoader(manager);
  const draco = new DRACOLoader();
  draco.setDecoderPath(DRACO_GLTF_CONFIG);
  draco.setWorkerLimit(2);
  loader.setDRACOLoader(draco);
  loader.setMeshoptDecoder(MeshoptDecoder);
  return loader;
}

/** Releases the Draco worker pool a loader made. */
export function releaseLoader(loader: GLTFLoader) {
  (loader as unknown as { dracoLoader?: { dispose(): void } | null }).dracoLoader?.dispose();
}
