import { useEffect, useRef, type RefObject } from "react";

/** Keeps the latest value in a ref without writing to it during render (parents pass fresh callbacks every render). */
export function useLatest<T>(value: T): RefObject<T> {
  const ref = useRef(value);
  useEffect(() => {
    ref.current = value;
  });
  return ref;
}
