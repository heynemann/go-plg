// A hostile TypeScript plugin whose Create runs out of memory, 1 MiB at a time (see the hostile plugin's oom).
export function Create() {
  const hoard: ArrayBuffer[] = [];
  for (;;) {
    hoard.push(new ArrayBuffer(1 << 20));
  }
}
