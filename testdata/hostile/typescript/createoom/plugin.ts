// A hostile TypeScript plugin whose Create runs out of memory.
export function Create() {
  const hoard: number[][] = [];
  for (;;) {
    hoard.push(new Array(1 << 16).fill(1));
  }
}
