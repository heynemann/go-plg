import config from "../config.json";

export function fahrenheit(celsius: number): number {
  return (celsius * 9) / 5 + config.freezing;
}
